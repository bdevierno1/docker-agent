package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/desktop"
)

func TestNew_UsesDesktopProxyWhenAvailable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Create a transport
	rt := New(ctx)
	require.NotNil(t, rt)

	// If Docker Desktop is running, verify fallback transport is used
	if desktop.IsDockerDesktopRunning(ctx) {
		_, ok := rt.(*fallbackTransport)
		assert.True(t, ok, "transport should be *fallbackTransport when Docker Desktop is running")
	} else {
		// Otherwise, it should be a plain *http.Transport
		_, ok := rt.(*http.Transport)
		assert.True(t, ok, "transport should be *http.Transport when Docker Desktop is not running")
	}
}

func TestNew_PreservesWrappedDefaultTransport(t *testing.T) {
	// Intentionally not parallel: mutates the http.DefaultTransport global.
	previous := http.DefaultTransport
	wrapped := &countingRoundTripper{}
	http.DefaultTransport = wrapped
	t.Cleanup(func() { http.DefaultTransport = previous })

	for _, test := range []struct {
		name    string
		running bool
	}{
		{name: "without Desktop", running: false},
		{name: "with Desktop", running: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			SetDesktopRunningForTest(t, func(context.Context) (bool, error) {
				return test.running, nil
			})

			rt := New(t.Context())
			assert.Same(t, wrapped, rt)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", http.NoBody)
			require.NoError(t, err)
			resp, err := (&http.Client{Transport: rt}).Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		})
	}

	assert.Equal(t, int32(2), wrapped.calls.Load())
}

func TestNew_WorksWithoutDesktopProxy(t *testing.T) {
	t.Parallel()

	// Create a test server to simulate a registry
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := t.Context()

	// Create a transport (should work whether Desktop is running or not)
	rt := New(ctx)
	require.NotNil(t, rt)

	// Make a simple HTTP request to verify the transport works
	client := &http.Client{Transport: rt}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestIsProxySocketError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		errStr   string
		expected bool
	}{
		{
			name:     "no such file or directory",
			errStr:   "proxyconnect tcp: dial unix /path/to/httpproxy.sock: connect: no such file or directory",
			expected: true,
		},
		{
			name:     "connection refused",
			errStr:   "proxyconnect tcp: dial unix /path/to/httpproxy.sock: connect: connection refused",
			expected: true,
		},
		{
			name:     "proxyconnect tcp error",
			errStr:   "Post https://api.anthropic.com/v1/messages: proxyconnect tcp: some error",
			expected: true,
		},
		{
			name:     "dial unix error",
			errStr:   "dial unix /var/run/docker.sock: operation timed out",
			expected: true,
		},
		{
			name:     "regular network error",
			errStr:   "dial tcp 192.168.1.1:443: i/o timeout",
			expected: false,
		},
		{
			name:     "HTTP error",
			errStr:   "HTTP 500: internal server error",
			expected: false,
		},
		{
			name:     "nil error",
			errStr:   "",
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.errStr != "" {
				err = &testError{msg: tc.errStr}
			}
			result := isProxySocketError(err)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestFallbackTransport_DisableCompression(t *testing.T) {
	t.Parallel()

	proxy := &http.Transport{}
	direct := &http.Transport{}

	ft := newFallbackTransport(proxy, direct)

	// Verify compression is not disabled initially
	assert.False(t, proxy.DisableCompression)
	assert.False(t, direct.DisableCompression)

	// Disable compression
	ft.DisableCompression()

	// Verify compression is now disabled on both transports
	assert.True(t, proxy.DisableCompression)
	assert.True(t, direct.DisableCompression)
}

// testError is a simple error type for testing
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

type countingRoundTripper struct {
	calls atomic.Int32
	err   error
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// fakeFallback drives the fallbackTransport state machine against RoundTripper
// fakes so tests don't need a real Unix socket.
type fakeFallback struct {
	*fallbackTransport

	fakeProxy  *countingRoundTripper
	fakeDirect *countingRoundTripper
}

func (f *fakeFallback) RoundTrip(req *http.Request) (*http.Response, error) {
	if !f.proxyEnabled() {
		return f.fakeDirect.RoundTrip(req)
	}
	resp, err := f.fakeProxy.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if isProxySocketError(err) {
		f.disableProxy()
		return f.fakeDirect.RoundTrip(req)
	}
	return nil, err
}

func newFakeFallback(proxyErr, directErr error) *fakeFallback {
	return &fakeFallback{
		fallbackTransport: newFallbackTransport(&http.Transport{}, &http.Transport{}),
		fakeProxy:         &countingRoundTripper{err: proxyErr},
		fakeDirect:        &countingRoundTripper{err: directErr},
	}
}

func TestFallbackTransport_ProxyRecoversAfterCooldown(t *testing.T) {
	t.Parallel()

	proxySocketErr := &testError{msg: "proxyconnect tcp: dial unix /path/to/httpproxy.sock: connect: no such file or directory"}
	ft := newFakeFallback(proxySocketErr, nil)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/", http.NoBody)
	require.NoError(t, err)

	// 1st request: proxy fails → direct.
	resp, err := ft.RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, int32(1), ft.fakeProxy.calls.Load())
	require.Equal(t, int32(1), ft.fakeDirect.calls.Load())

	// 2nd request: still on cooldown, proxy is skipped.
	resp, err = ft.RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, int32(1), ft.fakeProxy.calls.Load(), "proxy should be skipped during cooldown")
	require.Equal(t, int32(2), ft.fakeDirect.calls.Load())

	// Push the deadline into the past to exercise the CAS clear branch in
	// proxyEnabled (Store(0) would short-circuit it).
	ft.disabledUntilUnixNano.Store(time.Now().Add(-time.Second).UnixNano())
	ft.fakeProxy = &countingRoundTripper{}

	// 3rd request: cooldown elapsed, proxy re-probed and succeeds.
	resp, err = ft.RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, int32(1), ft.fakeProxy.calls.Load(), "healthy proxy should be tried again once cooldown expires")
	assert.Equal(t, int32(2), ft.fakeDirect.calls.Load(), "direct should not be hit once proxy recovers")
}

// Non-socket errors (e.g. upstream timeouts) must not trip the cooldown,
// otherwise a transient upstream problem would look like a proxy outage.
func TestFallbackTransport_NonSocketErrorDoesNotDisableProxy(t *testing.T) {
	t.Parallel()

	upstreamErr := &testError{msg: "read tcp 10.0.0.1:443: i/o timeout"}
	ft := newFakeFallback(upstreamErr, nil)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/", http.NoBody)
	require.NoError(t, err)

	resp, err := ft.RoundTrip(req) //nolint:bodyclose // resp is nil on error, checked below
	require.Error(t, err)
	require.Nil(t, resp)
	assert.True(t, errors.Is(err, upstreamErr) || err.Error() == upstreamErr.Error())
	assert.True(t, ft.proxyEnabled(), "proxy must stay enabled after a non-socket error")
	assert.Equal(t, int32(0), ft.fakeDirect.calls.Load(), "direct must not be tried on non-socket errors")
}

func TestFallbackTransport_ProxyEnabledAfterCooldownExpires(t *testing.T) {
	t.Parallel()

	ft := newFallbackTransport(&http.Transport{}, &http.Transport{})

	require.True(t, ft.proxyEnabled())

	ft.disabledUntilUnixNano.Store(time.Now().Add(-time.Second).UnixNano())
	require.True(t, ft.proxyEnabled(), "expired cooldown should re-enable the proxy")
	require.Equal(t, int64(0), ft.disabledUntilUnixNano.Load(), "expired cooldown should be cleared")

	future := time.Now().Add(time.Hour).UnixNano()
	ft.disabledUntilUnixNano.Store(future)
	require.False(t, ft.proxyEnabled())
	require.Equal(t, future, ft.disabledUntilUnixNano.Load())
}

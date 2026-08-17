package httpclient

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	desktoptransport "github.com/docker/docker-agent/pkg/desktop/transport"
)

func TestDesktopAwareTransportProxySafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		host     string
		resolver func(context.Context, string) ([]net.IP, error)
		want     bool
	}{
		{
			name: "literal private address",
			host: "127.0.0.1",
			want: false,
		},
		{
			name: "all resolved addresses private",
			host: "internal.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("192.168.1.1")}, nil
			},
			want: false,
		},
		{
			name: "mixed public and private addresses stay direct",
			host: "mixed.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("1.1.1.1")}, nil
			},
			want: false,
		},
		{
			name: "all resolved addresses public",
			host: "public.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")}, nil
			},
			want: true,
		},
		{
			name: "empty DNS response stays direct",
			host: "empty.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return nil, nil
			},
			want: false,
		},
		{
			name: "unresolvable host delegates to proxy",
			host: "proxy-only.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return nil, &net.DNSError{IsNotFound: true}
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			transport := newDesktopAwareTransport(true).(*desktopAwareTransport)
			if tt.resolver != nil {
				transport.resolver = tt.resolver
			}
			assert.Equal(t, tt.want, transport.proxySafe(t.Context(), tt.host))
		})
	}
}

func TestDesktopAwareTransportLoopbackIsNeverProxied(t *testing.T) {
	t.Parallel()

	assert.True(t, isLoopbackHost("localhost"))
	assert.True(t, isLoopbackHost("127.0.0.1"))
	assert.True(t, isLoopbackHost("::1"))
	assert.False(t, isLoopbackHost("example.com"))
}

func TestDesktopAwareTransportUsesDesktopTransportPerRequest(t *testing.T) {
	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	var calls int
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
		calls++
		return roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
		})
	}

	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
		require.NoError(t, err)
		resp, err := transport.RoundTrip(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, 2, calls)
}

func TestDesktopAwareTransportDisabledOrGuardedUsesDirect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		guarded  bool
		host     string
		disabled bool
	}{
		{name: "kill switch", host: "example.com", disabled: true},
		{name: "guarded loopback", guarded: true, host: "127.0.0.1"},
		{name: "unguarded loopback", host: "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.disabled {
				t.Setenv(disableDesktopProxyEnv, "1")
			}
			transport := newDesktopAwareTransport(tc.guarded).(*desktopAwareTransport)
			transport.resolver = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.1")}, nil }
			transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
				t.Fatal("Desktop transport must not be used")
				return nil
			}
			transport.direct = &http.Transport{}
			transport.direct.RegisterProtocol("https", roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
			}))
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+tc.host, http.NoBody)
			require.NoError(t, err)
			resp, err := transport.RoundTrip(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDesktopAwareTransportConsultsDesktopDetectionPerRequest(t *testing.T) {
	var detections int
	desktoptransport.SetDesktopRunningForTest(t, func(context.Context) (bool, error) {
		detections++
		return false, nil
	})

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	assert.Zero(t, detections)
	transport.direct = &http.Transport{}
	transport.direct.RegisterProtocol("https", roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}))

	for range 2 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
		require.NoError(t, err)
		resp, err := transport.RoundTrip(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, 1, detections)
}

func TestDesktopAwareTransportDisableCompressionBeforeDesktopTransport(t *testing.T) {
	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	transport.DisableCompression()
	proxy := &compressionTransport{}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper { return proxy }

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.True(t, proxy.disabled)
}

func TestDesktopAwareTransportDisableCompressionBeforeAndAfterDirectFallback(t *testing.T) {
	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	proxy := &compressionTransport{}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper { return proxy }

	transport.DisableCompression()
	assert.True(t, transport.direct.DisableCompression)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://127.0.0.1", http.NoBody)
	require.NoError(t, err)
	assert.True(t, isLoopbackHost(req.URL.Hostname()))
}

type compressionTransport struct {
	disabled bool
}

func (t *compressionTransport) DisableCompression() {
	t.disabled = true
}

func (t *compressionTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
}

func TestDesktopProxyDisabled(t *testing.T) {
	t.Setenv(disableDesktopProxyEnv, "1")
	require.True(t, desktopProxyDisabled())
}

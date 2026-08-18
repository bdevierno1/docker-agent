// Package transport provides an HTTP transport that routes requests through
// the Docker Desktop proxy when available. It lives in its own leaf package
// so callers that only need the transport don't pull the OCI registry stack
// that pkg/remote depends on.
package transport

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/desktop"
	socket "github.com/docker/docker-agent/pkg/desktop/socket"
	"github.com/docker/docker-agent/pkg/memoize"
)

var (
	memoizer       = memoize.New[bool](1 * time.Minute)
	detectionMu    sync.Mutex
	desktopRunning = func(ctx context.Context) (bool, error) {
		return desktop.IsDockerDesktopRunning(context.WithoutCancel(ctx)), nil
	}
)

// New returns an HTTP transport that uses the Docker Desktop proxy
// if available, and falls back to direct connections while re-probing the
// proxy after a cooldown so long-lived processes recover on their own.
func New(ctx context.Context) http.RoundTripper {
	return NewWithDirectTransport(ctx, nil)
}

// NewWithDirectTransport is like New but uses direct as its direct fallback.
// A nil direct clones http.DefaultTransport.
func NewWithDirectTransport(ctx context.Context, direct http.RoundTripper) http.RoundTripper {
	if running, err := DesktopRunning(ctx); err == nil && running {
		return NewDesktopTransport(direct)
	}
	return directTransport(direct)
}

// DesktopRunning reports the memoized Docker Desktop availability state.
func DesktopRunning(ctx context.Context) (bool, error) {
	detectionMu.Lock()
	defer detectionMu.Unlock()

	return memoizer.Memoize("desktopRunning", func() (bool, error) {
		return desktopRunning(ctx)
	})
}

// NewDesktopTransport returns a Docker Desktop proxy transport with direct fallback.
// A nil direct clones http.DefaultTransport.
func NewDesktopTransport(direct http.RoundTripper) http.RoundTripper {
	transport := directTransport(direct)
	proxyTransport := transport.Clone()
	proxyTransport.Proxy = http.ProxyURL(&url.URL{
		Scheme: "http",
	})
	proxyTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return socket.DialUnix(ctx, desktop.Paths().ProxySocket)
	}
	return newFallbackTransport(proxyTransport, transport)
}

func directTransport(direct http.RoundTripper) *http.Transport {
	if transport, ok := direct.(*http.Transport); ok {
		return transport
	}
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

// SetDesktopRunningForTest replaces Docker Desktop detection until cleanup.
// It is intended for deterministic transport tests outside this package.
func SetDesktopRunningForTest(tb testing.TB, detect func(context.Context) (bool, error)) {
	tb.Helper()
	detectionMu.Lock()
	previous := desktopRunning
	desktopRunning = detect
	memoizer = memoize.New[bool](time.Nanosecond)
	detectionMu.Unlock()
	tb.Cleanup(func() {
		detectionMu.Lock()
		defer detectionMu.Unlock()
		desktopRunning = previous
		memoizer = memoize.New[bool](1 * time.Minute)
	})
}

// Bounded backoff: one probe per cooldown, not per request.
const proxyRetryCooldown = 30 * time.Second

// fallbackTransport tries the proxy first, direct second. A socket error
// disables the proxy for proxyRetryCooldown, so a stale error can't latch
// the transport into direct mode for the rest of the process's lifetime.
type fallbackTransport struct {
	proxy  *http.Transport
	direct *http.Transport

	// Zero = enabled. Non-zero = disabled until this UnixNano deadline.
	disabledUntilUnixNano atomic.Int64
}

// newFallbackTransport creates a transport that tries the proxy first, then falls back to direct.
func newFallbackTransport(proxy, direct *http.Transport) *fallbackTransport {
	return &fallbackTransport{
		proxy:  proxy,
		direct: direct,
	}
}

// DisableCompression disables automatic gzip compression on both transports.
// This is needed for SSE streaming compatibility.
func (f *fallbackTransport) DisableCompression() {
	f.proxy.DisableCompression = true
	f.direct.DisableCompression = true
}

func (f *fallbackTransport) proxyEnabled() bool {
	until := f.disabledUntilUnixNano.Load()
	if until == 0 {
		return true
	}
	if time.Now().UnixNano() < until {
		return false
	}
	// CAS (not Store) so a concurrent disableProxy() can't be stomped.
	f.disabledUntilUnixNano.CompareAndSwap(until, 0)
	return true
}

func (f *fallbackTransport) disableProxy() {
	f.disabledUntilUnixNano.Store(time.Now().Add(proxyRetryCooldown).UnixNano())
}

func (f *fallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !f.proxyEnabled() {
		return f.direct.RoundTrip(req)
	}

	resp, err := f.proxy.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if !isProxySocketError(err) {
		return nil, err
	}

	slog.Warn("Docker Desktop proxy unavailable, falling back to direct connection",
		"error", err.Error(),
		"url", req.URL.String(),
		"retry_after", proxyRetryCooldown)
	f.disableProxy()

	// Retry direct only when the body is safe to replay; otherwise the
	// proxy may have already consumed it.
	if req.Body != nil && req.GetBody == nil {
		return nil, err
	}
	retryReq := req.Clone(req.Context())
	if req.GetBody != nil {
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return nil, err
		}
		retryReq.Body = body
	}
	return f.direct.RoundTrip(retryReq)
}

// isProxySocketError checks if the error indicates the proxy socket is unavailable.
// This includes:
// - "no such file or directory" - socket file was deleted
// - "connection refused" - socket exists but nothing is listening
// - "dial unix" errors - general Unix socket connection failures
func isProxySocketError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	// Check for common proxy socket failure patterns
	proxyErrorPatterns := []string{
		"no such file or directory",   // Socket file deleted
		"connect: connection refused", // Socket exists but no listener
		"proxyconnect tcp",            // Proxy connection failure
		"dial unix",                   // Unix socket dial failure
		"unix socket",                 // Generic Unix socket error
	}

	for _, pattern := range proxyErrorPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

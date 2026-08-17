package httpclient

import (
	"net/http"
	"time"
)

// DefaultToolHTTPTimeout is the HTTP client timeout used by the built-in
// HTTP-based toolsets (`fetch`, `api`, `openapi`, `a2a`) when the operator
// does not override it via `timeout:` in the agent config.
//
// Centralised so the four toolsets agree on a single default — changing
// this value uniformly affects every HTTP-based built-in tool.
const DefaultToolHTTPTimeout = 30 * time.Second

// NewAllowPrivateIPsClient returns an HTTP client for explicit
// allow_private_ips opt-ins. It can reach private addresses directly, but uses
// Docker Desktop's PAC proxy when available; loopback is always direct. Docker
// Desktop remains optional and CAGENT_DISABLE_DESKTOP_PROXY=1 restores the
// default environment-proxy/direct behavior.
func NewAllowPrivateIPsClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     newAllowPrivateIPsTransport(),
		CheckRedirect: BoundedRedirects(10),
	}
}

// NewSafeClient returns the HTTP client used by built-in tools that issue
// outbound calls to URLs the operator (or a fetched OpenAPI spec) supplies.
//
// The default refuses connections to non-public IPs at dial time
// — defeating DNS rebinding to loopback / RFC1918 / link-local incl. cloud
// metadata at 169.254.169.254 — and bounds the redirect chain at 10 hops.
//
// When unsafe is true the client uses [http.DefaultTransport]. This branch
// exists ONLY for tests, which use [httptest.NewServer] (binds to 127.0.0.1)
// and therefore cannot pass the SSRF check.
func NewSafeClient(timeout time.Duration, unsafe bool) *http.Client {
	if unsafe {
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     NewDesktopAwareSSRFSafeTransport(),
		CheckRedirect: BoundedRedirects(10),
	}
}

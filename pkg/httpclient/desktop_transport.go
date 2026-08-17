package httpclient

import (
	"context"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"

	desktoptransport "github.com/docker/docker-agent/pkg/desktop/transport"
)

const disableDesktopProxyEnv = "CAGENT_DISABLE_DESKTOP_PROXY"

type desktopAwareTransport struct {
	direct   *http.Transport
	guarded  bool
	resolver func(context.Context, string) ([]net.IP, error)

	once      sync.Once
	desktopRT http.RoundTripper
}

func newDesktopAwareTransport(guarded bool) http.RoundTripper {
	var direct *http.Transport
	if guarded {
		direct = NewSSRFSafeTransport()
	} else {
		direct = cloneDefaultTransport()
	}
	return &desktopAwareTransport{
		direct:  direct,
		guarded: guarded,
		resolver: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
	}
}

// NewDesktopAwareSSRFSafeTransport returns a transport that preserves the
// existing dial-time SSRF guard and uses Docker Desktop's PAC proxy when it is
// available. Docker Desktop is optional: absent or disabled, requests use the
// same environment-proxy/direct transport as NewSSRFSafeTransport.
func NewDesktopAwareSSRFSafeTransport() http.RoundTripper {
	return newDesktopAwareTransport(true)
}

func newAllowPrivateIPsTransport() http.RoundTripper {
	return newDesktopAwareTransport(false)
}

func cloneDefaultTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

func (t *desktopAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if desktopProxyDisabled() || isLoopbackHost(req.URL.Hostname()) || (t.guarded && !t.proxySafe(req.Context(), req.URL.Hostname())) {
		return t.direct.RoundTrip(req)
	}
	t.once.Do(func() {
		t.desktopRT = desktoptransport.NewWithDirectTransport(req.Context(), t.direct)
	})
	return t.desktopRT.RoundTrip(req)
}

func (t *desktopAwareTransport) proxySafe(ctx context.Context, host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return IsPublicIP(ip)
	}
	ips, err := t.resolver(ctx, host)
	if err != nil {
		// A PAC proxy can resolve names unavailable to the local resolver.
		return true
	}
	return slices.ContainsFunc(ips, IsPublicIP)
}

func desktopProxyDisabled() bool {
	return os.Getenv(disableDesktopProxyEnv) == "1"
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (t *desktopAwareTransport) DisableCompression() {
	t.direct.DisableCompression = true
	if disabler, ok := t.desktopRT.(interface{ DisableCompression() }); ok {
		disabler.DisableCompression()
	}
}

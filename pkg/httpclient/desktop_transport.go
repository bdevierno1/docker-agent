package httpclient

import (
	"context"
	"net"
	"net/http"
	"os"

	desktoptransport "github.com/docker/docker-agent/pkg/desktop/transport"
)

const disableDesktopProxyEnv = "CAGENT_DISABLE_DESKTOP_PROXY"

type desktopAwareTransport struct {
	direct              *http.Transport
	guarded             bool
	resolver            func(context.Context, string) ([]net.IP, error)
	newDesktopTransport func(context.Context, http.RoundTripper) http.RoundTripper

	disableCompression bool
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
		newDesktopTransport: desktoptransport.NewWithDirectTransport,
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
	desktopRT := t.newDesktopTransport(req.Context(), t.direct)
	if t.disableCompression {
		if disabler, ok := desktopRT.(interface{ DisableCompression() }); ok {
			disabler.DisableCompression()
		}
	}
	return desktopRT.RoundTrip(req)
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
	if len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !IsPublicIP(ip) {
			return false
		}
	}
	return true
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
	t.disableCompression = true
	t.direct.DisableCompression = true
}

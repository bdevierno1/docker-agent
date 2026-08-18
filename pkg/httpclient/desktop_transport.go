package httpclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"golang.org/x/net/http/httpproxy"

	desktoptransport "github.com/docker/docker-agent/pkg/desktop/transport"
)

const disableDesktopProxyEnv = "DOCKER_AGENT_DISABLE_DESKTOP_PROXY"

type desktopAwareTransport struct {
	direct              *http.Transport
	guarded             bool
	resolver            func(context.Context, string) ([]net.IP, error)
	newDesktopTransport func(context.Context, http.RoundTripper) http.RoundTripper

	mu                 sync.Mutex
	desktopTransport   http.RoundTripper
	disableCompression bool
	warnedCompression  bool
}

func newDesktopAwareTransport(guarded bool) http.RoundTripper {
	var direct *http.Transport
	if guarded {
		direct = NewSSRFSafeTransport()
	} else {
		direct = cloneDefaultTransport(environmentProxyFunc())
	}
	return &desktopAwareTransport{
		direct:  direct,
		guarded: guarded,
		resolver: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		newDesktopTransport: func(_ context.Context, direct http.RoundTripper) http.RoundTripper {
			return desktoptransport.NewDesktopTransport(direct)
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

func cloneDefaultTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport := base.Clone()
		transport.Proxy = proxy
		return transport
	}
	return &http.Transport{Proxy: proxy}
}

func environmentProxyFunc() func(*http.Request) (*url.URL, error) {
	proxyForURL := httpproxy.FromEnvironment().ProxyFunc()
	return func(req *http.Request) (*url.URL, error) {
		return proxyForURL(req.URL)
	}
}

func (t *desktopAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if desktopProxyDisabled() || isLoopbackHost(req.URL.Hostname()) {
		return t.direct.RoundTrip(req)
	}

	transport := t.selectedTransportFor(req.Context())
	if transport == t.direct || (t.guarded && !t.proxySafe(req.Context(), req.URL.Hostname())) {
		return t.direct.RoundTrip(req)
	}
	return transport.RoundTrip(req)
}

func (t *desktopAwareTransport) selectedTransportFor(ctx context.Context) http.RoundTripper {
	running, err := desktoptransport.DesktopRunning(ctx)
	if err != nil || !running {
		return t.direct
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.desktopTransport == nil {
		t.desktopTransport = t.newDesktopTransport(ctx, t.direct)
		if t.disableCompression {
			disableCompression(t.desktopTransport)
		}
	}
	return t.desktopTransport
}

func (t *desktopAwareTransport) proxySafe(ctx context.Context, host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return IsPublicIP(ip)
	}
	ips, err := t.resolver(ctx, host)
	if err != nil {
		var dnsErr *net.DNSError
		return errors.As(err, &dnsErr) && dnsErr.IsNotFound
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
	value := strings.ToLower(strings.TrimSpace(os.Getenv(disableDesktopProxyEnv)))
	switch value {
	case "", "0", "false", "no", "off":
		return false
	case "1", "true", "yes", "on":
		return true
	default:
		slog.Warn("unrecognized DOCKER_AGENT_DISABLE_DESKTOP_PROXY value; treating it as disabled", "value", value)
		return false
	}
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (t *desktopAwareTransport) DisableCompression() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.disableCompression = true
	if !disableCompression(t.direct) && !t.warnedCompression {
		t.warnedCompression = true
		slog.Warn("cannot disable compression for custom direct transport", "transport", fmt.Sprintf("%T", t.direct))
	}
	disableCompression(t.desktopTransport)
}

func disableCompression(transport any) bool {
	if transport == nil {
		return true
	}
	if disabler, ok := transport.(interface{ DisableCompression() }); ok {
		disabler.DisableCompression()
		return true
	}
	if direct, ok := transport.(*http.Transport); ok {
		direct.DisableCompression = true
		return true
	}
	return false
}

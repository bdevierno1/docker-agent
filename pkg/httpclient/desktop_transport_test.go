package httpclient

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			name: "one resolved address public",
			host: "mixed.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("1.1.1.1")}, nil
			},
			want: true,
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

func TestDesktopProxyDisabled(t *testing.T) {
	t.Setenv(disableDesktopProxyEnv, "1")
	require.True(t, desktopProxyDisabled())
}

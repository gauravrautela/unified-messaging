// Package safehttp builds HTTP clients for attacker-influenced destinations:
// webhook targets, notify URLs, chat-platform APIs. They never follow
// redirects and refuse to connect to non-public addresses, checked on the
// resolved IP at dial time so a hostname or a DNS rebind cannot smuggle a
// request into the private network.
package safehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var ErrPrivateAddress = errors.New("safehttp: destination is not a public address")

var allowLoopback atomic.Bool

// AllowLoopbackForTests lets httptest servers (always loopback) be dialled
// for the lifetime of t. Never called from production code.
func AllowLoopbackForTests(t testing.TB) {
	t.Helper()
	allowLoopback.Store(true)
	t.Cleanup(func() { allowLoopback.Store(false) })
}

var (
	cgnat    = netip.MustParsePrefix("100.64.0.0/10")
	metadata = netip.MustParsePrefix("169.254.0.0/16")
)

// PublicOnlyControl is the net.Dialer.Control hook. address is host:port
// with the host already resolved to an IP literal.
func PublicOnlyControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrPrivateAddress
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return ErrPrivateAddress
	}
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if ip.IsLoopback() && allowLoopback.Load() {
		return nil
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsMulticast(), ip.IsUnspecified(), ip.IsInterfaceLocalMulticast(),
		cgnat.Contains(ip), metadata.Contains(ip):
		return ErrPrivateAddress
	}
	return nil
}

// Client returns a client with the dial guard and no redirect following.
// A 3xx answer is returned to the caller as-is, so a webhook that redirects
// simply fails its delivery with that status.
func Client(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: PublicOnlyControl}
	transport := &http.Transport{
		Proxy: nil, // never honour HTTP_PROXY for attacker-chosen URLs
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

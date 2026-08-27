package api

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// publicHTTPURL rejects URLs the server must never be talked into fetching.
//
// Webhook targets, notify_url, and the OAuth redirect URLs are all
// attacker-chosen and all end up as outbound requests from inside our network.
// Under open signup that makes them an SSRF primitive: register
// http://169.254.169.254/… or an RFC1918 address, then read the outcome back
// through GET /api/v1/webhooks/{id}/deliveries, which reports the status code.
//
// The check is deliberately literal — it inspects the host as written and
// never resolves DNS, so a hostname that resolves to a private address, or a
// DNS rebind, still gets through this check alone. What actually closes that
// gap is the safehttp dial guard every one of these targets is fetched
// through: it checks the resolved IP at dial time, on every connection
// attempt, however the hostname resolves. This check remains as a fast,
// early rejection of the literal-IP case, before a link is ever minted.
func publicHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url %q is not a valid URL", raw)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url %q must be an absolute http(s) URL", raw)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("url %q must name a host", raw)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("url %q must not point at localhost", raw)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Not an IP literal: a hostname we deliberately do not resolve.
		return nil
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	switch {
	case addr.IsLoopback(), addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast(),
		addr.IsPrivate(), addr.IsUnspecified(), addr.IsMulticast(),
		addr.IsInterfaceLocalMulticast():
		return fmt.Errorf("url %q must not point at a private, loopback, or link-local address", raw)
	}
	return nil
}

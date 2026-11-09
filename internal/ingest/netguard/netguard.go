// Package netguard is the SSRF boundary: it resolves a URL's host and
// blocks loopback, private, link-local, multicast and unspecified targets
// before every network hop (including each redirect) — and, through
// CheckDial, at connect time on the actual resolved address, which closes
// the DNS-rebinding window between check-time and dial-time resolution.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/url"
)

// Guard checks URLs against the blocked address ranges.
type Guard struct{}

// CheckURL resolves the URL's host and errors when any resolved address is
// private or special. IP literals are checked directly (the resolver parses
// them), so no DNS is needed for those.
func (g *Guard) CheckURL(ctx context.Context, u *url.URL) error {
	if u == nil {
		return fmt.Errorf("netguard: nil url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("netguard: scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("netguard: url without host")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("netguard: resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if IsBlocked(ip.IP) {
			return fmt.Errorf("netguard: blocked address %s for host %q", ip.IP, host)
		}
	}
	return nil
}

// CheckDial validates the actual address a connection is about to dial
// (host:port as resolved by the dialer); it is the rebinding-proof half of
// the guard.
func (g *Guard) CheckDial(_, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("netguard: dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("netguard: dial address %q: unparsable host", address)
	}
	if IsBlocked(ip) {
		return fmt.Errorf("netguard: blocked dial target %s", ip)
	}
	return nil
}

// blockedNets are special-purpose ranges beyond the stdlib predicates
// (CGNAT, documentation, benchmarking, reserved, NAT64 and deprecated
// site-local prefixes).
var blockedNets = func() []*net.IPNet {
	raw := []string{
		"100.64.0.0/10",   // RFC 6598 CGNAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved (incl. 255.255.255.255)
		"64:ff9b::/96",    // NAT64 well-known prefix
		"fec0::/10",       // deprecated site-local
	}
	out := make([]*net.IPNet, 0, len(raw))
	for _, cidr := range raw {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("netguard: bad blocked range " + cidr)
		}
		out = append(out, n)
	}
	return out
}()

// IsBlocked reports loopback, private, link-local (including the cloud
// metadata address 169.254.169.254), multicast, unspecified and the
// special-purpose ranges above.
func IsBlocked(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

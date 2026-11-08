// Package netguard decides which network addresses Gotell may connect to,
// so a target URL cannot be used to reach internal systems (SSRF,
// blueprint §13).
//
// The check runs on the real IP address at the moment of connecting
// (Dialer.Control), after DNS has been resolved. Checking only the URL
// text would miss a name that resolves to an internal address, or one
// that changes its answer between the check and the connection (DNS
// rebinding).
package netguard

import (
	"errors"
	"fmt"
	"net/netip"
	"syscall"
)

// ErrBlocked marks a connection refused because it pointed at an internal
// address. Use errors.Is to detect it.
var ErrBlocked = errors.New("blocked: private or internal address")

// extraBlocked are internal ranges that netip.Addr has no helper for.
var extraBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, shared address space
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking networks
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, includes 255.255.255.255
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64: may map to an internal IPv4
	netip.MustParsePrefix("2001:db8::/32"), // documentation only
}

// IsBlocked reports whether ip is internal: loopback (127.0.0.1, ::1),
// private networks (10.x, 172.16-31.x, 192.168.x, fc00::/7), link-local
// (169.254.x.x, where cloud metadata services live, and fe80::/10),
// multicast, unspecified, or another reserved range.
func IsBlocked(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:127.0.0.1 is 127.0.0.1 in disguise
	if !ip.IsValid() ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() {
		return true
	}
	for _, p := range extraBlocked {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Control is a net.Dialer Control function that refuses connections to
// internal addresses. address is "ip:port", already resolved.
func Control(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w (%s: unreadable address)", ErrBlocked, address)
	}
	if IsBlocked(ap.Addr()) {
		return fmt.Errorf("%w (%s)", ErrBlocked, ap.Addr())
	}
	return nil
}

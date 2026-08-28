// Package netutil provides small network-address helpers shared by the
// gateway entrypoint and the API surface.
package netutil

import (
	"net"
	"strings"
)

// IsLoopbackHost reports whether host is a loopback host: empty, "localhost",
// or an IP address in the loopback ranges (127.0.0.0/8 or ::1). Empty counts
// as loopback for client-address checks, where a missing RemoteAddr means a
// local origin request; a bind check that treats an empty host as a wildcard
// (all-interface) bind must special-case it in the caller.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isCGNAT reports whether ip is in 100.64.0.0/10, the CGNAT range that
// contains Tailscale's default 100.x.y.z addresses. net.IP.IsPrivate does not
// cover it (RFC 6598 is not RFC 1918), so it is checked explicitly.
func isCGNAT(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 100 && ip4[1]&0xC0 == 0x40
}

// isULA reports whether ip is in fc00::/7 (RFC 4193), the unique-local
// range containing Tailscale's IPv6 addresses (fd7a::/48).
func isULA(ip net.IP) bool {
	return len(ip) == net.IPv6len && ip[0]&0xFE == 0xFC
}

// IsPrivateAddress reports whether ip is a loopback, private (RFC 1918,
// RFC 4193), or CGNAT (RFC 6598) address: the families a single-user gateway
// is meant to serve.
func IsPrivateAddress(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || isCGNAT(ip) || isULA(ip)
}

// IsBindableHost reports whether host names an interface the gateway may bind
// without an explicit override: the loopback families, a CGNAT address (the
// Tailscale default range, 100.64.0.0/10), or a ULA (fc00::/7, e.g. a custom
// Tailscale IPv6). Plain RFC 1918 LAN addresses are not included — binding
// them would expose the unauthenticated API to every host on the local
// network. Hostnames are resolved first: "localhost" and any name resolving
// only to such addresses qualify. A wildcard (empty) host is refused — it
// would bind every interface, including untrusted LANs, and silently defeat
// the local-only default.
func IsBindableHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	for _, ip := range resolveHost(host) {
		if ip.IsLoopback() || isCGNAT(ip) || isULA(ip) {
			return true
		}
	}
	return false
}

// resolveHost resolves host to its IP addresses. The literal hosts
// "localhost" and "ip6-localhost" are mapped to their loopback addresses
// directly (they are not guaranteed to appear in /etc/hosts); "localhost"
// yields 127.0.0.1 and ::1, "ip6-localhost" yields ::1. Any other host is
// resolved via the resolver, with a bare IP address matching itself. Failures
// (NxDomain, malformed address) return nil.
func resolveHost(host string) []net.IP {
	lower := strings.ToLower(host)
	switch lower {
	case "localhost":
		return []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	case "ip6-localhost":
		return []net.IP{net.IPv6loopback}
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		return nil
	}
	return addrs
}

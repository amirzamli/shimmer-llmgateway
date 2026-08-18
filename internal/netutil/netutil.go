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

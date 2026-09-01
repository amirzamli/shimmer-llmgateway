package gateway

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/amirzamli/shimmer-llmgateway/internal/netutil"
)

// The §6.2 REST API and the embedded UI are unauthenticated by design, so the
// requests addressing them are validated against the same local-only policy
// the listen addresses follow: the Host header must name an address the
// gateway serves (a bound listen host, a loopback host, or a literal
// loopback/CGNAT/ULA address), and state-changing requests must be same-site
// (matching Origin, or a non-cross-site Sec-Fetch-Site), so a web page the
// operator is viewing cannot drive the admin API cross-site from the browser.
// The capture surface and /healthz are deliberately not guarded: non-browser
// LLM clients address those with whatever Host their base_url carries and
// send no Origin.

// hostGuard validates admin-surface requests against the listen addresses and
// the bind policy's address families.
type hostGuard struct {
	// boundHosts is the lowercased host part of every configured listen
	// address, so named hosts (e.g. a Tailscale MagicDNS name resolving to a
	// CGNAT address) are accepted by exact match without any request-path DNS.
	boundHosts map[string]bool
}

// newHostGuard builds the guard over the gateway's listen addresses.
func newHostGuard(listenAddrs []string) *hostGuard {
	g := &hostGuard{boundHosts: map[string]bool{}}
	for _, a := range listenAddrs {
		a = strings.TrimSpace(a)
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			host = a // no port; treat the whole value as the host
		}
		if host != "" {
			g.boundHosts[strings.ToLower(host)] = true
		}
	}
	return g
}

// rejectReason returns why r may not proceed on the admin surface, or "" when
// it may.
func (g *hostGuard) rejectReason(r *http.Request) string {
	host := hostOnly(r.Host)
	if !g.allowHost(host) {
		return fmt.Sprintf("host %q is not an address this gateway serves", host)
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ""
	}
	// State-changing method: require same-site. An explicit Origin wins —
	// browsers send it on every cross-site request that matters here.
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			// Includes the sandboxed "null" origin.
			return fmt.Sprintf("cross-origin request rejected: unusable Origin %q", origin)
		}
		if !strings.EqualFold(u.Host, r.Host) {
			return fmt.Sprintf("cross-origin request rejected: Origin %q does not match %q", origin, r.Host)
		}
		return ""
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" &&
		site != "same-origin" && site != "same-site" && site != "none" {
		return fmt.Sprintf("cross-site request rejected: Sec-Fetch-Site %q", site)
	}
	return ""
}

// allowHost reports whether host (a Host header value with the port already
// removed, lowercased) may address the admin surface: an exactly bound listen
// host, a loopback host ("localhost" or a loopback literal), or a literal IP
// in one of the families the bind policy serves (loopback, CGNAT
// 100.64.0.0/10, ULA fc00::/7). Any other hostname is refused — hostnames are
// never resolved here, only literal IPs and exact bound-name matches count.
func (g *hostGuard) allowHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	if g.boundHosts[host] {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return netutil.IsServedHostIP(ip)
	}
	return netutil.IsLoopbackHost(host)
}

// hostOnly strips the port from a Host header value ("127.0.0.1:8787" →
// "127.0.0.1"); bracketed IPv6 keeps its colons, and a value without a port
// passes through unchanged. A bracketed literal with no port ("[::1]") is
// SplitHostPort's failure case, so only its surrounding brackets are
// stripped — the guard compares against the unbracketed host ("::1") that a
// "[::1]:port" listen address is bound by.
func hostOnly(hostPort string) string {
	h := strings.TrimSpace(hostPort)
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		return h[1 : len(h)-1]
	}
	return h
}

// guard wraps a browser-facing handler (the §6.2 REST API, the embedded UI)
// with the admin-surface host/origin policy and maps a rejection to a 403.
func (s *Server) guard(next http.Handler) http.Handler {
	g := newHostGuard(s.listenAddrs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reason := g.rejectReason(r); reason != "" {
			s.writeError(w, http.StatusForbidden, "FORBIDDEN", reason)
			return
		}
		next.ServeHTTP(w, r)
	})
}

package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestHostGuardAllowHost pins the admin-surface Host policy: an exactly bound
// listen host, a loopback host, or a literal loopback/CGNAT/ULA IP. Any other
// hostname is refused — never DNS-resolved — and RFC 1918 LAN addresses are
// refused too (the bind policy does not serve them).
func TestHostGuardAllowHost(t *testing.T) {
	g := newHostGuard([]string{"127.0.0.1:8787", "myhost.tailnet.example:8787"})
	cases := []struct {
		host string
		want bool
	}{
		{"", false},                           // empty Host
		{"127.0.0.1", true},                   // bound + loopback literal
		{"127.0.0.1:8787", false},             // port must be stripped before the check
		{"localhost", true},                   // loopback host
		{"::1", true},                         // IPv6 loopback
		{"100.64.0.1", true},                  // CGNAT (Tailscale default range)
		{"100.127.255.254", true},             // top of the CGNAT /10
		{"100.63.0.1", false},                 // below the CGNAT range
		{"100.128.0.1", false},                // above the CGNAT range
		{"fd00::1", true},                     // ULA
		{"fd7a::1", true},                     // Tailscale IPv6 ULA
		{"192.168.1.5", false},                // RFC 1918 is not served by the bind policy
		{"10.0.0.9", false},                   // RFC 1918
		{"8.8.8.8", false},                    // public IP
		{"evil.example.com", false},           // unbound hostname: never resolved
		{"myhost.tailnet.example", true},      // exactly bound (MagicDNS-style name)
		{"MYHOST.TAILNET.EXAMPLE", true},      // bound, case-insensitive
		{"localhost.evil.example.com", false}, // not a loopback host
		{"127.0.0.1.evil.example.com", false}, // loopback-looking but a hostname
	}
	for _, c := range cases {
		if got := g.allowHost(c.host); got != c.want {
			t.Errorf("allowHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestGuardEmptyAllowlistStillAllowsLocal verifies the fallback when the
// gateway has no listen addresses recorded (nil): the loopback/CGNAT/ULA
// family policy alone decides.
func TestGuardEmptyAllowlistStillAllowsLocal(t *testing.T) {
	g := newHostGuard(nil)
	for _, host := range []string{"127.0.0.1", "localhost", "100.64.0.1", "fd00::1"} {
		if !g.allowHost(host) {
			t.Errorf("allowHost(%q) = false with an empty bind list, want true", host)
		}
	}
	if g.allowHost("evil.example.com") {
		t.Error("unbound hostname must stay refused with an empty bind list")
	}
}

// guardTestServer serves a full gateway handler. listenAddrs, when non-empty,
// overrides the server's bound-address list (the guard's exact-match seed).
func guardTestServer(t *testing.T, listenAddrs []string) *httptest.Server {
	t.Helper()
	provider := newFakeProvider(t, nil)
	srv, _ := newGatewayServer(t, provider, twoInstanceTOML, defaultEnv, io.Discard)
	if listenAddrs != nil {
		srv.listenAddrs = listenAddrs
	}
	gs := httptest.NewServer(srv.Handler())
	t.Cleanup(gs.Close)
	return gs
}

// doWithHost sends a request with an explicit Host header (and optional
// Origin/Sec-Fetch-Site) and returns the response status.
func doWithHost(t *testing.T, gs *httptest.Server, method, path, host, origin, secFetchSite string) int {
	t.Helper()
	req, err := http.NewRequest(method, gs.URL+path, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if secFetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", secFetchSite)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestGuardRejectsForgedHost(t *testing.T) {
	gs := guardTestServer(t, nil)

	// The httptest default Host (a 127.0.0.1 literal) passes the guard.
	if st := doWithHost(t, gs, http.MethodGet, "/api/status", "", "", ""); st == http.StatusForbidden {
		t.Errorf("GET /api/status with loopback Host = %d, want not 403", st)
	}

	// A forged, non-serving Host is rejected on both admin surfaces.
	for _, path := range []string{"/api/status", "/"} {
		if st := doWithHost(t, gs, http.MethodGet, path, "evil.example.com", "", ""); st != http.StatusForbidden {
			t.Errorf("GET %s with forged Host = %d, want 403", path, st)
		}
	}

	// The capture surface is deliberately not host-guarded: the /v1/ fallback
	// answers 404, not 403, for an unusual Host.
	if st := doWithHost(t, gs, http.MethodPost, "/v1/nope", "evil.example.com", "", ""); st != http.StatusNotFound {
		t.Errorf("POST /v1/nope with forged Host = %d, want 404 (capture surface is not guarded)", st)
	}
}

// TestHostOnlyStripsBracketedIPv6WithoutPort pins the port-stripping edge:
// SplitHostPort rejects a bracketed literal with no port ("[::1]"), so
// hostOnly must drop just the surrounding brackets — the guard matches
// against the unbracketed host a "[::1]:port" listen address is bound by.
func TestHostOnlyStripsBracketedIPv6WithoutPort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:8787", "127.0.0.1"},
		{"[::1]:8787", "::1"}, // bracketed literal with a port
		{"[::1]", "::1"},      // bracketed literal without a port
		{"::1", "::1"},        // unbracketed literal passes through
		{"myhost.tailnet.example", "myhost.tailnet.example"},
	}
	for _, c := range cases {
		if got := hostOnly(c.in); got != c.want {
			t.Errorf("hostOnly(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestGuardAllowsBracketedIPv6Host verifies an exactly bound IPv6 literal
// listen address accepts both Host spellings a client can send: with a port
// and as a bare bracketed literal with no port (which SplitHostPort alone
// would reject, leaving brackets in place for the host match).
func TestGuardAllowsBracketedIPv6Host(t *testing.T) {
	gs := guardTestServer(t, []string{"[::1]:8787"})
	for _, host := range []string{"[::1]:8787", "[::1]"} {
		if st := doWithHost(t, gs, http.MethodGet, "/api/status", host, "", ""); st == http.StatusForbidden {
			t.Errorf("GET /api/status with Host %q = %d, want not 403", host, st)
		}
	}
}

func TestGuardAllowsBoundHostname(t *testing.T) {
	gs := guardTestServer(t, []string{"myhost.tailnet.example:8787"})
	if st := doWithHost(t, gs, http.MethodGet, "/api/status", "myhost.tailnet.example:8787", "", ""); st == http.StatusForbidden {
		t.Errorf("GET /api/status with the bound hostname = %d, want not 403", st)
	}
	if st := doWithHost(t, gs, http.MethodGet, "/api/status", "otherhost.tailnet.example:8787", "", ""); st != http.StatusForbidden {
		t.Errorf("GET /api/status with an unbound hostname = %d, want 403", st)
	}
}

func TestGuardRejectsCrossOriginPost(t *testing.T) {
	gs := guardTestServer(t, nil)
	u, err := url.Parse(gs.URL)
	if err != nil {
		t.Fatal(err)
	}
	sameOrigin := "http://" + u.Host // the httptest server's own origin

	cases := []struct {
		name          string
		origin        string
		secFetchSite  string
		wantForbidden bool
	}{
		{"same-origin Origin passes", sameOrigin, "", false},
		{"cross-origin Origin rejected", "https://evil.example", "", true},
		{"null Origin rejected", "null", "", true},
		{"unparseable Origin rejected", "::", "", true},
		{"cross-site Sec-Fetch-Site rejected", "", "cross-site", true},
		{"unknown Sec-Fetch-Site rejected", "", "subdomain-hijack", true},
		{"same-origin Sec-Fetch-Site passes", "", "same-origin", false},
		{"user-initiated Sec-Fetch-Site passes", "", "none", false},
		{"no Origin and no Sec-Fetch-Site passes (non-browser client)", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := doWithHost(t, gs, http.MethodPatch, "/api/settings", "", c.origin, c.secFetchSite)
			if c.wantForbidden && st != http.StatusForbidden {
				t.Errorf("PATCH /api/settings status = %d, want 403", st)
			}
			if !c.wantForbidden && st == http.StatusForbidden {
				t.Errorf("PATCH /api/settings status = %d, want not 403", st)
			}
		})
	}

	// The host check runs first: a cross-origin POST from a hostile Host is
	// rejected as a host violation even before the Origin matters.
	if st := doWithHost(t, gs, http.MethodPatch, "/api/settings", "evil.example.com", sameOrigin, ""); st != http.StatusForbidden {
		t.Errorf("PATCH /api/settings with forged Host = %d, want 403", st)
	}
}

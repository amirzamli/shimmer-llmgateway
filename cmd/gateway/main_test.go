package main

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/amirzamli/shimmer-llmgateway/internal/netutil"
)

func TestBindableListenPolicy(t *testing.T) {
	allowed := []string{
		"127.0.0.1:8787",
		"127.0.0.2:9000",
		"localhost:8787",
		"[::1]:8787",
		"100.64.0.1:8787",          // Tailscale CGNAT
		"100.100.100.100:8787",     // MagicDNS IP
		"[fd7a:115c:a1e0::1]:8787", // Tailscale ULA IPv6
	}
	for _, listen := range allowed {
		host, _, err := net.SplitHostPort(listen)
		if err != nil {
			t.Errorf("split %q: %v", listen, err)
			continue
		}
		if !netutil.IsBindableHost(host) {
			t.Errorf("listen %q (host %q) refused, want allowed", listen, host)
		}
	}

	refused := []string{
		":8787", // empty host is a wildcard bind
		"0.0.0.0:8787",
		"[::]:8787",
		"10.0.0.5:8787", // plain RFC 1918 LAN
		"192.168.1.10:8787",
		"203.0.113.9:8787", // public IP
		"example.com:8787", // resolves to public addresses
	}
	for _, listen := range refused {
		host, _, err := net.SplitHostPort(listen)
		if err != nil {
			continue // invalid addresses are refused at the entrypoint
		}
		if netutil.IsBindableHost(host) {
			t.Errorf("listen %q (host %q) allowed, want refused", listen, host)
		}
	}
}

func TestServeListeners(t *testing.T) {
	// Reserve two free ports, one per family.
	ports := make([]string, 2)
	for i, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("cannot listen on %s: %v", addr, err)
		}
		ports[i] = ln.Addr().String()
		ln.Close()
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	go serveListeners(ports, handler) // blocks; the goroutine keeps the servers up

	for _, port := range ports {
		waitHTTP(t, "http://"+port)
	}
}

// waitHTTP polls url until it returns 204 or the deadline passes.
func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never served", url)
}

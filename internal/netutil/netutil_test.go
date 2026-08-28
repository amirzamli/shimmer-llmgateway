package netutil

import (
	"net"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	loopback := []string{
		"", // missing RemoteAddr → local origin
		"localhost",
		"LOCALHOST",
		"127.0.0.1",
		"127.0.0.2",
		"127.255.255.254", // rest of the 127.0.0.0/8 range
		"::1",
		"0:0:0:0:0:0:0:1",
	}
	for _, host := range loopback {
		if !IsLoopbackHost(host) {
			t.Errorf("IsLoopbackHost(%q) = false, want true", host)
		}
	}

	remote := []string{
		"0.0.0.0",
		"8.8.8.8",
		"203.0.113.9",
		"10.0.0.5",
		"192.168.1.10",
		"2001:db8::1",
		"example.com", // not an IP and not localhost
		"[::1]",       // bracketed form is not a bare IP; SplitHostPort strips brackets
	}
	for _, host := range remote {
		if IsLoopbackHost(host) {
			t.Errorf("IsLoopbackHost(%q) = true, want false", host)
		}
	}
}

func TestIsPrivateAddress(t *testing.T) {
	private := []string{
		"127.0.0.1",
		"100.64.0.1",      // CGNAT start
		"100.100.100.100", // MagicDNS
		"100.127.255.255", // CGNAT end
		"10.0.0.5",
		"192.168.1.10",
		"172.16.0.1",
		"::1",
		"fd7a:115c:a1e0::1", // ULA (Tailscale IPv6)
	}
	for _, s := range private {
		if ip := net.ParseIP(s); !IsPrivateAddress(ip) {
			t.Errorf("IsPrivateAddress(%q) = false, want true", s)
		}
	}

	public := []string{
		"8.8.8.8",
		"203.0.113.9",
		"100.63.255.255", // just below CGNAT
		"100.128.0.1",    // just above CGNAT
		"2001:db8::1",
		"2606:4700:4700::1111",
	}
	for _, s := range public {
		if ip := net.ParseIP(s); IsPrivateAddress(ip) {
			t.Errorf("IsPrivateAddress(%q) = true, want false", s)
		}
	}
}

func TestIsBindableHost(t *testing.T) {
	bindable := []string{
		"localhost",
		"LOCALHOST",
		"127.0.0.1",
		"127.0.0.2",
		"::1",
		"100.64.0.1",      // CGNAT (Tailscale default)
		"100.100.100.100", // MagicDNS IP
		"fd7a:115c:a1e0::1",
	}
	for _, host := range bindable {
		if !IsBindableHost(host) {
			t.Errorf("IsBindableHost(%q) = false, want true", host)
		}
	}

	refused := []string{
		"", // wildcard: binds every interface
		"0.0.0.0",
		"::",
		"8.8.8.8",
		"203.0.113.9", // public IPv4
		"10.0.0.5",    // plain private LAN RFC1918: not loopback/CGNAT/ULA
		"192.168.1.10",
		"172.16.0.1",
		"2001:db8::1",
		"example.com", // resolves to public addresses
	}
	for _, host := range refused {
		if IsBindableHost(host) {
			t.Errorf("IsBindableHost(%q) = true, want false", host)
		}
	}
}

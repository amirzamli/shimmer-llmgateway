package netutil

import "testing"

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

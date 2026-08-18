package main

import "testing"

func TestCheckLoopbackListen(t *testing.T) {
	allowed := []string{
		"127.0.0.1:8787",
		"127.0.0.2:9000",
		"localhost:8787",
		"[::1]:8787",
	}
	for _, listen := range allowed {
		if err := checkLoopbackListen(listen); err != nil {
			t.Errorf("checkLoopbackListen(%q) = %v, want nil", listen, err)
		}
	}

	refused := []string{
		"0.0.0.0:8787",
		":8787", // empty host is a wildcard bind, not loopback
		"10.0.0.5:8787",
		"192.168.1.10:8787",
		"203.0.113.9:8787",
		"example.com:8787",
		"not-an-address",
	}
	for _, listen := range refused {
		if err := checkLoopbackListen(listen); err == nil {
			t.Errorf("checkLoopbackListen(%q) = nil, want error", listen)
		}
	}
}

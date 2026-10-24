package api

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{
		"localhost", "LOCALHOST", "127.0.0.1", "127.0.0.2", "127.255.255.255",
		"::1", "0:0:0:0:0:0:0:1", "::ffff:127.0.0.1",
	} {
		if !IsLoopbackHost(host) {
			t.Errorf("loopback host %q refused", host)
		}
	}
	for _, host := range []string{
		"", "0.0.0.0", "::", "10.0.0.5", "192.0.2.1", "2001:db8::1",
		"::ffff:10.0.0.5", "crux.example", "localhost.example", "localhost.",
		"127.0.0.1.example", "127.1", "2130706433", "[::1]", "localhost:7777",
	} {
		if IsLoopbackHost(host) {
			t.Errorf("non-loopback or malformed host %q accepted", host)
		}
	}
}

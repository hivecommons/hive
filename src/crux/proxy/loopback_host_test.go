package proxy

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"localhost":        true,
		"LOCALHOST:3000":   true,
		"127.0.0.1":        true,
		"127.0.0.1:8080":   true,
		"[::1]:3000":       true,
		"::1":              true,
		"[::1]":            true,
		"example.com:80":   false,
		"10.0.0.5:3000":    false,
		"":                 false,
		"localhost.evil.x": false,
	}
	for in, want := range cases {
		if got := isLoopbackHost(in); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", in, got, want)
		}
	}
}

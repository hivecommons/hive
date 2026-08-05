package proxy

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

// TestProxyEgressMarkDefault verifies the mark resolves to the documented
// default when the env var is unset, keeping the Go proxy and entrypoint in
// lockstep on the agreed value.
func TestProxyEgressMarkDefault(t *testing.T) {
	t.Setenv(proxyEgressMarkEnv, "")
	if got := proxyEgressMark(); got != defaultProxyEgressMark {
		t.Fatalf("proxyEgressMark() default = %#x, want %#x", got, defaultProxyEgressMark)
	}
}

// TestProxyEgressMarkEnvOverride verifies HIVE_PROXY_EGRESS_MARK overrides the
// default in both hex and decimal forms, and that a garbage value fails safe
// back to the default rather than disabling the exemption.
func TestProxyEgressMarkEnvOverride(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"hex", "0x2223", 0x2223},
		{"decimal", "8739", 8739},
		{"garbage falls back", "not-a-number", defaultProxyEgressMark},
		{"zero falls back", "0", defaultProxyEgressMark},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(proxyEgressMarkEnv, tc.env)
			if got := proxyEgressMark(); got != tc.want {
				t.Fatalf("proxyEgressMark(%q) = %#x, want %#x", tc.env, got, tc.want)
			}
		})
	}
}

// TestMarkDialerControlRuns exercises the mark dialer's Control hook against a
// real loopback listener. The Control func must run and set SO_MARK (a no-op on
// non-Linux) WITHOUT failing the dial — a returned error here would break every
// upstream connection the proxy makes. This is the default dial path used when
// copilotDial is unset.
func TestMarkDialerControlRuns(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Close()
	}()

	conn, err := markDialer(2*time.Second).Dial("tcp", ln.Addr().String())
	if err != nil {
		// Setting SO_MARK requires CAP_NET_ADMIN. Unprivileged CI on Linux will
		// see EPERM from the Control hook — that still proves the hook ran and
		// attempted the setsockopt (the whole point). Tolerate ONLY EPERM; any
		// other error is a real dialer regression. On non-Linux the hook no-ops,
		// so the dial simply succeeds.
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("SO_MARK requires CAP_NET_ADMIN; hook ran but got EPERM (expected unprivileged): %v", err)
		}
		t.Fatalf("markDialer Dial failed (Control hook must not break the dial): %v", err)
	}
	conn.Close()
}

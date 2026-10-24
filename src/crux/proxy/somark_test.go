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
// real loopback listener. The Control func must run and attempt SO_MARK (a no-op
// on non-Linux) WITHOUT failing the dial — a returned error here would break
// every upstream connection the proxy makes. This is the default dial path used
// when copilotDial is unset.
//
// CRITICALLY: the dial must SUCCEED even when SO_MARK fails with EPERM. The hive
// Go process runs as a non-root user (entrypoint drops from root to `dev` via
// gosu, which strips CAP_NET_ADMIN from the effective set), so setsockopt(SO_MARK)
// returns EPERM on every real deployment. Before this was fixed, the Control hook
// propagated that EPERM and aborted the dial — so every proxy upstream dial to
// GitHub failed with "operation not permitted" and agents could not authenticate
// (they got empty replies through the proxy). SO_MARK is a best-effort egress
// hint (the owner-UID iptables exemption already covers the proxy's uid on OKE),
// so a mark failure must NOT break the connection. This test now asserts success
// rather than skipping on EPERM — the skip previously masked the regression.
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
		// A SO_MARK EPERM (unprivileged, the common non-root case) must NOT reach
		// here: the Control hook is expected to swallow it and dial unmarked. If we
		// see EPERM, the regression is back.
		if errors.Is(err, syscall.EPERM) {
			t.Fatalf("markDialer aborted the dial on SO_MARK EPERM — the non-fatal-mark fix regressed: %v", err)
		}
		t.Fatalf("markDialer Dial failed (Control hook must not break the dial): %v", err)
	}
	conn.Close()
}

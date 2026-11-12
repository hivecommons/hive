package egressmark

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

// TestMarkDefaultAndEnvOverride verifies the mark resolves to the entrypoint
// default when unset, honours hex and decimal overrides, and fails safe back
// to the default on garbage or zero rather than disabling the exemption.
func TestMarkDefaultAndEnvOverride(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"unset", "", DefaultMark},
		{"hex", "0x2223", 0x2223},
		{"decimal", "8739", 8739},
		{"garbage falls back", "not-a-number", DefaultMark},
		{"zero falls back", "0", DefaultMark},
		{"negative falls back", "-5", DefaultMark},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(Env, tc.env)
			if got := Mark(); got != tc.want {
				t.Fatalf("Mark() with %s=%q = %#x, want %#x", Env, tc.env, got, tc.want)
			}
		})
	}
}

// TestDialerControlDoesNotBreakDial dials a loopback listener through the
// marked dialer. The Control hook must run and attempt SO_MARK without failing
// the dial: the hive runs without CAP_NET_ADMIN in its effective set in many
// deployments, so setsockopt(SO_MARK) returns EPERM there and a propagated
// error would break every self-upgrade / startup-probe / kubejob API call.
func TestDialerControlDoesNotBreakDial(t *testing.T) {
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

	d := Dialer(2 * time.Second)
	if d.Timeout != 2*time.Second {
		t.Fatalf("Dialer timeout = %v, want 2s", d.Timeout)
	}
	conn, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Fatalf("Dialer aborted the dial on SO_MARK EPERM — mark must be best-effort: %v", err)
		}
		t.Fatalf("Dialer Dial failed (Control hook must not break the dial): %v", err)
	}
	conn.Close()
}

// TestDialerZeroTimeoutLeavesDefault confirms a zero timeout is passed through
// unchanged so callers that rely on the http.Transport's own deadlines are not
// given a surprise dial timeout.
func TestDialerZeroTimeoutLeavesDefault(t *testing.T) {
	if d := Dialer(0); d.Timeout != 0 || d.Control == nil {
		t.Fatalf("Dialer(0) = timeout %v, control nil=%v; want 0 and a Control hook", d.Timeout, d.Control == nil)
	}
}

// TestWarnOnceIsIdempotent exercises the once-only warning path directly so a
// mark failure on every dial does not spam the log.
func TestWarnOnceIsIdempotent(t *testing.T) {
	warnOnce(DefaultMark, errors.New("first"))
	warnOnce(DefaultMark, errors.New("second"))
}

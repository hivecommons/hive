package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestClassifyDialError(t *testing.T) {
	timeoutErr := &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, dialErrOther},
		{"os timeout", timeoutErr, dialErrTimeout},
		{"context deadline", fmt.Errorf("dial: %w", context.DeadlineExceeded), dialErrTimeout},
		{"i/o timeout text", errors.New("dial tcp 140.82.112.6:443: i/o timeout"), dialErrTimeout},
		{"refused", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, dialErrRefused},
		{"reset", fmt.Errorf("read: %w", syscall.ECONNRESET), dialErrReset},
		{"unreachable", fmt.Errorf("dial: %w", syscall.ENETUNREACH), dialErrUnreachable},
		{"dns", &net.DNSError{Err: "no such host", Name: "api.github.com"}, dialErrDNS},
		{"tls", errors.New("tls: first record does not look like a TLS handshake"), dialErrTLS},
		{"eof", errors.New("EOF"), dialErrOther},
	}
	for _, tc := range cases {
		if got := classifyDialError(tc.err); got != tc.want {
			t.Errorf("%s: classifyDialError = %q, want %q", tc.name, got, tc.want)
		}
	}
}

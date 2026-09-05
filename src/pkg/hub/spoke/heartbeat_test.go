package spoke

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

type timeoutProbeError struct{}

func (timeoutProbeError) Error() string   { return "dial tcp 203.0.113.10:443: i/o timeout" }
func (timeoutProbeError) Timeout() bool   { return true }
func (timeoutProbeError) Temporary() bool { return true }

type deadlineIsProbeError struct{}

func (deadlineIsProbeError) Error() string { return "self probe deadline reached without net.Error" }
func (deadlineIsProbeError) Is(target error) bool {
	return target == context.DeadlineExceeded
}

func TestDashboardHostSupplementalCases(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{name: "host without scheme is not a URL host", rawURL: "hive.example.com", want: ""},
		{name: "whitespace only is empty", rawURL: "   ", want: ""},
		{name: "strips user info and port", rawURL: "https://operator@HIVE.EXAMPLE.COM:9443/dashboard", want: "hive.example.com"},
		{name: "ipv6 literal host", rawURL: "http://[2001:db8::1]:8080/api", want: "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dashboardHost(tt.rawURL); got != tt.want {
				t.Fatalf("dashboardHost(%q) = %q, want %q", tt.rawURL, got, tt.want)
			}
		})
	}
}

func TestTerseProbeErrorUnicodeBoundary(t *testing.T) {
	exact := strings.Repeat("界", 180)
	if got := terseProbeError(errors.New(exact)); got != exact {
		t.Fatalf("exactly 180 runes should be preserved; got %d runes", len([]rune(got)))
	}

	long := strings.Repeat("界", 181)
	got := terseProbeError(errors.New(long))
	runes := []rune(got)
	if len(runes) != 180 {
		t.Fatalf("truncated length = %d runes, want 180", len(runes))
	}
	if runes[179] != '…' {
		t.Fatalf("last rune = %q, want ellipsis", runes[179])
	}
	if prefix := string(runes[:179]); prefix != strings.Repeat("界", 179) {
		t.Fatalf("truncated prefix changed: got %q", prefix)
	}
}

func TestPublicURLSelfCheckStatusForErrorWrappedAndNetworkErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "wrapped context deadline is transient", err: fmt.Errorf("self probe: %w", context.DeadlineExceeded), want: PublicURLSelfCheckUnknown},
		{name: "deadline identity without net.Error is transient", err: deadlineIsProbeError{}, want: PublicURLSelfCheckUnknown},
		{name: "net timeout is transient", err: timeoutProbeError{}, want: PublicURLSelfCheckUnknown},
		{name: "dns port 53 timeout string is transient", err: errors.New("read udp 10.0.0.2:43123->10.0.0.10:53: i/o timeout"), want: PublicURLSelfCheckUnknown},
		{name: "certificate validation is a real listener failure", err: errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), want: PublicURLSelfCheckFail},
		{name: "connection reset is a real listener failure", err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, want: PublicURLSelfCheckFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := publicURLSelfCheckStatusForError(tt.err); got != tt.want {
				t.Fatalf("publicURLSelfCheckStatusForError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestGatedPublicURLSelfCheckResetsUnknownAndStoresStableCopy(t *testing.T) {
	failures := 2
	stable := &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 204, CheckedAt: "before"}
	unknown := PublicURLSelfCheck{Status: PublicURLSelfCheckUnknown, Error: "dns lookup deferred", CheckedAt: "now"}

	got := gatedPublicURLSelfCheck(unknown, &failures, &stable)
	if got != unknown {
		t.Fatalf("unknown result = %+v, want raw unknown %+v", got, unknown)
	}
	if failures != 0 {
		t.Fatalf("failures after unknown = %d, want 0", failures)
	}
	if stable == nil || stable.Status != PublicURLSelfCheckOK || stable.HTTPStatus != 204 {
		t.Fatalf("unknown should not discard last stable check, got %+v", stable)
	}

	ok := PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 200, CheckedAt: "ok"}
	got = gatedPublicURLSelfCheck(ok, &failures, &stable)
	if got != ok {
		t.Fatalf("ok result = %+v, want raw ok %+v", got, ok)
	}
	if stable == nil || *stable != ok {
		t.Fatalf("stable copy = %+v, want %+v", stable, ok)
	}
	ok.Status = PublicURLSelfCheckFail
	if stable.Status != PublicURLSelfCheckOK {
		t.Fatalf("stable aliases caller-owned raw value; got %+v", stable)
	}
}

func TestClonePublicURLSelfCheckCopiesAllFields(t *testing.T) {
	orig := &PublicURLSelfCheck{Status: PublicURLSelfCheckFail, CheckedAt: "2026-08-08T10:15:00Z", Error: "HTTP 502", HTTPStatus: 502}
	clone := clonePublicURLSelfCheck(orig)
	if clone == nil {
		t.Fatal("clone is nil")
	}
	if clone == orig {
		t.Fatal("clone should not alias original")
	}
	if *clone != *orig {
		t.Fatalf("clone = %+v, want %+v", clone, orig)
	}
	clone.Error = "changed"
	if orig.Error != "HTTP 502" {
		t.Fatalf("mutating clone changed original: %+v", orig)
	}
}

func TestCloneRouteExistenceCheckCopiesAllFields(t *testing.T) {
	orig := &RouteExistenceCheck{Status: RouteExistenceFound, CheckedAt: "2026-08-08T10:15:00Z", Host: "hive.example.com", Kind: "Ingress", Error: ""}
	clone := cloneRouteExistenceCheck(orig)
	if clone == nil {
		t.Fatal("clone is nil")
	}
	if clone == orig {
		t.Fatal("clone should not alias original")
	}
	if *clone != *orig {
		t.Fatalf("clone = %+v, want %+v", clone, orig)
	}
	clone.Host = "other.example.com"
	if orig.Host != "hive.example.com" {
		t.Fatalf("mutating clone changed original: %+v", orig)
	}
}

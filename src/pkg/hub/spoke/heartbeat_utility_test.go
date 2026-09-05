package spoke

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestDashboardHost(t *testing.T) {
	tests := []struct {
		rawURL string
		want   string
	}{
		{"https://my-hive.example.com/dashboard", "my-hive.example.com"},
		{"https://MY-HIVE.EXAMPLE.COM:8443/path", "my-hive.example.com"},
		{"http://localhost:9090", "localhost"},
		{"", ""},
		{"://invalid", ""},
		{"   https://padded.io   ", "padded.io"},
	}
	for _, tt := range tests {
		got := dashboardHost(tt.rawURL)
		if got != tt.want {
			t.Errorf("dashboardHost(%q) = %q, want %q", tt.rawURL, got, tt.want)
		}
	}
}

func TestPublicURLSelfCheckStatusForError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil error returns unknown", nil, PublicURLSelfCheckUnknown},
		{"dns error returns unknown", &net.DNSError{Err: "no such host", Name: "example.com"}, PublicURLSelfCheckUnknown},
		{"no such host string returns unknown", errors.New("dial tcp: lookup bad.host: no such host"), PublicURLSelfCheckUnknown},
		{"temporary failure returns unknown", errors.New("temporary failure in name resolution"), PublicURLSelfCheckUnknown},
		{"context deadline exceeded returns unknown", context.DeadlineExceeded, PublicURLSelfCheckUnknown},
		{"connection refused returns fail", errors.New("connect: connection refused"), PublicURLSelfCheckFail},
		{"generic error returns fail", errors.New("some random error"), PublicURLSelfCheckFail},
		{"server misbehaving returns unknown", errors.New("server misbehaving"), PublicURLSelfCheckUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := publicURLSelfCheckStatusForError(tt.err)
			if got != tt.want {
				t.Errorf("publicURLSelfCheckStatusForError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestTerseProbeError(t *testing.T) {
	t.Run("nil returns empty", func(t *testing.T) {
		if got := terseProbeError(nil); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("short message passes through", func(t *testing.T) {
		err := errors.New("connection refused")
		got := terseProbeError(err)
		if got != "connection refused" {
			t.Errorf("got %q, want 'connection refused'", got)
		}
	})

	t.Run("long message is truncated with ellipsis", func(t *testing.T) {
		// Build a message longer than 180 runes
		long := ""
		for i := 0; i < 200; i++ {
			long += "x"
		}
		err := errors.New(long)
		got := terseProbeError(err)
		runes := []rune(got)
		if len(runes) != 180 {
			t.Errorf("truncated length = %d runes, want 180", len(runes))
		}
		if runes[len(runes)-1] != '…' {
			t.Errorf("last rune = %q, want '…'", runes[len(runes)-1])
		}
	})
}

func TestClonePublicURLSelfCheck(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		if got := clonePublicURLSelfCheck(nil); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})

	t.Run("produces independent copy", func(t *testing.T) {
		original := &PublicURLSelfCheck{
			Status: PublicURLSelfCheckOK,
		}
		clone := clonePublicURLSelfCheck(original)
		if clone == original {
			t.Error("clone should be a different pointer")
		}
		if clone.Status != PublicURLSelfCheckOK {
			t.Errorf("clone.Status = %q, want %q", clone.Status, PublicURLSelfCheckOK)
		}
		clone.Status = PublicURLSelfCheckFail
		if original.Status != PublicURLSelfCheckOK {
			t.Error("mutating clone affected original")
		}
	})
}

func TestCloneRouteExistenceCheck(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		if got := cloneRouteExistenceCheck(nil); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})

	t.Run("produces independent copy", func(t *testing.T) {
		original := &RouteExistenceCheck{
			Status: "found",
		}
		clone := cloneRouteExistenceCheck(original)
		if clone == original {
			t.Error("clone should be a different pointer")
		}
		if clone.Status != "found" {
			t.Errorf("clone.Status = %q, want 'found'", clone.Status)
		}
	})
}

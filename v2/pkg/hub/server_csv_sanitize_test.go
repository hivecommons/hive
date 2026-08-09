package hub

import (
	"testing"
)

func TestFirstCSV(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"single value", "alpha", "alpha"},
		{"first of many", "alpha,beta,gamma", "alpha"},
		{"leading comma", ",beta,gamma", "beta"},
		{"only commas", ",,,", ""},
		{"empty string", "", ""},
		{"whitespace value", "  hello  , world", "hello"},
		{"all whitespace entries before real", "  ,  , real", "real"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstCSV(tt.input)
			if got != tt.want {
				t.Errorf("firstCSV(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestReplaceFirstCSV(t *testing.T) {
	tests := []struct {
		name    string
		s       string
		first   string
		want    string
	}{
		{"replace in single", "alpha", "REPLACED", "REPLACED"},
		{"replace first of many", "alpha,beta,gamma", "REPLACED", "REPLACED,beta,gamma"},
		{"leading comma preserves structure", ",beta,gamma", "NEW", ",NEW,gamma"},
		{"empty replacement removes first", "alpha,beta", "  ", "beta"},
		{"no values returns replacement", "", "NEW", "NEW"},
		{"all empty returns trimmed replacement", ",,,", "NEW", "NEW"},
		{"whitespace replacement on populated", "a,b", "  ", "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceFirstCSV(tt.s, tt.first)
			if got != tt.want {
				t.Errorf("replaceFirstCSV(%q, %q) = %q, want %q", tt.s, tt.first, got, tt.want)
			}
		})
	}
}

func TestSanitizePublicURLSelfCheck(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		if got := sanitizePublicURLSelfCheck(nil); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("valid ok status", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:     "ok",
			CheckedAt:  "2024-01-01T00:00:00Z",
			Error:      "",
			HTTPStatus: 200,
		}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != "ok" {
			t.Errorf("Status = %q, want %q", got.Status, "ok")
		}
		if got.HTTPStatus != 200 {
			t.Errorf("HTTPStatus = %d, want 200", got.HTTPStatus)
		}
	})

	t.Run("valid fail status with error", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:     "fail",
			CheckedAt:  "2024-01-01T00:00:00Z",
			Error:      "connection refused",
			HTTPStatus: 0,
		}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != "fail" {
			t.Errorf("Status = %q, want %q", got.Status, "fail")
		}
		if got.Error == "" {
			t.Error("expected Error to be preserved")
		}
	})

	t.Run("valid unknown status", func(t *testing.T) {
		in := &PublicURLSelfCheck{Status: "unknown"}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != "unknown" {
			t.Errorf("Status = %q, want %q", got.Status, "unknown")
		}
	})

	t.Run("invalid status returns nil", func(t *testing.T) {
		in := &PublicURLSelfCheck{Status: "bogus<script>"}
		got := sanitizePublicURLSelfCheck(in)
		if got != nil {
			t.Errorf("expected nil for invalid status, got %+v", got)
		}
	})

	t.Run("HTTPStatus clamped to 599", func(t *testing.T) {
		in := &PublicURLSelfCheck{Status: "ok", HTTPStatus: 9999}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.HTTPStatus != 599 {
			t.Errorf("HTTPStatus = %d, want 599 (clamped)", got.HTTPStatus)
		}
	})

	t.Run("negative HTTPStatus clamped to 0", func(t *testing.T) {
		in := &PublicURLSelfCheck{Status: "fail", HTTPStatus: -1}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.HTTPStatus != 0 {
			t.Errorf("HTTPStatus = %d, want 0 (clamped)", got.HTTPStatus)
		}
	})

	t.Run("hostile CheckedAt is sanitized", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:    "ok",
			CheckedAt: "<script>alert(1)</script>",
		}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		// sanitizeHeartbeatField strips angle brackets and special chars
		if got.CheckedAt == in.CheckedAt {
			t.Errorf("CheckedAt should be sanitized, got %q", got.CheckedAt)
		}
	})
}

func TestSanitizeRouteExistenceCheck(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		if got := sanitizeRouteExistenceCheck(nil); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("valid found status", func(t *testing.T) {
		in := &RouteExistenceCheck{
			Status:    "found",
			CheckedAt: "2024-01-01T00:00:00Z",
			Host:      "my-hive.example.com",
			Kind:      "Ingress",
		}
		got := sanitizeRouteExistenceCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != "found" {
			t.Errorf("Status = %q, want %q", got.Status, "found")
		}
		if got.Host != "my-hive.example.com" {
			t.Errorf("Host = %q, want %q", got.Host, "my-hive.example.com")
		}
		if got.Kind != "Ingress" {
			t.Errorf("Kind = %q, want %q", got.Kind, "Ingress")
		}
	})

	t.Run("valid missing status", func(t *testing.T) {
		in := &RouteExistenceCheck{Status: "missing", Error: "not found in namespace"}
		got := sanitizeRouteExistenceCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Status != "missing" {
			t.Errorf("Status = %q, want %q", got.Status, "missing")
		}
	})

	t.Run("valid unknown status", func(t *testing.T) {
		in := &RouteExistenceCheck{Status: "unknown"}
		got := sanitizeRouteExistenceCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
	})

	t.Run("invalid status returns nil", func(t *testing.T) {
		in := &RouteExistenceCheck{Status: "hacked"}
		got := sanitizeRouteExistenceCheck(in)
		if got != nil {
			t.Errorf("expected nil for invalid status, got %+v", got)
		}
	})

	t.Run("hostile fields are sanitized", func(t *testing.T) {
		in := &RouteExistenceCheck{
			Status: "found",
			Host:   "<img src=x onerror=alert(1)>",
			Kind:   "Route; DROP TABLE",
		}
		got := sanitizeRouteExistenceCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		// sanitizeHeartbeatField strips everything not [a-zA-Z0-9._-/]
		if got.Host == in.Host {
			t.Errorf("Host should be sanitized, got %q", got.Host)
		}
		if got.Kind == in.Kind {
			t.Errorf("Kind should be sanitized, got %q", got.Kind)
		}
	})
}

func TestIsAvailableRegistryEntry(t *testing.T) {
	tests := []struct {
		name string
		h    RegistryEntry
		want bool
	}{
		{
			name: "statusAvailable is available",
			h:    RegistryEntry{ProvStatus: statusAvailable},
			want: true,
		},
		{
			name: "placeholder org prefix is available",
			h:    RegistryEntry{Org: placeholderOrgPrefix + "oke-01-bb95"},
			want: true,
		},
		{
			name: "active hive not available",
			h:    RegistryEntry{ProvStatus: "active", Org: "my-real-org"},
			want: false,
		},
		{
			name: "empty entry not available",
			h:    RegistryEntry{},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAvailableRegistryEntry(tt.h)
			if got != tt.want {
				t.Errorf("isAvailableRegistryEntry(%+v) = %v, want %v", tt.h, got, tt.want)
			}
		})
	}
}

package hub

import (
	"strings"
	"testing"
)

func TestFirstCSV(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"whitespace only", "  ", ""},
		{"only commas", ",,,", ""},
		{"single value", "alpha", "alpha"},
		{"first of many", "alpha,beta", "alpha"},
		{"skips leading blanks", " , beta , gamma", "beta"},
		{"trims selected value", "  alpha  , beta", "alpha"},
		{"leading comma", ",alpha", "alpha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstCSV(tc.in); got != tc.want {
				t.Errorf("firstCSV(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReplaceFirstCSV(t *testing.T) {
	cases := []struct {
		name, s, first, want string
	}{
		{"replace single", "alpha", "NEW", "NEW"},
		{"replace first of many", "alpha,beta,gamma", "NEW", "NEW,beta,gamma"},
		{"replace after leading comma", ",beta,gamma", "NEW", ",NEW,gamma"},
		{"replace after blank entry", " , alpha , beta", "NEW", " ,NEW, beta"},
		{"empty replacement removes first", "alpha,beta", "", "beta"},
		{"blank replacement removes first", "alpha,beta", "  ", "beta"},
		{"all empty returns trimmed replacement", ",,", "NEW", "NEW"},
		{"empty input returns trimmed replacement", "", " NEW ", "NEW"},
		{"empty replacement with empty input", "", "", ""},
		{"empty replacement with all empty input", ",,", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := replaceFirstCSV(tc.s, tc.first); got != tc.want {
				t.Errorf("replaceFirstCSV(%q, %q) = %q, want %q", tc.s, tc.first, got, tc.want)
			}
		})
	}
}

func TestLooksLikeGitHubForgeHost(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"github.com", true},
		{"GitHub.COM", true},
		{"github.ibm.com", true},
		{"GITHUB.enterprise.org", true},
		{"gitlab.com", false},
		{"example.com", false},
		{"github.", false},
		{"github", false},
		{"github.x", true},
		{"", false},
		{"  ", false},
	}
	for _, tc := range cases {
		if got := looksLikeGitHubForgeHost(tc.in); got != tc.want {
			t.Errorf("looksLikeGitHubForgeHost(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeRepoRef(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{"ui", "ui"},
		{"z-aiops-unite/ui", "ui"},
		{"https://github.ibm.com/z-aiops-unite/ui", "ui"},
		{"http://github.com/org/repo", "repo"},
		{"https://github.com/org/repo.git", "repo"},
		{"https://github.com/org/repo/", "repo"},
		{"org/sub/deep/repo", "repo"},
	}
	for _, tc := range cases {
		if got := normalizeRepoRef(tc.in); got != tc.want {
			t.Errorf("normalizeRepoRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizePublicURLSelfCheck(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		if got := sanitizePublicURLSelfCheck(nil); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("valid statuses are preserved", func(t *testing.T) {
		for _, status := range []string{PublicURLSelfCheckOK, PublicURLSelfCheckFail, PublicURLSelfCheckUnknown} {
			got := sanitizePublicURLSelfCheck(&PublicURLSelfCheck{Status: status, HTTPStatus: 200})
			if got == nil || got.Status != status || got.HTTPStatus != 200 {
				t.Fatalf("status %q sanitized to %+v", status, got)
			}
		}
	})

	t.Run("invalid statuses are rejected", func(t *testing.T) {
		for _, status := range []string{"", "OK", "FAIL", "bogus", "<script>", "ok ok"} {
			if got := sanitizePublicURLSelfCheck(&PublicURLSelfCheck{Status: status}); got != nil {
				t.Errorf("status %q: expected nil, got %+v", status, got)
			}
		}
	})

	t.Run("HTTPStatus is clamped", func(t *testing.T) {
		tooHigh := sanitizePublicURLSelfCheck(&PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 999})
		if tooHigh == nil || tooHigh.HTTPStatus != 599 {
			t.Fatalf("HTTPStatus over range sanitized to %+v, want 599", tooHigh)
		}
		negative := sanitizePublicURLSelfCheck(&PublicURLSelfCheck{Status: PublicURLSelfCheckFail, HTTPStatus: -5})
		if negative == nil || negative.HTTPStatus != 0 {
			t.Fatalf("negative HTTPStatus sanitized to %+v, want 0", negative)
		}
	})

	t.Run("fields are sanitized and copied", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:     PublicURLSelfCheckFail,
			CheckedAt:  "2026-08-09T12:00:00Z<script>alert(1)</script>",
			Error:      "timeout <b>connecting</b> to\nlocalhost & retry",
			HTTPStatus: 503,
		}
		got := sanitizePublicURLSelfCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got == in {
			t.Fatal("returned pointer aliases input")
		}
		if got.CheckedAt != "2026-08-09T120000Zscriptalert1/script" {
			t.Errorf("CheckedAt = %q, want strict heartbeat-field sanitization", got.CheckedAt)
		}
		assertDoesNotContainAny(t, got.Error, "<", ">", "&", "\n")
		if !strings.Contains(got.Error, "timeout") || !strings.Contains(got.Error, "localhost") {
			t.Errorf("Error = %q, want readable sanitized prose", got.Error)
		}
	})
}

func TestSanitizeRouteExistenceCheck(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		if got := sanitizeRouteExistenceCheck(nil); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("valid statuses are preserved", func(t *testing.T) {
		for _, status := range []string{RouteExistenceFound, RouteExistenceMissing, RouteExistenceUnknown} {
			got := sanitizeRouteExistenceCheck(&RouteExistenceCheck{Status: status, Host: "hive.example.com", Kind: "Ingress"})
			if got == nil || got.Status != status {
				t.Fatalf("status %q sanitized to %+v", status, got)
			}
		}
	})

	t.Run("invalid statuses are rejected", func(t *testing.T) {
		for _, status := range []string{"", "FOUND", "Missing", "present", "<img>", "found found"} {
			if got := sanitizeRouteExistenceCheck(&RouteExistenceCheck{Status: status, Host: "x.com"}); got != nil {
				t.Errorf("status %q: expected nil, got %+v", status, got)
			}
		}
	})

	t.Run("fields are sanitized and copied", func(t *testing.T) {
		in := &RouteExistenceCheck{
			Status:    RouteExistenceFound,
			CheckedAt: "2026-08-09T12:00:00Z  extra",
			Host:      "hive.example.com<script>",
			Kind:      "Ingress; DROP TABLE",
			Error:     "no error <b>here</b> & retry",
		}
		got := sanitizeRouteExistenceCheck(in)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got == in {
			t.Fatal("returned pointer aliases input")
		}
		if got.CheckedAt != "2026-08-09T120000Zextra" {
			t.Errorf("CheckedAt = %q, want strict heartbeat-field sanitization", got.CheckedAt)
		}
		if got.Host != "hive.example.comscript" {
			t.Errorf("Host = %q, want strict heartbeat-field sanitization", got.Host)
		}
		if got.Kind != "IngressDROPTABLE" {
			t.Errorf("Kind = %q, want strict heartbeat-field sanitization", got.Kind)
		}
		assertDoesNotContainAny(t, got.Error, "<", ">", "&")
	})
}

func TestIsAvailableRegistryEntry(t *testing.T) {
	cases := []struct {
		name string
		h    RegistryEntry
		want bool
	}{
		{"statusAvailable", RegistryEntry{ProvStatus: statusAvailable}, true},
		{"placeholder org prefix", RegistryEntry{Org: placeholderOrgPrefix + "oke-01-bb95"}, true},
		{"active hive", RegistryEntry{ProvStatus: "active", Org: "my-real-org"}, false},
		{"empty entry", RegistryEntry{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAvailableRegistryEntry(tc.h); got != tc.want {
				t.Errorf("isAvailableRegistryEntry(%+v) = %v, want %v", tc.h, got, tc.want)
			}
		})
	}
}

func TestGheAPIURLForHost(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"github.com", ""},
		{"GITHUB.COM", ""},
		{"api.github.com", ""},
		{"github.ibm.com", "https://github.ibm.com/api/v3"},
		{"  GitHub.Enterprise.org  ", "https://github.enterprise.org/api/v3"},
	}
	for _, tc := range cases {
		if got := gheAPIURLForHost(tc.in); got != tc.want {
			t.Errorf("gheAPIURLForHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func assertDoesNotContainAny(t *testing.T, s string, forbidden ...string) {
	t.Helper()
	for _, bad := range forbidden {
		if strings.Contains(s, bad) {
			t.Errorf("%q still contains %q", s, bad)
		}
	}
}

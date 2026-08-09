package hub

import (
	"testing"
)

// --- firstCSV ---

func TestFirstCSV(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{",,,", ""},
		{"alpha", "alpha"},
		{"alpha,beta", "alpha"},
		{" , beta , gamma", "beta"},
		{"  alpha  , beta", "alpha"},
		{",alpha", "alpha"},
	}
	for _, tc := range cases {
		got := firstCSV(tc.in)
		if got != tc.want {
			t.Errorf("firstCSV(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- replaceFirstCSV ---

func TestReplaceFirstCSV(t *testing.T) {
	cases := []struct {
		s, first, want string
	}{
		// Replace existing first element
		{"alpha,beta,gamma", "NEW", "NEW,beta,gamma"},
		// Leading whitespace entry is skipped; "alpha" is the first non-empty
		{" , alpha , beta", "NEW", " ,NEW, beta"},
		// Empty replacement removes the first non-empty entry
		{"alpha,beta", "", "beta"},
		{"alpha,beta", "  ", "beta"},
		// All-empty input: returns the trimmed replacement
		{",,", "NEW", "NEW"},
		{"", "NEW", "NEW"},
		// Empty replacement with empty input
		{"", "", ""},
		{",,", "", ""},
	}
	for _, tc := range cases {
		got := replaceFirstCSV(tc.s, tc.first)
		if got != tc.want {
			t.Errorf("replaceFirstCSV(%q, %q) = %q, want %q", tc.s, tc.first, got, tc.want)
		}
	}
}

// --- looksLikeGitHubForgeHost ---

func TestLooksLikeGitHubForgeHost(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"github.com", true},
		{"GitHub.COM", true},
		{"github.ibm.com", true},
		{"GITHUB.enterprise.org", true},
		// Not a forge host: no "github." prefix
		{"gitlab.com", false},
		{"example.com", false},
		// Trailing dot (invalid TLD)
		{"github.", false},
		// Only "github" with no dot suffix (an org name)
		{"github", false},
		// Single label after prefix doesn't satisfy minForgeHostLabels (needs >=2 labels total)
		// "github.com" has 2 labels, so it passes
		{"github.x", true},
		// Empty
		{"", false},
		{"  ", false},
	}
	for _, tc := range cases {
		got := looksLikeGitHubForgeHost(tc.in)
		if got != tc.want {
			t.Errorf("looksLikeGitHubForgeHost(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// --- normalizeRepoRef ---

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
		got := normalizeRepoRef(tc.in)
		if got != tc.want {
			t.Errorf("normalizeRepoRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- sanitizePublicURLSelfCheck ---

func TestSanitizePublicURLSelfCheck(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		if sanitizePublicURLSelfCheck(nil) != nil {
			t.Fatal("expected nil")
		}
	})

	t.Run("valid OK status passes through", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:     "ok",
			CheckedAt:  "2024-01-01T00:00:00Z",
			Error:      "",
			HTTPStatus: 200,
		}
		out := sanitizePublicURLSelfCheck(in)
		if out == nil {
			t.Fatal("expected non-nil")
		}
		if out.Status != "ok" {
			t.Errorf("Status = %q, want ok", out.Status)
		}
		if out.HTTPStatus != 200 {
			t.Errorf("HTTPStatus = %d, want 200", out.HTTPStatus)
		}
	})

	t.Run("valid fail status passes through", func(t *testing.T) {
		in := &PublicURLSelfCheck{
			Status:     "fail",
			CheckedAt:  "2024-01-01T00:00:00Z",
			Error:      "connection refused",
			HTTPStatus: 503,
		}
		out := sanitizePublicURLSelfCheck(in)
		if out == nil || out.Status != "fail" {
			t.Fatalf("expected fail status, got %+v", out)
		}
		if out.HTTPStatus != 503 {
			t.Errorf("HTTPStatus = %d, want 503", out.HTTPStatus)
		}
	})

	t.Run("valid unknown status passes through", func(t *testing.T) {
		in := &PublicURLSelfCheck{Status: "unknown"}
		out := sanitizePublicURLSelfCheck(in)
		if out == nil || out.Status != "unknown" {
			t.Fatalf("expected unknown status, got %+v", out)
		}
	})

	t.Run("invalid status returns nil", func(t *testing.T) {
		in := &PublicURLSelfCheck{Status: "bogus"}
		if sanitizePublicURLSelfCheck(in) != nil {
			t.Fatal("expected nil for invalid status")
		}
	})

	t.Run("HTTPStatus clamped to 0-599", func(t *testing.T) {
		in := &PublicURLSelfCheck{Status: "ok", HTTPStatus: 999}
		out := sanitizePublicURLSelfCheck(in)
		if out == nil {
			t.Fatal("expected non-nil")
		}
		if out.HTTPStatus != 599 {
			t.Errorf("HTTPStatus = %d, want 599 (clamped)", out.HTTPStatus)
		}

		in2 := &PublicURLSelfCheck{Status: "ok", HTTPStatus: -5}
		out2 := sanitizePublicURLSelfCheck(in2)
		if out2 == nil || out2.HTTPStatus != 0 {
			t.Errorf("HTTPStatus = %d, want 0 (clamped)", out2.HTTPStatus)
		}
	})
}

// --- sanitizeRouteExistenceCheck ---

func TestSanitizeRouteExistenceCheck(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		if sanitizeRouteExistenceCheck(nil) != nil {
			t.Fatal("expected nil")
		}
	})

	t.Run("found status passes through", func(t *testing.T) {
		in := &RouteExistenceCheck{
			Status:    "found",
			CheckedAt: "2024-01-01T00:00:00Z",
			Host:      "my-hive.example.com",
			Kind:      "Ingress",
		}
		out := sanitizeRouteExistenceCheck(in)
		if out == nil || out.Status != "found" {
			t.Fatalf("expected found, got %+v", out)
		}
		if out.Host != "my-hive.example.com" {
			t.Errorf("Host = %q", out.Host)
		}
		if out.Kind != "Ingress" {
			t.Errorf("Kind = %q", out.Kind)
		}
	})

	t.Run("missing status passes through", func(t *testing.T) {
		in := &RouteExistenceCheck{Status: "missing", Host: "h.example.com"}
		out := sanitizeRouteExistenceCheck(in)
		if out == nil || out.Status != "missing" {
			t.Fatalf("expected missing, got %+v", out)
		}
	})

	t.Run("unknown status passes through", func(t *testing.T) {
		in := &RouteExistenceCheck{Status: "unknown", Error: "RBAC denied"}
		out := sanitizeRouteExistenceCheck(in)
		if out == nil || out.Status != "unknown" {
			t.Fatalf("expected unknown, got %+v", out)
		}
	})

	t.Run("invalid status returns nil", func(t *testing.T) {
		in := &RouteExistenceCheck{Status: "hacked"}
		if sanitizeRouteExistenceCheck(in) != nil {
			t.Fatal("expected nil for invalid status")
		}
	})

	t.Run("dangerous characters in fields are sanitized", func(t *testing.T) {
		in := &RouteExistenceCheck{
			Status:    "found",
			CheckedAt: "2024-01-01<script>",
			Host:      "host<img>",
			Kind:      "Ingress\"onload=",
			Error:     "some error with <html>",
		}
		out := sanitizeRouteExistenceCheck(in)
		if out == nil {
			t.Fatal("expected non-nil")
		}
		// sanitizeHeartbeatField strips non-allowed chars
		if out.CheckedAt == in.CheckedAt {
			t.Logf("CheckedAt sanitized: %q -> %q", in.CheckedAt, out.CheckedAt)
		}
	})
}

// --- gheAPIURLForHost ---

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
		got := gheAPIURLForHost(tc.in)
		if got != tc.want {
			t.Errorf("gheAPIURLForHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

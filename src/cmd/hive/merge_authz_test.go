package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestTrustedMergerFunc covers the merger-tier gate handed to the auto-merge
// sweep (SetMergerAuthorizer): only allowlisted users at merger tier or above
// may queue other people's PRs. Everything ambiguous fails closed.
func TestTrustedMergerFunc(t *testing.T) {
	cfg := &config.Config{}
	cfg.Dashboard.AuthorizedUsers = []string{
		"olivia:owner",
		"mia:merger",
		"walt:read-write",
		"rhea:read",
	}
	authz := trustedMergerFunc(cfg)

	tests := []struct {
		name  string
		login string
		want  bool
	}{
		{"merger tier allowed", "mia", true},
		{"owner tier allowed (at least merger)", "olivia", true},
		{"read-write tier denied", "walt", false},
		{"read tier denied", "rhea", false},
		{"unknown login denied", "mallory", false},
		{"empty login denied", "", false},
		{"whitespace login denied", "   ", false},
		{"case-insensitive match", "MIA", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authz(tt.login); got != tt.want {
				t.Errorf("trustedMergerFunc(cfg)(%q) = %v, want %v", tt.login, got, tt.want)
			}
		})
	}

	t.Run("nil config denies everyone", func(t *testing.T) {
		if trustedMergerFunc(nil)("olivia") {
			t.Error("nil config must fail closed")
		}
	})
}

// TestBindMergeAuthz covers the merge-relay authorizer composition: the inner
// agent/UID check runs first, then the TOCTOU guard (a pinned head SHA is
// mandatory), then the merge-eligible list binding at that exact SHA. Each
// layer fails closed independently.
func TestBindMergeAuthz(t *testing.T) {
	const goodSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const movedSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	writeEligibleFixture(t, `{
		"generated_at": "2026-08-24T00:00:00Z",
		"merge_eligible": [
			{"number": 42, "repo": "hivecommons/hive", "head_sha": "`+goodSHA+`"}
		]
	}`)

	allow := func(agent string, fileUID int) error { return nil }
	innerErr := errors.New("uid mismatch")
	deny := func(agent string, fileUID int) error { return innerErr }

	t.Run("inner denial short-circuits", func(t *testing.T) {
		err := bindMergeAuthz(deny)("guide", 1001, "hivecommons/hive", 42, goodSHA)
		if !errors.Is(err, innerErr) {
			t.Fatalf("expected inner error to propagate, got %v", err)
		}
	})

	t.Run("empty expected SHA denied (TOCTOU guard)", func(t *testing.T) {
		err := bindMergeAuthz(allow)("guide", 1001, "hivecommons/hive", 42, "")
		if err == nil {
			t.Fatal("unpinned head must be refused")
		}
		if !strings.Contains(err.Error(), "TOCTOU") {
			t.Errorf("error should name the TOCTOU guard, got: %v", err)
		}
	})

	t.Run("whitespace expected SHA denied", func(t *testing.T) {
		if err := bindMergeAuthz(allow)("guide", 1001, "hivecommons/hive", 42, "   "); err == nil {
			t.Fatal("whitespace SHA must be refused")
		}
	})

	t.Run("target not in eligible list denied", func(t *testing.T) {
		if err := bindMergeAuthz(allow)("guide", 1001, "hivecommons/hive", 999, goodSHA); err == nil {
			t.Fatal("a PR outside the merge-eligible list must be refused")
		}
	})

	t.Run("moved head denied (SHA mismatch)", func(t *testing.T) {
		if err := bindMergeAuthz(allow)("guide", 1001, "hivecommons/hive", 42, movedSHA); err == nil {
			t.Fatal("an eligible PR at a moved head must be refused")
		}
	})

	t.Run("eligible target at reviewed SHA allowed", func(t *testing.T) {
		if err := bindMergeAuthz(allow)("guide", 1001, "hivecommons/hive", 42, goodSHA); err != nil {
			t.Fatalf("governor-approved PR at its reviewed head must pass, got: %v", err)
		}
	})
}

// TestHiveIdentity covers which PR authors count as "this hive": the
// configured ai_author plus the "<app-slug>[bot]" login derived from the
// GitHub config (falling back to the public default slug).
func TestHiveIdentity(t *testing.T) {
	t.Run("ai_author and explicit app slug", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Project.AIAuthor = "hive-bot"
		cfg.GitHub.AppSlug = "my-enterprise-hive"
		id := hiveIdentity(cfg)
		if id.AIAuthor != "hive-bot" {
			t.Errorf("AIAuthor = %q, want %q", id.AIAuthor, "hive-bot")
		}
		if id.AppLogin != "my-enterprise-hive[bot]" {
			t.Errorf("AppLogin = %q, want %q", id.AppLogin, "my-enterprise-hive[bot]")
		}
	})

	t.Run("default slug when none configured", func(t *testing.T) {
		id := hiveIdentity(&config.Config{})
		want := config.DefaultGitHubAppSlug + "[bot]"
		if id.AppLogin != want {
			t.Errorf("AppLogin = %q, want default %q", id.AppLogin, want)
		}
		if id.AIAuthor != "" {
			t.Errorf("AIAuthor = %q, want empty", id.AIAuthor)
		}
	})
}

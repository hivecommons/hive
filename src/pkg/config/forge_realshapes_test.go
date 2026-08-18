package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run REAL spoke hive.yaml text through the REAL loader
// (config.Load), rather than constructing GitHubConfig values by hand.
//
// WHY THAT MATTERS HERE. A previous identity check shipped green because its
// test data was crafted to match the check: all three of IdentitySetIssues'
// GHE markers derive from fields that are empty across most of the fleet, so
// the rule was unreachable on real data while passing on fixtures. Anything
// asserted about identity must therefore be asserted against the YAML shapes
// that actually exist on spokes, parsed the way a spoke parses them — including
// the yaml key spellings, which a hand-built struct silently bypasses.
//
// The shapes below are the fleet's real ones: an implicit-empty public spoke
// (~41 of ~51 spokes), a fully-populated GHE spoke, a pooled placeholder
// carrying a GHE api_url with base_url absent, and the damaged shape from
// 2026-07-31.

func loadSpokeYAML(t *testing.T, body string) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	// A minimal but VALID hive.yaml: the loader applies defaults and validation,
	// so the github block is exercised in its real context, not in isolation.
	full := "project:\n  name: test-hive\n  org: acme\n  repos:\n    - acme/widgets\n" +
		"agents:\n  scanner:\n    enabled: true\n    backend: claude\n" + body
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("real spoke config failed to load: %v\n---\n%s", err, full)
	}
	return cfg
}

// TestRealSpokeShapesResolveTheirForge walks the four shapes that actually
// exist on disk across the fleet and asserts each resolves to the right forge
// through the real YAML path.
func TestRealSpokeShapesResolveTheirForge(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		wantForge string
		wantGHE   bool
	}{
		{
			// The majority shape: no api_url, no base_url, no forge. This is the
			// implicit-empty default that disguised the incident, and the single
			// most important shape to keep working untouched.
			name: "implicit public spoke (fleet majority)",
			yaml: "github:\n  app_id: 3568013\n  installation_id: 149922991\n" +
				"  key_file: /secrets/gh-app-key.pem\n",
			wantForge: ForgePublic,
		},
		{
			name: "fully populated GHE spoke",
			yaml: "github:\n  app_id: 5686\n  installation_id: 12345\n" +
				"  app_slug: kubestellar-hive-ghe\n" +
				"  api_url: https://github.ibm.com/api/v3\n" +
				"  base_url: https://github.ibm.com\n",
			wantForge: ForgeGHEIBM,
			wantGHE:   true,
		},
		{
			// Pooled placeholders legitimately carry a GHE api_url with base_url
			// absent — ResolvedBaseURL/HostLabel document exactly this shape.
			name: "pooled GHE placeholder, base_url absent",
			yaml: "github:\n  app_id: 999999999\n" +
				"  api_url: https://github.ibm.com/api/v3\n",
			wantForge: ForgeGHEIBM,
			wantGHE:   true,
		},
		{
			// Token-only, no App at all — the in-repo deploy/hive.yaml shape.
			name:      "token-only spoke (deploy/hive.yaml shape)",
			yaml:      "github:\n  token: ghp_exampletoken\n",
			wantForge: ForgePublic,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadSpokeYAML(t, tc.yaml)
			if got := cfg.GitHub.Forge(); got != tc.wantForge {
				t.Errorf("Forge() = %q, want %q", got, tc.wantForge)
			}
			if got := cfg.GitHub.IsGHE(); got != tc.wantGHE {
				t.Errorf("IsGHE() = %v, want %v", got, tc.wantGHE)
			}
			// A spoke with no explicit `forge:` must NEVER be reported as
			// mismatched — the forge is inferred from these very fields, so they
			// agree by construction. If this fires, the whole un-backfilled fleet
			// would light up as broken.
			if issues := cfg.GitHub.ForgeIdentityMismatches(); len(issues) != 0 {
				t.Errorf("un-backfilled real spoke shape reported as mismatched: %v", issues)
			}
		})
	}
}

// TestRealSpokeYAMLAcceptsForgeKey proves the new field is wired to the yaml key
// `forge` through the real loader. A struct literal cannot catch a wrong or
// missing yaml tag; this can.
func TestRealSpokeYAMLAcceptsForgeKey(t *testing.T) {
	cfg := loadSpokeYAML(t, "github:\n  forge: github.ibm.com\n  installation_id: 12345\n")
	if got := cfg.GitHub.Forge_; got != ForgeGHEIBM {
		t.Fatalf("the yaml key `forge` did not populate the field: got %q", got)
	}
	if got := cfg.GitHub.Forge(); got != ForgeGHEIBM {
		t.Errorf("Forge() = %q, want %q", got, ForgeGHEIBM)
	}
	// Derivation from `forge:` ALONE — the end state this design is aiming at,
	// where a spoke names one field and the rest follow.
	if got := cfg.GitHub.ResolvedAppID(); got != GHEIBMAppID {
		t.Errorf("derived app_id = %d, want %d", got, GHEIBMAppID)
	}
	if got := cfg.GitHub.ResolvedAppSlug(); got != GHEIBMAppSlug {
		t.Errorf("derived app_slug = %q, want %q", got, GHEIBMAppSlug)
	}

	// And a public spoke declaring `forge: github.com` stays byte-identical to
	// the implicit-empty spoke it replaces.
	// Both carry the real public app_id, which is the actual fleet shape: the
	// ONLY difference between them is the added `forge: github.com` line.
	pub := loadSpokeYAML(t, "github:\n  forge: github.com\n  app_id: 3568013\n  installation_id: 1\n")
	implicit := loadSpokeYAML(t, "github:\n  app_id: 3568013\n  installation_id: 1\n")
	if pub.GitHub.ResolvedAPIURL() != implicit.GitHub.ResolvedAPIURL() ||
		pub.GitHub.ResolvedBaseURL() != implicit.GitHub.ResolvedBaseURL() ||
		pub.GitHub.ResolvedAppSlug() != implicit.GitHub.ResolvedAppSlug() {
		t.Error("declaring `forge: github.com` changed a healthy public spoke's resolved identity — the migration must be a no-op")
	}
}

// TestRealDamagedSpokeShapeIsCaught is the incident regression asserted through
// the REAL loader: the exact YAML the seven hives ended up with after the
// clusters.json push, on a hive whose election was github.com.
func TestRealDamagedSpokeShapeIsCaught(t *testing.T) {
	// This is what was on disk: the GHE app_id and slug arrived, api_url and
	// base_url did not. The hive's own election (hub meta.json github_host:
	// github.com) is expressed as `forge: github.com`.
	cfg := loadSpokeYAML(t, "github:\n  forge: github.com\n"+
		"  app_id: 5686\n"+
		"  app_slug: kubestellar-hive-ghe\n"+
		"  installation_id: 149922991\n"+
		"  key_file: /secrets/gh-app-key.pem\n")

	issues := cfg.GitHub.ForgeIdentityMismatches()
	if len(issues) == 0 {
		t.Fatal("the real damaged spoke shape was ACCEPTED — this is the exact 2026-07-31 regression")
	}
	joined := strings.Join(issues, " | ")
	for _, want := range []string{"app_id", "app_slug"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the report must name %s, got: %s", want, joined)
		}
	}

	// The user-visible half of the incident: BotLogin drives EffectiveAIAuthor,
	// which becomes PROJECT_AI_AUTHOR and made agents author as
	// kubestellar-hive-ghe[bot] on PUBLIC repos. Pin that this shape is exactly
	// what produces the wrong bot, so the connection is not lost.
	if got := cfg.GitHub.BotLogin(); got != "kubestellar-hive-ghe[bot]" {
		t.Errorf("BotLogin() = %q; the damaged shape is expected to still yield the GHE bot "+
			"(that is the symptom) — if this changed, re-check EffectiveAIAuthor", got)
	}
	// installation_id is per-forge AND per-org and is never derived. It must
	// survive untouched: all seven affected IDs were verified intact.
	if cfg.GitHub.InstallationID != 149922991 {
		t.Errorf("installation_id = %d, want it preserved unchanged", cfg.GitHub.InstallationID)
	}
}

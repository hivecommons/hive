package config

import "testing"

// Regression test for the fleet-stats collector never starting fleet-wide.
//
// Observed live: every hosted spoke logged
//
//	"fleet stats collector disabled: author or org is empty" author=""
//
// even though the hub registry showed a populated aiAuthorEffective of
// "kubestellar-hive[bot]". The collector was reading the RAW Project.AIAuthor
// field, which App-authored hives deliberately leave empty (they derive the
// bot login from the installed App so App-bot mode survives restarts). With
// author empty, Start() returned before making a single GitHub call — so this
// was never a rate limit, a 403, or a retry problem. Nothing was ever tried.
//
// The PAT fallback could not rescue it either: those hives authenticate as a
// GitHub App and carry github.token == "".
//
// This pins EffectiveAIAuthor() as the identity the collector must use.
func TestEffectiveAIAuthor_AppBotHiveWithEmptyAIAuthor(t *testing.T) {
	// Exactly the shape of the live kubestellar/console spoke: App installed,
	// no PAT, ai_author left empty.
	cfg := &Config{}
	cfg.Project.Org = "kubestellar"
	cfg.Project.AIAuthor = ""
	cfg.GitHub.Token = ""
	cfg.GitHub.AppID = 3568013
	cfg.GitHub.InstallationID = 128685788
	cfg.GitHub.AppSlug = "kubestellar-hive"

	got := cfg.EffectiveAIAuthor()
	if got == "" {
		t.Fatal("EffectiveAIAuthor() = \"\" for an App-authored hive; the fleet-stats " +
			"collector would be disabled and this hive would never contribute")
	}
	if want := "kubestellar-hive[bot]"; got != want {
		t.Errorf("EffectiveAIAuthor() = %q, want %q", got, want)
	}
	// The raw field is what the buggy code read — assert it really is empty, so
	// this test would have failed against the old implementation.
	if cfg.Project.AIAuthor != "" {
		t.Fatal("test setup wrong: Project.AIAuthor must be empty to reproduce")
	}
}

// An App-less hive must still resolve to no author, so we never fabricate a
// phantom "<slug>[bot]" that authored nothing.
func TestEffectiveAIAuthor_AppLessHiveStaysEmpty(t *testing.T) {
	cfg := &Config{}
	cfg.Project.Org = "someorg"
	cfg.GitHub.AppID = 3568013
	cfg.GitHub.InstallationID = 0 // never installed
	cfg.GitHub.AppSlug = "kubestellar-hive"

	if got := cfg.EffectiveAIAuthor(); got != "" {
		t.Errorf("EffectiveAIAuthor() = %q, want \"\" for an uninstalled App", got)
	}
}

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pkg/config sits right on the coverage gate's 90% default floor (89.9%
// measured on v5 HEAD), so any refactor that trims a covered line can fail
// v2-tests with no behavior change. These tests cover the pure accessors that
// were at 0% — theme wrappers, ACMM repo policy, contribute toggles, and the
// gateway key digest — to restore headroom above the floor.

// APIKeySHA256 must never leak the key: an empty key yields an empty digest
// (not the hash of ""), and a real key yields the exact lowercase hex SHA-256
// the gateway will compare against.
func TestAPIKeySHA256FloorMargin(t *testing.T) {
	if got := APIKeySHA256(""); got != "" {
		t.Errorf("APIKeySHA256(\"\") = %q, want empty", got)
	}
	key := "sk-hive-test-key"
	want := sha256.Sum256([]byte(key))
	if got := APIKeySHA256(key); got != hex.EncodeToString(want[:]) {
		t.Errorf("APIKeySHA256(%q) = %q, want %q", key, got, hex.EncodeToString(want[:]))
	}
	if got := APIKeySHA256(key); got != strings.ToLower(got) {
		t.Errorf("digest %q is not lowercase", got)
	}
}

// Bot and Dependabot filtering both default ON when unset (nil), honor an
// explicit false, and honor an explicit true.
func TestGitHubActivityFilterDefaults(t *testing.T) {
	off, on := false, true
	cases := []struct {
		name string
		g    GitHubActivityConfig
		bots bool
		dep  bool
	}{
		{"unset defaults on", GitHubActivityConfig{}, true, true},
		{"explicit off", GitHubActivityConfig{FilterBots: &off, FilterDependabot: &off}, false, false},
		{"explicit on", GitHubActivityConfig{FilterBots: &on, FilterDependabot: &on}, true, true},
	}
	for _, tc := range cases {
		if got := tc.g.BotsFiltered(); got != tc.bots {
			t.Errorf("%s: BotsFiltered() = %v, want %v", tc.name, got, tc.bots)
		}
		if got := tc.g.DependabotFiltered(); got != tc.dep {
			t.Errorf("%s: DependabotFiltered() = %v, want %v", tc.name, got, tc.dep)
		}
	}
}

// The already-done auto-close toggle is the opposite polarity of the cooldown
// toggle: unset means DISABLED (default action stays comment + label).
func TestIsContributeCloseAlreadyDone(t *testing.T) {
	on, off := true, false
	if (HubConfig{}).IsContributeCloseAlreadyDone() {
		t.Error("unset ContributeCloseAlreadyDone must default to false")
	}
	if !(HubConfig{ContributeCloseAlreadyDone: &on}).IsContributeCloseAlreadyDone() {
		t.Error("explicit true not honored")
	}
	if (HubConfig{ContributeCloseAlreadyDone: &off}).IsContributeCloseAlreadyDone() {
		t.Error("explicit false not honored")
	}
}

// The already-done hold falls back to the unverified hold, then the default,
// and is clamped to [min, max] days.
func TestContributeAlreadyDoneHoldDaysOrDefault(t *testing.T) {
	cases := []struct {
		name       string
		hold, unvf int
		want       int
	}{
		{"both unset -> default", 0, 0, contributeAlreadyDoneDefaultDays},
		{"falls back to unverified", 0, 14, 14},
		{"hold wins over unverified", 7, 14, 7},
		{"clamped to min", -3, 0, contributeAlreadyDoneDefaultDays},
		{"below min clamps up", 0, -1, contributeAlreadyDoneDefaultDays},
		{"above max clamps down", 4000, 0, contributeAlreadyDoneMaxDays},
	}
	for _, tc := range cases {
		h := HubConfig{
			ContributeAlreadyDoneHoldDays:           tc.hold,
			ContributeAlreadyDoneUnverifiedHoldDays: tc.unvf,
		}
		if got := h.ContributeAlreadyDoneHoldDaysOrDefault(); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// EffectiveMaxLevel resolves the autonomy ceiling: unset means the global max,
// the hive ceiling caps it, and the result is clamped to [Min, Max]ACMMLevel.
func TestAutonomyEffectiveMaxLevel(t *testing.T) {
	cases := []struct {
		name         string
		max, ceiling int
		want         int
	}{
		{"unset means global max", 0, 0, MaxACMMLevel},
		{"ceiling caps", 0, 4, 4},
		{"explicit below ceiling", 3, 5, 3},
		{"explicit above ceiling capped", 6, 4, 4},
		{"never above global max", 99, 0, MaxACMMLevel},
		{"never below global min", -2, -2, MaxACMMLevel},
		{"ceiling below min clamps up", 5, 0, 5},
	}
	for _, tc := range cases {
		a := AutonomyConfig{MaxLevel: tc.max}
		if got := a.EffectiveMaxLevel(tc.ceiling); got != tc.want {
			t.Errorf("%s: EffectiveMaxLevel(%d) with MaxLevel=%d = %d, want %d",
				tc.name, tc.ceiling, tc.max, got, tc.want)
		}
	}
}

// OnDemandIsOperatorOwned fires only for the operator owner stamp.
func TestOnDemandIsOperatorOwned(t *testing.T) {
	if (AgentConfig{}).OnDemandIsOperatorOwned() {
		t.Error("unset owner must not read as operator-owned")
	}
	if !(AgentConfig{OnDemandOwner: FieldOwnerOperator}).OnDemandIsOperatorOwned() {
		t.Error("operator owner stamp not honored")
	}
}

// EffectiveFallback: empty means pinned; the two named policies and any other
// non-empty string pass through trimmed.
func TestReviewModelsEffectiveFallback(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ReviewModelsFallbackPinned},
		{"   ", ReviewModelsFallbackPinned},
		{ReviewModelsFallbackSkip, ReviewModelsFallbackSkip},
		{ReviewModelsFallbackRequiresHuman, ReviewModelsFallbackRequiresHuman},
		{"  gpt-fallback  ", "gpt-fallback"},
	}
	for _, tc := range cases {
		if got := (ReviewModelsConfig{Fallback: tc.in}).EffectiveFallback(); got != tc.want {
			t.Errorf("EffectiveFallback(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func acmmTestConfig(t *testing.T, hiveLevel int, policies ...RepoPolicy) *Config {
	t.Helper()
	dir := t.TempDir()
	hermeticPersistPaths(t, dir)
	cfg := &Config{
		SourcePath: filepath.Join(dir, "hive.yaml"),
		Project:    ProjectConfig{Org: "acme", Repos: []string{"console"}, RepoPolicies: policies},
		GitHub:     GitHubConfig{AppID: 3568013},
		Agents:     map[string]AgentConfig{"scanner": {Backend: "claude"}},
		Data:       DataConfig{AgentsDir: t.TempDir()},
	}
	if hiveLevel > 0 {
		cfg.ACMMLevel = &hiveLevel
	}
	return cfg
}

// A repo policy may only LOWER the hive-wide ACMM level, never raise it, and
// with no hive level set the answer stays zero regardless of policy.
func TestEffectiveACMMLevelForRepo(t *testing.T) {
	lower, higher := 2, 6
	cfg := acmmTestConfig(t, 4,
		RepoPolicy{Repo: "console", ACMMLevel: &lower},
		RepoPolicy{Repo: "dashboard", ACMMLevel: &higher},
	)
	if got := cfg.EffectiveACMMLevelForRepo("console"); got != 2 {
		t.Errorf("policy below hive level: got %d, want 2", got)
	}
	if got := cfg.EffectiveACMMLevelForRepo("dashboard"); got != 4 {
		t.Errorf("policy above hive level must not raise it: got %d, want 4", got)
	}
	if got := cfg.EffectiveACMMLevelForRepo("unlisted"); got != 4 {
		t.Errorf("repo without policy: got %d, want hive level 4", got)
	}
	unleveled := acmmTestConfig(t, 0, RepoPolicy{Repo: "console", ACMMLevel: &lower})
	if got := unleveled.EffectiveACMMLevelForRepo("console"); got != 0 {
		t.Errorf("no hive level: got %d, want 0", got)
	}
}

// Pinning a repo's ACMM level persists to disk, is idempotent, and validates
// its input; RepoACMMPinned reads the same state back.
func TestSetRepoACMMPinnedAndSave(t *testing.T) {
	cfg := acmmTestConfig(t, 4)

	if cfg.RepoACMMPinned("console") {
		t.Fatal("fresh config reports console pinned")
	}
	changed, err := cfg.SetRepoACMMPinnedAndSave("console", true)
	if err != nil {
		t.Fatalf("SetRepoACMMPinnedAndSave: %v", err)
	}
	if !changed {
		t.Fatal("changed = false for a real pin")
	}
	if !cfg.RepoACMMPinned("console") {
		t.Fatal("pin not visible via RepoACMMPinned")
	}
	raw, err := os.ReadFile(cfg.SourcePath)
	if err != nil {
		t.Fatalf("reading persisted config: %v", err)
	}
	if !strings.Contains(string(raw), "acmm_pinned") {
		t.Errorf("persisted config missing acmm_pinned:\n%s", raw)
	}

	// Re-pinning is a no-op and must not rewrite the file.
	changed, err = cfg.SetRepoACMMPinnedAndSave("console", true)
	if err != nil || changed {
		t.Fatalf("re-pin: changed=%v err=%v, want false,nil", changed, err)
	}
	// Unpinning an unknown repo is also a no-op.
	changed, err = cfg.SetRepoACMMPinnedAndSave("never-pinned", false)
	if err != nil || changed {
		t.Fatalf("unpin unknown: changed=%v err=%v, want false,nil", changed, err)
	}
	// Unpin flips the stored policy back.
	changed, err = cfg.SetRepoACMMPinnedAndSave("console", false)
	if err != nil || !changed {
		t.Fatalf("unpin: changed=%v err=%v, want true,nil", changed, err)
	}
	if cfg.RepoACMMPinned("console") {
		t.Fatal("console still pinned after unpin")
	}

	if _, err := cfg.SetRepoACMMPinnedAndSave("   ", true); err == nil {
		t.Error("blank repo accepted")
	}
	var nilCfg *Config
	if _, err := nilCfg.SetRepoACMMPinnedAndSave("console", true); err == nil {
		t.Error("nil config accepted")
	}
}

// The config-side theme wrappers must stay faithful one-line delegates: the
// catalog is non-empty, the default id resolves as a builtin and canonicalizes
// to itself, Effective honors DashboardConfig, and CSS/ETag digests are
// non-empty and distinct between full and preview renders.
func TestDashboardThemeWrappers(t *testing.T) {
	catalog := DashboardThemeCatalog()
	if len(catalog) == 0 {
		t.Fatal("empty theme catalog")
	}
	def := DefaultDashboardThemeID()
	if def == "" {
		t.Fatal("empty default theme id")
	}
	if got := CanonicalDashboardThemeID(def); got != def {
		t.Errorf("CanonicalDashboardThemeID(%q) = %q, want identity", def, got)
	}
	th, ok := DashboardThemeBuiltin(def)
	if !ok {
		t.Fatalf("default theme %q is not a builtin", def)
	}
	if _, ok := DashboardThemeBuiltin("no-such-theme-xyz"); ok {
		t.Error("unknown id reported as builtin")
	}

	eff, err := DashboardThemeEffective(DashboardConfig{Theme: def})
	if err != nil {
		t.Fatalf("DashboardThemeEffective: %v", err)
	}
	if eff.ID != th.ID {
		t.Errorf("effective theme id %q, want %q", eff.ID, th.ID)
	}

	css, err := DashboardThemeCSS(th)
	if err != nil || css == "" {
		t.Fatalf("DashboardThemeCSS: css empty or err %v", err)
	}
	preview, err := DashboardThemePreviewCSS(th)
	if err != nil || preview == "" {
		t.Fatalf("DashboardThemePreviewCSS: css empty or err %v", err)
	}
	etag, err := DashboardThemeETag(th)
	if err != nil || etag == "" {
		t.Fatalf("DashboardThemeETag: etag empty or err %v", err)
	}
	previewETag, err := DashboardThemePreviewETag(th)
	if err != nil || previewETag == "" {
		t.Fatalf("DashboardThemePreviewETag: etag empty or err %v", err)
	}
	if etag == previewETag && css != preview {
		t.Error("full and preview CSS differ but share an ETag")
	}
}

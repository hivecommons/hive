package hub

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// --- verify / allowlist (the guarantee) --------------------------------------

// TestResetToPoolParseLeftover_FailsClosedOnJunk asserts that a /data tree with
// arbitrary accreted junk (a bead dir, a tmux socket, a stray creds file, a
// per-agent CODEX_HOME) is detected as NOT clean — every unexpected entry is
// surfaced as a leftover, so the reset fails closed. A tree with only the seed
// (verify prints the clean sentinel) is detected as clean.
func TestResetToPoolParseLeftover_FailsClosedOnJunk(t *testing.T) {
	// The verify Job prints one leftover /data path per line (or the clean
	// sentinel when there is nothing unexpected).
	dirtyLogs := strings.Join([]string{
		"/data/beads",
		"/data/tmux-1001/default",   // a tmux socket dir
		"/data/gh-user-token",       // a stray credential file
		"/data/home/.codex-scanner", // a per-agent CODEX_HOME
		"/data/hive.yaml.dashboard",
	}, "\n")

	leftover := resetToPoolParseLeftover(dirtyLogs)
	if len(leftover) != 5 {
		t.Fatalf("expected 5 leftover entries (fail-closed), got %d: %v", len(leftover), leftover)
	}
	// Sanity: the credential file and the per-agent CODEX_HOME must both be caught.
	found := map[string]bool{}
	for _, e := range leftover {
		found[e] = true
	}
	for _, must := range []string{"/data/gh-user-token", "/data/home/.codex-scanner"} {
		if !found[must] {
			t.Errorf("leftover detection missed %q — would leak", must)
		}
	}
}

// TestResetToPoolParseLeftover_CleanSentinelPasses asserts that the clean
// sentinel (and only the sentinel) is treated as a clean /data.
func TestResetToPoolParseLeftover_CleanSentinelPasses(t *testing.T) {
	if got := resetToPoolParseLeftover(resetToPoolCleanSentinel + "\n"); len(got) != 0 {
		t.Fatalf("clean sentinel must parse as no leftover, got %v", got)
	}
	// Blank/whitespace-only logs are also clean (nothing to wipe).
	if got := resetToPoolParseLeftover("\n   \n"); len(got) != 0 {
		t.Fatalf("blank logs must parse as no leftover, got %v", got)
	}
}

// TestResetToPoolSeedAllowlistIsEmpty pins the design invariant: the seed
// allowlist is empty, so the wipe is total and the verify tolerates nothing. If
// a future change adds a seeded PVC file it must be a deliberate edit here (which
// this test will flag) so the wipe and verify stay in lockstep.
func TestResetToPoolSeedAllowlistIsEmpty(t *testing.T) {
	if len(resetToPoolSeedAllowlist) != 0 {
		t.Fatalf("seed allowlist must be empty (total wipe); got %v — if intentional, update this test and confirm both wipe and verify honour it", resetToPoolSeedAllowlist)
	}
	// With an empty allowlist the find prune expression must be empty, so nothing
	// is excluded from either the wipe or the verify.
	if expr := resetToPoolFindPruneExpr(resetToPoolSeedAllowlist); expr != "" {
		t.Fatalf("empty allowlist must yield an empty prune expr, got %q", expr)
	}
}

// TestResetToPoolFindPruneExpr_Allowlisted asserts the prune expression correctly
// excludes an allowlisted name (used if the allowlist ever becomes non-empty) and
// shell-quotes it defensively.
func TestResetToPoolFindPruneExpr_Allowlisted(t *testing.T) {
	expr := resetToPoolFindPruneExpr([]string{"hive.yaml", "keep me"})
	if !strings.Contains(expr, "-name 'hive.yaml'") {
		t.Errorf("prune expr should exclude hive.yaml, got %q", expr)
	}
	if !strings.Contains(expr, "-prune -o") {
		t.Errorf("prune expr should end with -prune -o, got %q", expr)
	}
	if !strings.Contains(expr, "'keep me'") {
		t.Errorf("prune expr should shell-quote names with spaces, got %q", expr)
	}
}

// TestResetToPoolWipeScript_TotalWipe asserts the wipe script, with the (empty)
// seed allowlist, wipes everything under /data and does NOT carry a prune clause.
func TestResetToPoolWipeScript_TotalWipe(t *testing.T) {
	script := resetToPoolWipeScript(resetToPoolSeedAllowlist)
	if !strings.Contains(script, "find /data -mindepth 1 -maxdepth 1  -exec rm -rf {} +") {
		t.Errorf("wipe script must be a total rm of /data with no prune, got: %s", script)
	}
	if strings.Contains(script, "-prune") {
		t.Errorf("empty-allowlist wipe must not prune anything, got: %s", script)
	}
}

// TestResetToPoolVerifyScript_PrintsSentinelWhenClean asserts the verify script
// prints the clean sentinel when nothing is left, else the leftover list.
func TestResetToPoolVerifyScript_PrintsSentinelWhenClean(t *testing.T) {
	script := resetToPoolVerifyScript(resetToPoolSeedAllowlist)
	if !strings.Contains(script, resetToPoolCleanSentinel) {
		t.Errorf("verify script must print the clean sentinel, got: %s", script)
	}
	if !strings.Contains(script, "find /data -mindepth 1 -maxdepth 1") {
		t.Errorf("verify script must scan /data top level, got: %s", script)
	}
}

// --- Secret reset shape -------------------------------------------------------

// TestResetToPoolPlaceholderSecretKeys pins the exact placeholder Secret key set.
func TestResetToPoolPlaceholderSecretKeys(t *testing.T) {
	want := []string{"dashboard-token", "github-token"}
	got := resetToPoolPlaceholderSecretKeys()
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("placeholder secret keys = %v, want %v", got, want)
	}
}

// TestResetToPoolUnexpectedSecretKeys_FailsClosedOnSurvivingCreds asserts that a
// surviving credential key (e.g. a gh-app-key*.pem, or a copilot token) on the
// Secret after reset is detected as unexpected — the fail-closed signal. The
// exact placeholder key set passes.
func TestResetToPoolUnexpectedSecretKeys_FailsClosedOnSurvivingCreds(t *testing.T) {
	// A dirty Secret that still carries an App key and a github token PAT.
	dirty := []string{"dashboard-token", "github-token", "gh-app-key.pem", "gh-app-key-3568013.pem"}
	extra := resetToPoolUnexpectedSecretKeys(dirty)
	sort.Strings(extra)
	want := []string{"gh-app-key-3568013.pem", "gh-app-key.pem"}
	if !reflect.DeepEqual(extra, want) {
		t.Fatalf("unexpected keys = %v, want %v (surviving App keys must fail closed)", extra, want)
	}

	// The exact placeholder shape has no unexpected keys.
	if extra := resetToPoolUnexpectedSecretKeys(resetToPoolPlaceholderSecretKeys()); len(extra) != 0 {
		t.Fatalf("placeholder key set must have no unexpected keys, got %v", extra)
	}
}

// TestResetToPoolSecretManifest_NoTenantCreds asserts the recreated Secret
// manifest carries ONLY the placeholder keys — a fresh dashboard-token and the
// non-secret placeholder github-token — and no gh-app-key / tenant credential.
func TestResetToPoolSecretManifest_NoTenantCreds(t *testing.T) {
	m := resetToPoolSecretManifest("hive-hosted-x1", "deadbeef-fresh-token")
	if !strings.Contains(m, "dashboard-token: 'deadbeef-fresh-token'") {
		t.Errorf("secret manifest missing fresh dashboard-token, got:\n%s", m)
	}
	if !strings.Contains(m, "github-token: '"+resetToPoolPlaceholderToken+"'") {
		t.Errorf("secret manifest missing placeholder github-token, got:\n%s", m)
	}
	if strings.Contains(m, "gh-app-key") {
		t.Errorf("secret manifest must NOT carry any App key, got:\n%s", m)
	}
}

// --- state guard: record reset only after verified clean ----------------------

// TestIsResettableLiveHive gates which hives reset-to-pool targets: a delivered
// claim (LIVE) is resettable; an unclaimed placeholder is NOT (that uses
// reset-assignment); a hive already mid-reset is NOT (concurrency latch).
func TestIsResettableLiveHive(t *testing.T) {
	cases := []struct {
		name string
		h    *SaaSHive
		want bool
	}{
		{"live claimed hive", &SaaSHive{ID: "a", ClaimDelivered: true}, true},
		{"unclaimed placeholder", &SaaSHive{ID: "b", Status: statusAvailable}, false},
		{"assigned-but-unclaimed wedge", &SaaSHive{ID: "c", Status: statusAssigned, ClaimDelivered: false}, false},
		{"already resetting", &SaaSHive{ID: "d", ClaimDelivered: true, ResetToPoolStatus: resetToPoolStatusInProgress}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isResettableLiveHive(tc.h); got != tc.want {
				t.Fatalf("isResettableLiveHive(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestResetHiveToAvailablePlaceholder_RecordShape confirms the record-reset half
// (reused from Phase 1) only ever produces the clean available-placeholder shape,
// which is what a verified-clean reset flips the record to. This is the record
// mutation that must NOT run before the wipe is verified clean (the handler
// sequences it after performResetToPoolWipe returns nil).
func TestResetHiveToAvailablePlaceholder_RecordShape(t *testing.T) {
	h := &SaaSHive{
		ID:             "z9",
		Owner:          "tenant-owner",
		Org:            "acme-corp",
		Repos:          []string{"acme/thing"},
		PrimaryRepo:    "acme/thing",
		Status:         statusAssigned,
		ClaimDelivered: true,
		VanityURL:      "https://hosted-acme.example",
		ACMMLevel:      6,
	}
	resetHiveToAvailablePlaceholder(h)
	if h.Status != statusAvailable {
		t.Errorf("status = %q, want %q", h.Status, statusAvailable)
	}
	if h.Owner != hubAdminUsername {
		t.Errorf("owner = %q, want admin", h.Owner)
	}
	if !strings.HasPrefix(h.Org, placeholderOrgPrefix) {
		t.Errorf("org = %q, want %q-prefixed", h.Org, placeholderOrgPrefix)
	}
	if len(h.Repos) != 0 || h.PrimaryRepo != "" || h.VanityURL != "" {
		t.Errorf("tenant identity not cleared: repos=%v primary=%q vanity=%q", h.Repos, h.PrimaryRepo, h.VanityURL)
	}
	if h.ClaimDelivered {
		t.Errorf("ClaimDelivered must be re-armed to false")
	}
}

// --- security helpers ---------------------------------------------------------

// TestResetToPoolSameOrigin exercises the same-origin defense-in-depth check:
// a trusted hub Origin passes, an untrusted one is rejected, and a request with
// no Origin/Referer is allowed (the admin + typed-confirm gates still apply).
func TestResetToPoolSameOrigin(t *testing.T) {
	mk := func(origin, referer string) bool {
		r := httptest.NewRequest(http.MethodPost, "/api/saas/hives/x/reset-to-pool", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if referer != "" {
			r.Header.Set("Referer", referer)
		}
		return resetToPoolSameOrigin(r)
	}
	if !mk("https://hive.kubestellar.io", "") {
		t.Error("trusted Origin must pass")
	}
	if mk("https://evil.example", "") {
		t.Error("untrusted Origin must be rejected")
	}
	if !mk("", "https://hive.kubestellar.io/dashboard") {
		t.Error("trusted Referer (no Origin) must pass")
	}
	if mk("", "https://evil.example/x") {
		t.Error("untrusted Referer must be rejected")
	}
	if !mk("", "") {
		t.Error("no Origin and no Referer must be allowed (same-origin fetch / curl)")
	}
}

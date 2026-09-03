package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func setupContributeEnv(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", filepath.Join(tmpDir, "contributors"))
	t.Setenv("HIVE_FEDERATION_REGISTRY_PATH", filepath.Join(tmpDir, "federation", "registry.json"))
	redirectContributeWSDisk(t, filepath.Join(tmpDir, "ws-state"))

	// Stub DNS so tests with fake hostnames don't hit the fail-closed resolver.
	orig := privateURLResolver
	privateURLResolver = func(_ context.Context, _ string) ([]string, error) {
		return []string{"93.184.216.34"}, nil
	}
	t.Cleanup(func() { privateURLResolver = orig })
}

func redirectContributeWSDisk(t *testing.T, dir string) {
	t.Helper()
	oldActivity, oldCompleted, oldFailed, oldNoPR := activityFilePath, completedTasksFile, failedTasksFile, noPRStreaksFile
	oldLeases := taskLeasesFile
	oldAsyncActivitySave := asyncActivitySave
	oldActivityPersistenceEnabled := activityPersistenceEnabled
	activityFilePath = filepath.Join(dir, "activity.json")
	completedTasksFile = filepath.Join(dir, "completed-tasks.json")
	failedTasksFile = filepath.Join(dir, "failed-tasks.json")
	noPRStreaksFile = filepath.Join(dir, "no-pr-streaks.json")
	taskLeasesFile = filepath.Join(dir, "task-leases.json")
	asyncActivitySave = false
	activityPersistenceEnabled = false
	t.Cleanup(func() {
		activityFilePath, completedTasksFile, failedTasksFile, noPRStreaksFile = oldActivity, oldCompleted, oldFailed, oldNoPR
		taskLeasesFile = oldLeases
		asyncActivitySave = oldAsyncActivitySave
		activityPersistenceEnabled = oldActivityPersistenceEnabled
	})
}

func TestContributeRegister(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"github_username":"testuser123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["contributor_id"] == "" {
		t.Error("missing contributor_id")
	}
	if resp["registration_token"] == "" {
		t.Error("missing registration_token")
	}
	if !strings.Contains(resp["message"], "Registered successfully") {
		t.Errorf("unexpected message: %s", resp["message"])
	}

}

func TestContributeRegisterInvalidUsername(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"github_username":"bad user!"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestContributeRegisterEmpty(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"github_username":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestContributeStatus(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/status", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["hub"] != "online" {
		t.Errorf("expected hub=online, got %v", resp["hub"])
	}
}

func TestContributeLanding(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("Contribute to")) {
		t.Error("landing page missing expected content")
	}

	body := w.Body.Bytes()

	// #2536/#2540a: Copilot must carry a real host-mode install command, not
	// an empty data-host-install that falls through to a comment-only line.
	if !bytes.Contains(body, []byte(`data-host-install="npm install -g @github/copilot`)) {
		t.Error("copilot option missing a non-empty data-host-install")
	}

	// #2536: an OS selector must exist so install guidance isn't macOS-only,
	// and macOS must remain the default selection with the unchanged
	// brew-based prerequisite line.
	if !bytes.Contains(body, []byte(`id="os-select"`)) {
		t.Error("landing page missing OS selector")
	}
	if !bytes.Contains(body, []byte(`<option value="macos" selected>macOS</option>`)) {
		t.Error("OS selector must default to macOS")
	}
	if !bytes.Contains(body, []byte("brew install just gh")) {
		t.Error("macOS default prerequisite line (brew install just gh) missing")
	}

	// #2536/#2540b: the "don't see your CLI?" prefill link must not reference
	// the non-existent "contribute" label — only "enhancement" exists.
	if bytes.Contains(body, []byte("labels=contribute")) {
		t.Error("CLI request link must not reference the non-existent 'contribute' label")
	}
	if !bytes.Contains(body, []byte("labels=enhancement")) {
		t.Error("CLI request link missing labels=enhancement")
	}

	// #2536/#2540: the server-rendered (no-JS) fallback command block must be
	// self-labeling as a default.
	if !bytes.Contains(body, []byte("Default shown: macOS + Claude Code + containerized mode")) {
		t.Error("server-rendered fallback command block missing default marker comment")
	}

	// #2846: multi-hub contributor sessions must stay discoverable from the
	// onboarding page, including the ordering rule for paired tokens.
	for _, want := range [][]byte{
		[]byte(`id="multi-hub-note"`),
		[]byte("Contribute to multiple hives"),
		[]byte("HIVE_HUB</code> to comma-separated WebSocket URLs"),
		[]byte("HIVE_REGISTRATION_TOKEN</code> to the matching comma-separated tokens in the same order"),
		[]byte("one CLI/tmux session"),
		[]byte("github.com/hivecommons/hive/pull/2846"),
	} {
		if !bytes.Contains(body, want) {
			t.Errorf("landing page missing multi-hub help text %q", want)
		}
	}
}

// TestContributeLandingHasOpsTab pins the additive tab chrome: the landing page
// must now render the Onboarding / Management / Operations tab bar (the former
// single "Management & Operations" tab is split in two), the Operations panel,
// AND still carry the existing onboarding content unchanged.
func TestContributeLandingHasOpsTab(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// New tab chrome: FOUR tabs now — Onboarding, Management, Operations,
	// Leaderboard (the leaderboard was folded in from its standalone page).
	for _, want := range []string{
		`class="page-tabs"`,
		`data-panel="tab-onboarding"`,
		`data-panel="tab-manage"`,
		`data-panel="tab-ops"`,
		`data-panel="tab-leaderboard"`,
		`id="ptab-manage"`,
		`id="ptab-ops"`,
		`id="ptab-leaderboard"`,
		`>Management<`,  // Management tab label (controls only)
		`>Operations<`,  // Operations tab label (monitoring)
		`>Leaderboard<`, // Leaderboard tab label (inline rankings)
		`id="tab-manage"`,
		`id="tab-ops"`,
		`id="tab-leaderboard"`,
		`Connected clankers`,
		`id="work-list"`,
		`/api/contribute/fleet`,
		`opened`, // pipeline node
		// Leaderboard tab is hydrated client-side from the reused API endpoint,
		// through the same show/hide mechanism the Operations tab uses.
		`/api/leaderboard`,
		`id="leaderboard-list"`,
		`function loadLeaderboard`,
		`function renderLeaderboard`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tab chrome missing %q", want)
		}
	}

	// The old "View Leaderboard" link navigated AWAY to the standalone page.
	// It must no longer be an <a href="/leaderboard"> — the leaderboard is now a
	// tab on THIS page. (The /leaderboard route still exists as a redirect shim,
	// referenced by the Leaderboard tab's descriptive copy, but the onboarding CTA
	// must open the tab in place, not navigate away.)
	if strings.Contains(body, `href="/leaderboard"`) {
		t.Error(`navigate-away href="/leaderboard" link still present; leaderboard is now an inline tab`)
	}
	if !strings.Contains(body, `id="goto-leaderboard-tab"`) {
		t.Error("onboarding CTA should be a button that opens the Leaderboard tab in place")
	}

	// The Leaderboard panel must render AFTER the Operations panel (it is the
	// 4th tab), and its show/hide must run through the same tab JS — verify the
	// hydration is wired on tab-leaderboard open without any admin/role gate.
	iLbPanel := strings.Index(body, `id="tab-leaderboard"`)
	iOpsPanel := strings.Index(body, `id="tab-ops"`)
	if iLbPanel < 0 || iOpsPanel < 0 || iOpsPanel >= iLbPanel {
		t.Errorf("Leaderboard panel must render after Operations: ops=%d leaderboard=%d", iOpsPanel, iLbPanel)
	}
	if !strings.Contains(body, `dp==='tab-leaderboard'&&!lbStarted`) {
		t.Error("Leaderboard tab must hydrate on first open via the shared tab-switch JS")
	}

	// The single "Management & Operations" tab was split; that combined label
	// must no longer appear.
	if strings.Contains(body, "Management &amp; Operations") {
		t.Error(`combined "Management & Operations" tab label still present after the split`)
	}

	// The split must place the admin CONTROLS block under Management and the
	// monitoring panels (clankers list / my work / pipeline) under Operations.
	// Verify ordering: ops-admin appears inside tab-manage, before tab-ops opens.
	iManage := strings.Index(body, `id="tab-manage"`)
	iAdmin := strings.Index(body, `id="ops-admin"`)
	iOps := strings.Index(body, `id="tab-ops"`)
	iClankers := strings.Index(body, `<h3>Connected clankers</h3>`)
	if iManage < 0 || iAdmin < 0 || iOps < 0 || iClankers < 0 {
		t.Fatalf("missing anchors: manage=%d admin=%d ops=%d clankers=%d", iManage, iAdmin, iOps, iClankers)
	}
	if iManage >= iAdmin || iAdmin >= iOps {
		t.Errorf("admin controls must render under Management (before Operations): manage=%d admin=%d ops=%d", iManage, iAdmin, iOps)
	}
	if iOps >= iClankers {
		t.Errorf("Connected clankers must render under Operations (after tab-ops opens): ops=%d clankers=%d", iOps, iClankers)
	}

	// Existing onboarding content must still be present and unchanged.
	for _, want := range []string{
		`id="cli-select"`,
		`id="mode-select"`,
		`id="runtime-select"`,
		`How it works`,
		// #4549 tokenized the neutral inline literals so the light ramp reaches
		// them; this assertion guards the SENTENCE, not the hex it was written in.
		`Powered by <strong style="color:var(--cc-text)">ClankeR</strong>`,
		`Trust tiers`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("onboarding content missing %q (must remain unchanged)", want)
		}
	}
}

func TestContributeLandingHasCustomCSSHelp(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute/leaderboard", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /contribute/leaderboard = %d, want 200", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`id="custom-css-info-btn"`,
		`class="info-pop custom-css-pop"`,
		`pop.style.position='fixed'`,
		`Custom CSS`,
		`?style=owner/repo/path/theme.css@ref`,
		`?style=castrojo/themes/lb/bluefin.css@main`,
		`repo&rsquo;s <code>HEAD</code>`,
		`Public GitHub repos only`,
		`sanitized server-side`,
		`128 KiB`,
		`<code>/</code> and <code>/snapshot</code>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("custom CSS help missing %q", want)
		}
	}
}

// TestContributeLandingHasKubernetesMode pins the #2549 discoverability
// addition: the /contribute run-mode surface must offer a Kubernetes path
// alongside containerized and host, wired to `just contribute-k8s`, and it must
// state the two honest constraints — only headless-capable backends run in a
// cluster (#2660) and the interim credential model deferring to #2537.
func TestContributeLandingHasKubernetesMode(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /contribute = %d, want 200", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// The third run-mode option.
		`<option value="kubernetes">Kubernetes (cluster)</option>`,
		// The command the mode generates.
		`just contribute-k8s`,
		// Honest headless-backend constraint (#2660) surfaced to the reader.
		`K8S_HEADLESS_BACKENDS`,
		`headless`,
		// Interim credential story deferring to #2537, visible on the page.
		`id="k8s-note"`,
		`2537`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/contribute Kubernetes surface missing %q", want)
		}
	}

	// The existing two modes must remain — this is additive, not a replacement.
	for _, want := range []string{
		`<option value="containerized">Containerized (recommended)</option>`,
		`<option value="host">Host (non-containerized)</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("existing run mode removed: missing %q", want)
		}
	}
}

// TestContributeLandingHasWatsonxOption pins the clanker-side watsonx.ai
// onboarding option: a contributor bringing their own watsonx OpenAI-compatible
// endpoint + key gets a labeled, guided CLI choice (instead of "Other"), and
// the backend is headless-capable so it does not trip the Kubernetes-mode
// no-headless-mode warning. This is onboarding UX only — the hub never sees
// the contributor's watsonx endpoint or key.
func TestContributeLandingHasWatsonxOption(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /contribute = %d, want 200", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// The CLI select option: labeled, guided, own-endpoint-and-key.
		`value="watsonx"`,
		`watsonx.ai (IBM Granite + your key)`,
		// The env block routes through the OpenAI-compatible gateway, same
		// pattern as vllm/llm-d, and stays local (never sent to the hive).
		`ml.cloud.ibm.com/ml/gateway/v1`,
		`WATSONX_PROJECT_ID`,
		`never sent to the hive`,
		// CLIENTS metadata tile.
		`watsonx:{name:'watsonx.ai',tag:'IBM'}`,
		// Headless-capable: must be registered so Kubernetes mode does not warn.
		`K8S_HEADLESS_BACKENDS={claude:1,litellm:1,copilot:1,codex:1,watsonx:1,goose:1}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/contribute watsonx onboarding surface missing %q", want)
		}
	}
}

// TestContributeK8sHeadlessCapability enumerates every backend the Kubernetes
// run-mode reasons about and pins whether it is headless-capable, so adding a
// backend forces an explicit decision here rather than silently defaulting to
// "no headless mode". goose is capable via its `goose run --no-session -t`
// one-shot sub-command (#2828); bob/pi drive an interactive TUI with no known
// non-interactive entry point, so they must keep warning. agy keeps warning
// for a DIFFERENT reason — it has a print mode, but no way to sign in inside a
// pod — which is why the reason string is pinned alongside the boolean.
func TestContributeK8sHeadlessCapability(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /contribute = %d, want 200", w.Code)
	}
	body := w.Body.String()

	// Scan ONLY the capability map's own literal. A bare strings.Contains for
	// "<backend>:1" over the whole page matches any other JS object that
	// happens to use the same key (HOST_ONLY_BACKENDS did exactly that), which
	// would silently turn this pin into a page-wide grep.
	const mapKey = "K8S_HEADLESS_BACKENDS={"
	i := strings.Index(body, mapKey)
	if i < 0 {
		t.Fatalf("K8S_HEADLESS_BACKENDS literal not found on the page")
	}
	rest := body[i+len(mapKey):]
	j := strings.Index(rest, "}")
	if j < 0 {
		t.Fatalf("K8S_HEADLESS_BACKENDS literal is unterminated")
	}
	capabilityMap := rest[:j]

	for _, tc := range []struct {
		backend  string
		headless bool
		why      string
	}{
		{"claude", true, "claude -p print mode"},
		{"litellm", true, "claude binary against a LiteLLM proxy"},
		{"copilot", true, "copilot -p programmatic mode"},
		{"codex", true, "codex exec sub-command"},
		{"watsonx", true, "OpenAI-compatible endpoint via the claude binary"},
		{"goose", true, "goose run --no-session -t one-shot sub-command (#2828)"},
		{"bob", false, "interactive TUI, no known one-shot entry point"},
		// agy DOES have a one-shot mode (`agy -p`, in the relay's own
		// HEADLESS_BACKENDS since this change) but stays false HERE: this list
		// gates the Kubernetes path, and agy signs in through an interactive
		// Google OAuth flow with no API-key mode, so a pod can never
		// authenticate. Capability and credential are separate questions.
		{"agy", false, "headless-capable (agy -p) but cannot sign in inside a pod"},
		{"pi", false, "interactive TUI, no known one-shot entry point"},
	} {
		// The capability map is emitted as a JS object literal keyed by backend.
		present := strings.Contains(capabilityMap, tc.backend+":1")
		if present != tc.headless {
			t.Errorf("K8S_HEADLESS_BACKENDS headless=%v for %q, want %v (%s)",
				present, tc.backend, tc.headless, tc.why)
		}
	}
}

// TestContributeFleet pins the read-only fleet endpoint JSON shape.
func TestContributeFleet(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Clankers []FleetClanker            `json:"clankers"`
		Work     []FleetWorkItem           `json:"work"`
		Policy   ContributeAdmissionPolicy `json:"policy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// With no connections the fleet is empty but the keys must be present arrays.
	if resp.Clankers == nil {
		t.Error("clankers should be a non-nil array")
	}
	if resp.Work == nil {
		t.Error("work should be a non-nil array")
	}
	// Policy always exposes the promotion thresholds sourced from constants.
	if resp.Policy.AutoPromoteAt != contributorAutoPromoteAt {
		t.Errorf("auto_promote_at = %d, want %d", resp.Policy.AutoPromoteAt, contributorAutoPromoteAt)
	}
	if resp.Policy.TrustedAt != contributorTrustedAt {
		t.Errorf("trusted_at = %d, want %d", resp.Policy.TrustedAt, contributorTrustedAt)
	}
}

// TestTrustTierTableMatchesConstants (#2544): the on-page trust-tier table must
// describe what the promotion code actually does — auto-promotion counts tasks
// that PRODUCED A PR (TasksWithPR), not bare completed tasks — and must render
// its numbers from contributorAutoPromoteAt / contributorTrustedAt so the page
// cannot drift from the code again. The old over-promising wording ("5 completed
// tasks", "maintainer voucher" + "Merge PRs") must be gone.
func TestTrustTierTableMatchesConstants(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// A missing format arg would render as %!s(MISSING); the whole page must be
	// clean (this also guards the spliced tier-table %s verb).
	if strings.Contains(body, "%!") {
		t.Fatalf("landing page has a format error (%%! present) — verb/arg mismatch")
	}

	// Corrected, constant-sourced wording is present.
	wantAutoPromote := fmt.Sprintf("%d tasks that produced a PR", contributorAutoPromoteAt)
	if !strings.Contains(body, wantAutoPromote) {
		t.Errorf("tier table missing corrected contributor row %q", wantAutoPromote)
	}
	wantTrusted := fmt.Sprintf("~%d PR tasks, then granted by a maintainer", contributorTrustedAt)
	if !strings.Contains(body, wantTrusted) {
		t.Errorf("tier table missing corrected trusted row %q", wantTrusted)
	}

	// The old inaccurate wording must be gone.
	for _, gone := range []string{
		"5 completed tasks",
		"20 tasks + maintainer voucher",
		"<td>Merge PRs</td>",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("tier table still contains inaccurate wording %q", gone)
		}
	}
}

func TestContributorNotFound(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contributors/nonexistent", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHivesRegisterAndList(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	// Register
	body := `{"project_name":"test-proj","org":"test-org","hub_url":"wss://test:3001/contribute"}`
	req := httptest.NewRequest(http.MethodPost, "/api/hives/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("register: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// List
	req2 := httptest.NewRequest(http.MethodGet, "/api/hives", nil)
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", w2.Code)
	}

	var reg FederationRegistry
	if err := json.Unmarshal(w2.Body.Bytes(), &reg); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	found := false
	for _, h := range reg.Hives {
		if h.ProjectName == "test-proj" {
			found = true
			break
		}
	}
	if !found {
		t.Error("registered hive not found in list")
	}

}

func TestHivesRegisterMissingFields(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"project_name":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/hives/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── Leaderboard tests ─────────────────────────────────────────────────────

func seedContributor(t *testing.T, username string, completed, failed int) {
	t.Helper()
	p := &ContributorProfile{
		GitHubUsername: username,
		ContributorID:  "c-" + username,
		TrustTier:      "contributor",
		TasksCompleted: completed,
		TasksFailed:    failed,
		RegisteredAt:   "2025-01-01T00:00:00Z",
	}
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("seedContributor(%s): %v", username, err)
	}
}

func TestLeaderboardAPIEmpty(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/leaderboard", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Leaderboard) != 0 {
		t.Errorf("expected empty leaderboard, got %d entries", len(resp.Leaderboard))
	}
}

func TestLeaderboardAPISorted(t *testing.T) {
	setupContributeEnv(t)

	seedContributor(t, "alice", 10, 2)
	seedContributor(t, "bob", 25, 1)
	seedContributor(t, "carol", 5, 0)

	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/leaderboard", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if len(resp.Leaderboard) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(resp.Leaderboard))
	}

	// Verify sort order: bob (25) > alice (10) > carol (5)
	expectedOrder := []string{"bob", "alice", "carol"}
	for i, name := range expectedOrder {
		if resp.Leaderboard[i].GitHubUsername != name {
			t.Errorf("rank %d: expected %s, got %s", i+1, name, resp.Leaderboard[i].GitHubUsername)
		}
		if resp.Leaderboard[i].Rank != i+1 {
			t.Errorf("rank %d: expected rank=%d, got rank=%d", i+1, i+1, resp.Leaderboard[i].Rank)
		}
	}

	// Verify avatar URL format
	if resp.Leaderboard[0].AvatarURL != "https://github.com/bob.png" {
		t.Errorf("unexpected avatar URL: %s", resp.Leaderboard[0].AvatarURL)
	}
}

// TestLeaderboardPageRedirect pins the deep-link shim behaviour: the standalone
// leaderboard page was folded into the /contribute Leaderboard tab, so
// GET /leaderboard now redirects to the canonical path-style tab URL
// /contribute/leaderboard (which the tab JS reads from location.pathname on load
// to open the Leaderboard tab). Data still comes from the reused
// /api/leaderboard endpoint (asserted by TestLeaderboardAPISorted), so the
// redirect must hold regardless of whether any contributors exist.
func TestLeaderboardPageRedirect(t *testing.T) {
	setupContributeEnv(t)

	seedContributor(t, "alice", 10, 2)

	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/leaderboard", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/contribute/leaderboard" {
		t.Errorf("Location = %q, want /contribute/leaderboard", loc)
	}
}

// TestLeaderboardPageRedirectEmpty confirms the redirect holds with no
// contributors seeded (the tab hydrates client-side, so there is no empty-state
// server render to special-case any more).
func TestLeaderboardPageRedirectEmpty(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/leaderboard", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/contribute/leaderboard" {
		t.Errorf("Location = %q, want /contribute/leaderboard", loc)
	}
}

func TestTrustTierColor(t *testing.T) {
	cases := []struct {
		tier  string
		color string
	}{
		{"newcomer", "#8b949e"},
		{"contributor", "#3fb950"},
		{"trusted", "#d29922"},
		{"merger", "#f778ba"},
		{"advisor", "#a371f7"},
		{"revoked", "#f85149"},
		{"unknown", "#8b949e"},
	}
	for _, tc := range cases {
		got := trustTierColor(tc.tier)
		if got != tc.color {
			t.Errorf("trustTierColor(%q) = %q, want %q", tc.tier, got, tc.color)
		}
	}
}

func TestTrustTierBadgeCSS(t *testing.T) {
	cases := []struct {
		tier   string
		wantBg string
	}{
		{"newcomer", "rgba(107,114,128,0.2)"},
		{"contributor", "rgba(59,130,246,0.2)"},
		{"trusted", "rgba(34,197,94,0.2)"},
		{"merger", "rgba(247,120,186,0.2)"},
		{"advisor", "rgba(168,85,247,0.2)"},
		{"revoked", "rgba(239,68,68,0.2)"},
		{"unknown", "rgba(107,114,128,0.2)"},
	}
	for _, tc := range cases {
		bg, text, border := trustTierBadgeCSS(tc.tier)
		if bg != tc.wantBg {
			t.Errorf("trustTierBadgeCSS(%q) bg = %q, want %q", tc.tier, bg, tc.wantBg)
		}
		if text == "" {
			t.Errorf("trustTierBadgeCSS(%q) text is empty", tc.tier)
		}
		if border == "" {
			t.Errorf("trustTierBadgeCSS(%q) border is empty", tc.tier)
		}
	}
}

func TestRankDisplay(t *testing.T) {
	gold := rankDisplay(1)
	if !strings.Contains(gold, "medal") || !strings.Contains(gold, "1st place") {
		t.Errorf("rank 1 should show gold medal, got %q", gold)
	}

	silver := rankDisplay(2)
	if !strings.Contains(silver, "medal") || !strings.Contains(silver, "2nd place") {
		t.Errorf("rank 2 should show silver medal, got %q", silver)
	}

	bronze := rankDisplay(3)
	if !strings.Contains(bronze, "medal") || !strings.Contains(bronze, "3rd place") {
		t.Errorf("rank 3 should show bronze medal, got %q", bronze)
	}

	fourth := rankDisplay(4)
	if !strings.Contains(fourth, "#4") {
		t.Errorf("rank 4 should show #4, got %q", fourth)
	}
}

func TestIsValidUsername(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"testuser", true},
		{"test-user", true},
		{"test_user123", true},
		{"j.doe", true},
		{"user.name.with.dots", true},
		{"bad user!", false},
		{"", false},
		{"user@name", false},
		{"<script>alert(1)</script>", false},
		{"../../../etc/passwd", false},
		{strings.Repeat("a", 39), true},
		{strings.Repeat("a", 40), false},
	}
	for _, tc := range cases {
		got := isValidUsername(tc.input)
		if got != tc.want {
			t.Errorf("isValidUsername(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// ── Handler coverage tests ──────────────────────────────────────────────

func registerTestUser(t *testing.T, s *Server, username string) (contributorID, token string) {
	t.Helper()
	body := `{"github_username":"` + username + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("register %s: %d %s", username, w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	return resp["contributor_id"], resp["registration_token"]
}

func TestRegisterForceReissue(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	// First registration — should succeed with a token
	_, token1 := registerTestUser(t, s, "force-user")
	if token1 == "" {
		t.Fatal("expected token on first register")
	}

	// Second registration without force — should say already registered, no token
	body := `{"github_username":"force-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["registration_token"] != "" {
		t.Error("should not return token without force")
	}

	// SECURITY (F5): the unauthenticated register endpoint must NOT reissue an
	// existing contributor's token, even with force:true — that was an account
	// takeover primitive (POST any known username → receive their live token).
	// force:true is now ignored; no token is returned and the stored token is
	// unchanged. Rotation requires the authenticated /api/contribute/reissue-token.
	// (The X-Hive-User header below is irrelevant to v4's dropped-force design —
	// force is ignored regardless of caller identity.)
	body = `{"github_username":"force-user","force":true}`
	req = httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "force-user")
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["registration_token"] != "" {
		t.Errorf("force:true must NOT return a token (account-takeover fix), got %q", resp["registration_token"])
	}

	// The stored token hash must still match the ORIGINAL token — force did not
	// rotate it.
	profile, _ := loadContributorProfile("force-user")
	if profile == nil {
		t.Fatal("profile should exist")
	}
	if profile.RegistrationToken != sha256Hex(token1) {
		t.Error("force:true must not change the stored token")
	}
}

func TestContributorsList(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	registerTestUser(t, s, "list-a")
	registerTestUser(t, s, "list-b")

	req := httptest.NewRequest(http.MethodGet, "/api/contributors", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Contributors []ContributorProfile `json:"contributors"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Contributors) != 2 {
		t.Fatalf("expected 2, got %d", len(resp.Contributors))
	}
	for _, c := range resp.Contributors {
		if c.RegistrationToken != "" || c.TokenPlain != "" {
			t.Errorf("token leaked for %s", c.GitHubUsername)
		}
	}
}

func TestContributorGet(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	cid, _ := registerTestUser(t, s, "get-test")

	// Get by ID
	req := httptest.NewRequest(http.MethodGet, "/api/contributors/"+cid, nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("by ID: expected 200, got %d", w.Code)
	}

	// Get by username
	req2 := httptest.NewRequest(http.MethodGet, "/api/contributors/get-test", nil)
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("by username: expected 200, got %d", w2.Code)
	}
}

func TestContributorTrust(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	cid, _ := registerTestUser(t, s, "trust-test")

	// Valid tier change (owner role required — mutation endpoint fails closed on
	// a missing role, see requireContributorWrite / C5 fix).
	body := `{"tier":"contributor"}`
	req := httptest.NewRequest(http.MethodPut, "/api/contributors/"+cid+"/trust", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Invalid tier
	body2 := `{"tier":"superadmin"}`
	req2 := httptest.NewRequest(http.MethodPut, "/api/contributors/"+cid+"/trust", bytes.NewBufferString(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Hive-Role", "owner")
	req2.Header.Set(ownerRoleVerifiedHeader, "true")
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid tier, got %d", w2.Code)
	}
}

func TestContributorDelete(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	cid, _ := registerTestUser(t, s, "delete-test")

	req := httptest.NewRequest(http.MethodDelete, "/api/contributors/"+cid, nil)
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify gone
	req2 := httptest.NewRequest(http.MethodGet, "/api/contributors/"+cid, nil)
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", w2.Code)
	}
}

func TestContributorRevoke(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	cid, _ := registerTestUser(t, s, "revoke-test")

	req := httptest.NewRequest(http.MethodPost, "/api/contributors/"+cid+"/revoke", nil)
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Re-register blocked
	body := `{"github_username":"revoke-test"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for revoked re-register, got %d", w2.Code)
	}
}

func TestContributeActivity(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/activity", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Activity []any `json:"activity"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestContributeActivityHonorsLimit(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	for i := 0; i < 7; i++ {
		s.contributeHub.addActivity(fmt.Sprintf("user-%d", i), "task_complete", "scanner", "copilot", "gpt", "", fmt.Sprintf("repo#%d", i))
	}

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/activity?limit=3", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Activity []ActivityEntry `json:"activity"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got := len(resp.Activity); got != 3 {
		t.Fatalf("expected limit=3 to return 3 entries, got %d", got)
	}
	if resp.Activity[0].Username != "user-4" || resp.Activity[2].Username != "user-6" {
		t.Fatalf("expected most recent tail user-4..user-6, got %#v", resp.Activity)
	}
}

func TestHivesHeartbeat(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	// Register a hive first
	body := `{"project_name":"hb","org":"hb-org","hub_url":"wss://x:3001/c"}`
	req := httptest.NewRequest(http.MethodPost, "/api/hives/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	// Heartbeat
	hb := `{"active_contributors":5,"active_agents":3}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/hives/hive-hb-org-hb/heartbeat", bytes.NewBufferString(hb))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	// 404 for unknown hive
	req3 := httptest.NewRequest(http.MethodPost, "/api/hives/nonexistent/heartbeat", bytes.NewBufferString(hb))
	req3.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	s.mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w3.Code)
	}
}

func TestHivesDelete(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"project_name":"del","org":"del-org","hub_url":"wss://x:3001/c"}`
	req := httptest.NewRequest(http.MethodPost, "/api/hives/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	// Audit F25 (2026-08-14): this test used to send a bare DELETE with no role
	// headers and assert 200 — i.e. it ENCODED the missing gate as correct
	// behaviour. It is inverted rather than deleted: the un-gated call must now
	// be REFUSED, and the owner call must still succeed.
	reqUngated := httptest.NewRequest(http.MethodDelete, "/api/hives/hive-del-org-del", nil)
	wUngated := httptest.NewRecorder()
	s.mux.ServeHTTP(wUngated, reqUngated)
	if wUngated.Code != http.StatusForbidden {
		t.Fatalf("un-gated DELETE /api/hives/{id} got %d, want 403 — the owner gate is missing (audit F25)", wUngated.Code)
	}

	req2 := httptest.NewRequest(http.MethodDelete, "/api/hives/hive-del-org-del", nil)
	markOwnerRequest(req2)
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}

	// 404 for already deleted
	req3 := httptest.NewRequest(http.MethodDelete, "/api/hives/hive-del-org-del", nil)
	markOwnerRequest(req3)
	w3 := httptest.NewRecorder()
	s.mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w3.Code)
	}
}

func TestHivesOnboard(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	body := `{"project_name":"ob","org":"ob-org","repos":["ob-org/repo1"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/hives/onboard", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		NextSteps []string `json:"next_steps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.NextSteps) < 3 {
		t.Fatalf("expected >=3 steps, got %d", len(resp.NextSteps))
	}

	steps := strings.Join(resp.NextSteps, "\n")
	for _, want := range []string{
		"docker compose up -d",
		"Quadlet",
		"systemctl --user",
		"src/docs/podman-standalone-quadlet.md",
		"~/.config/hive/secrets/gh-app-key.pem",
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("onboarding steps do not mention %q: %q", want, steps)
		}
	}
}

func TestReservedUsernames(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	reserved := []string{"null", "undefined", "admin", "root", "system", "hive", "api", "contribute", "leaderboard"}
	for _, name := range reserved {
		body := `{"github_username":"` + name + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("reserved name %q: expected 400, got %d", name, w.Code)
		}
	}
}

func TestBodySizeLimit(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	huge := `{"github_username":"x","padding":"` + strings.Repeat("A", 50000) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", bytes.NewBufferString(huge))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for huge body, got %d", w.Code)
	}
}

// TestContributeTabPathServesLanding pins the path-style deep links introduced
// with URL-addressable tabs: GET /contribute/<tab> must serve the SAME landing
// HTML as /contribute (the client JS reads location.pathname on load and
// activates the matching tab). Every accepted slug — clean names and legacy
// short ids — must 200 with the page body, so Operations/Leaderboard hydrate on
// direct load exactly as a click would (activateTab is the single choke point).
func TestContributeTabPathServesLanding(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	paths := []string{
		"/contribute",
		"/contribute/onboarding",
		"/contribute/management",
		"/contribute/manage",
		"/contribute/operations",
		"/contribute/ops",
		"/contribute/leaderboard",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: expected 200, got %d", p, w.Code)
			continue
		}
		// The same landing page (not a redirect / empty body): the ClankeR
		// heading and the tab strip must be present.
		if !bytes.Contains(w.Body.Bytes(), []byte("Contribute to")) {
			t.Errorf("GET %s: landing page content missing", p)
		}
		if !bytes.Contains(w.Body.Bytes(), []byte(`data-panel="tab-ops"`)) {
			t.Errorf("GET %s: tab strip missing (page not fully rendered)", p)
		}
	}
}

// TestContributeTabURLWiring asserts the client-side URL-addressable-tab plumbing
// is present in the served HTML: the path/param reader (tabFromLocation), the
// address-bar reflection via history.pushState, the Back/Forward popstate handler,
// and the friendly-name -> panel mapping. These are runtime behaviours we can't
// execute in a Go test, so we pin their presence to guard against silent removal.
func TestContributeTabURLWiring(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, needle := range []string{
		"function tabFromLocation", // reads location.pathname + ?tab= on load
		"history.pushState",        // address-bar reflection on tab switch
		"popstate",                 // Back/Forward navigation
		"'operations'",             // clean name accepted for Operations
		"'management'",             // clean name accepted for Management
		"/contribute/'+slug",       // path-style URL construction
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("served page missing tab-URL wiring: %q", needle)
		}
	}
}

// TestContributeSubpathsArePublic guards that the path-style tab URLs stay
// anonymous-accessible: isPublicPath already prefixes /contribute/, but the tabs
// depend on it, so pin it. A regression here would 401 shared/bookmarked tab URLs.
func TestContributeSubpathsArePublic(t *testing.T) {
	for _, p := range []string{
		"/contribute",
		"/contribute/onboarding",
		"/contribute/management",
		"/contribute/operations",
		"/contribute/leaderboard",
	} {
		if !isPublicPath(p) {
			t.Errorf("isPublicPath(%q) = false, want true (shared tab URLs must be public)", p)
		}
	}
}

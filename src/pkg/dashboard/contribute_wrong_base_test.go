package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#13146: contributor dispatch honours the issue's declared
// target branch, and a wrong_base verdict is a dispatch error rather than a
// settlement.

func TestTargetBranch_DeclaredFromTitleAndBody(t *testing.T) {
	cases := []struct {
		name, title, body, wantBranch, wantSource string
	}{
		{"title release line", "[v6] Fix the scanner", "", "v6", targetSourceTitle},
		{"title after lane", "🐛 [quality] [v7] Fix", "", "v7", targetSourceTitle},
		{"body bold target", "Fix the scanner", "Some text.\n\n**Target: v6.**\n\nMore.", "v6", targetSourceBody},
		{"body plain target", "Fix", "Target: release-1.2", "release-1.2", targetSourceBody},
		{"body target branch", "Fix", "target branch: `feature/x`", "feature/x", targetSourceBody},
		{"title wins over body", "[v5] Fix", "Target: v6", "v5", targetSourceTitle},
		{"lane only is not a branch", "[quality] Fix", "", "", ""},
		{"nothing", "Fix", "no target here", "", ""},
		{"bad branch name", "Fix", "Target: ../etc", "", ""},
		{"inline mention is not a declaration", "Fix", "the Target: field is unrelated prose", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, src := declaredTargetBranch(tc.title, tc.body)
			if b != tc.wantBranch || src != tc.wantSource {
				t.Fatalf("declaredTargetBranch(%q,%q) = (%q,%q), want (%q,%q)", tc.title, tc.body, b, src, tc.wantBranch, tc.wantSource)
			}
		})
	}
}

func TestTargetBranch_PlausibleNames(t *testing.T) {
	for _, ok := range []string{"v6", "main", "release-1.2", "feature/x.y_z", "A1"} {
		if !isPlausibleBranchName(ok) {
			t.Errorf("%q must be plausible", ok)
		}
	}
	for _, bad := range []string{"", "-x", "/x", "x/", "a..b", "x.lock", "x.", "has space", "näme", strings.Repeat("a", maxBranchNameLen+1)} {
		if isPlausibleBranchName(bad) {
			t.Errorf("%q must not be plausible", bad)
		}
	}
	if tags := titlePrefixTags("🐛 [Quality] [v6] Fix [not-a-tag]"); len(tags) != 2 || tags[0] != "Quality" || tags[1] != "v6" {
		t.Fatalf("titlePrefixTags = %v", tags)
	}
}

// branchListFixture is a branchLister seam that counts calls so the
// per-sweep cache is observable.
type branchListFixture struct {
	mu    sync.Mutex
	names map[string][]string
	err   error
	calls int
}

func (f *branchListFixture) list(_ context.Context, repo string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.names[repo], nil
}

func (f *branchListFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestTargetBranch_ResolveValidatesAgainstRepoAndCaches(t *testing.T) {
	hub, s := covK2Hub(t)
	s.deps.Config.Project.Org = "acme"
	fx := &branchListFixture{names: map[string][]string{"acme/widgets": {"main", "v6", "testing"}}}
	hub.branchLister = fx.list
	ctx := context.Background()

	got := hub.resolveTaskTargetBranch(ctx, "acme/widgets", "acme/widgets#1", "Fix", "**Target: v6.**")
	if got.Branch != "v6" || got.Declared != "v6" || got.Source != targetSourceBody || got.Invalid {
		t.Fatalf("existing body target: %+v", got)
	}
	// A title release line the repository does not have falls back to the
	// default (an external repo: the ask-the-repository wording) and says so.
	got = hub.resolveTaskTargetBranch(ctx, "acme/widgets", "acme/widgets#2", "[v9] Fix", "")
	if got.Branch != "" || got.Declared != "v9" || !got.Invalid || got.Source != targetSourceTitle {
		t.Fatalf("missing title target must fall back and be Invalid: %+v", got)
	}
	// A generic bracket is a branch only when the repository has it.
	got = hub.resolveTaskTargetBranch(ctx, "acme/widgets", "acme/widgets#3", "[testing] Fix", "")
	if got.Branch != "testing" || got.Declared != "testing" || got.Invalid {
		t.Fatalf("generic bracket naming a real branch: %+v", got)
	}
	got = hub.resolveTaskTargetBranch(ctx, "acme/widgets", "acme/widgets#4", "[quality] Fix", "")
	if got.Branch != "" || got.Declared != "" {
		t.Fatalf("generic bracket that is a lane: %+v", got)
	}
	if n := fx.count(); n != 1 {
		t.Fatalf("branch list must be fetched once per repo per sweep, got %d calls", n)
	}
	// A recorded override (from a wrong_base verdict) wins over the title.
	hub.recordTargetBranchOverride("acme/widgets#5", targetBranchOverride{Branch: "testing", RecordedAt: time.Now()})
	got = hub.resolveTaskTargetBranch(ctx, "acme/widgets", "acme/widgets#5", "[v6] Fix", "")
	if got.Branch != "testing" || got.Source != targetSourceVerdict {
		t.Fatalf("override must win: %+v", got)
	}
	// An expired override is forgotten.
	hub.recordTargetBranchOverride("acme/widgets#6", targetBranchOverride{Branch: "testing", RecordedAt: time.Now().Add(-targetBranchOverrideTTL - time.Hour)})
	if _, ok := hub.targetBranchOverride("acme/widgets#6"); ok {
		t.Fatal("expired override must not resolve")
	}
	// The knob turns the whole thing off.
	off := false
	s.deps.Config.Hub.ContributeHonourTargetBranch = &off
	got = hub.resolveTaskTargetBranch(ctx, "acme/widgets", "acme/widgets#1", "Fix", "**Target: v6.**")
	if got.Declared != "" || got.Branch != "" {
		t.Fatalf("knob off must ignore the declaration: %+v", got)
	}
}

func TestTargetBranch_ResolveWithoutRepositoryAccess(t *testing.T) {
	hub, s := covK2Hub(t)
	s.deps.Config.Project.Org = "acme"
	ctx := context.Background()
	// No lister and no GitHub client: a title/body declaration is trusted
	// unverified (the pre-#13146 trust the title prefix always had); a
	// generic bracket is not.
	got := hub.resolveTaskTargetBranch(ctx, "acme/widgets", "k1", "Fix", "Target: v6")
	if got.Branch != "v6" || got.Invalid {
		t.Fatalf("unverified body target: %+v", got)
	}
	got = hub.resolveTaskTargetBranch(ctx, "acme/widgets", "k2", "[testing] Fix", "")
	if got.Declared != "" {
		t.Fatalf("generic bracket without a lister: %+v", got)
	}
	// A failing lister is the same as no access, and does not poison the cache.
	fx := &branchListFixture{err: errors.New("boom")}
	hub.branchLister = fx.list
	got = hub.resolveTaskTargetBranch(ctx, "acme/widgets", "k3", "[v6] Fix", "")
	if got.Branch != "v6" || got.Invalid {
		t.Fatalf("lister failure must leave the declaration unverified: %+v", got)
	}
	if exists, checked := hub.branchExists(ctx, "acme/widgets", "v6"); exists || checked {
		t.Fatalf("branchExists on a failing lister = (%v,%v)", exists, checked)
	}
	if exists, checked := hub.branchExists(ctx, "acme/widgets", "bad name"); exists || !checked {
		t.Fatalf("an implausible name is decided without the repository: (%v,%v)", exists, checked)
	}
	// Hive's own repo falls back to the hub's build branch when the
	// declared target is missing.
	fx.err = nil
	fx.names = map[string][]string{"hivecommons/hive": {"v5"}}
	got = hub.resolveTaskTargetBranch(ctx, "hivecommons/hive", "k4", "Fix", "Target: v9")
	if got.Branch != upstreamBranch() || !got.Invalid || got.Declared != "v9" {
		t.Fatalf("own-repo fallback: %+v (want %q)", got, upstreamBranch())
	}
}

func TestTargetBranch_OverrideLedgerRoundTrip(t *testing.T) {
	hub, _ := covK2Hub(t)
	dir := t.TempDir()
	hub.persistTaskLedgers = true
	hub.targetBranchOverridesFile = filepath.Join(dir, "nested", targetBranchOverridesFileName)
	hub.recordTargetBranchOverride("acme/widgets#7", targetBranchOverride{Branch: "v6", DispatchedBase: "main", Reporter: "dev"})
	hub.recordTargetBranchOverride("acme/widgets#8", targetBranchOverride{Branch: "v6", RecordedAt: time.Now().Add(-targetBranchOverrideTTL - time.Hour)})
	hub.recordTargetBranchOverride("", targetBranchOverride{Branch: "v6"})
	hub.recordTargetBranchOverride("acme/widgets#9", targetBranchOverride{})
	data, err := os.ReadFile(hub.targetBranchOverridesFile)
	if err != nil {
		t.Fatalf("ledger not written: %v", err)
	}
	var saved map[string]targetBranchOverride
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved["acme/widgets#7"].Branch != "v6" || saved["acme/widgets#7"].RecordedAt.IsZero() {
		t.Fatalf("saved = %+v", saved)
	}
	other, _ := covK2Hub(t)
	other.persistTaskLedgers = true
	other.targetBranchOverridesFile = hub.targetBranchOverridesFile
	other.loadTargetBranchOverrides()
	if ov, ok := other.targetBranchOverride("acme/widgets#7"); !ok || ov.Reporter != "dev" || ov.DispatchedBase != "main" {
		t.Fatalf("reload = %+v (%v)", ov, ok)
	}
	// Persistence off: nothing is read or written.
	quiet, _ := covK2Hub(t)
	quiet.persistTaskLedgers = false
	quiet.targetBranchOverridesFile = filepath.Join(dir, "never.json")
	quiet.recordTargetBranchOverride("k", targetBranchOverride{Branch: "b"})
	quiet.loadTargetBranchOverrides()
	if _, err := os.Stat(quiet.targetBranchOverridesFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ledger must not be written with persistence off: %v", err)
	}
	// Garbage on disk is ignored.
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{nope"), 0o644)
	other.targetBranchOverridesFile = bad
	other.loadTargetBranchOverrides()
	if hub.targetBranchOverridesPath() == "" || (&ContributeWSHub{}).targetBranchOverridesPath() == "" {
		t.Fatal("path must always resolve")
	}
}

// The prompt names the declared branch, says why, and tells the agent to
// report wrong_base rather than already_done.
func TestTargetBranch_PromptText(t *testing.T) {
	ok := &taskTargetBranch{Branch: "v6", Declared: "v6", Source: targetSourceBody}
	body := buildTaskPromptBodyWithHints("acme/widgets", "acme/widgets#77", "Fix", "", "main", true, "", taskPromptRepoHints{Target: ok})
	for _, want := range []string{
		"Base this work on the 'v6' branch of acme/widgets",
		"gh pr create --base v6",
		"declares its target branch as 'v6' (a 'Target:' line in its body), and that branch exists in acme/widgets",
		"HIVE_VERDICT: no_work_needed — wrong_base: <branch> — <short reason>",
		"never print the already-done form for it",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(body, "'main' branch") {
		t.Error("the resolved target must replace the caller's base")
	}
	bad := &taskTargetBranch{Branch: "", Declared: "v9", Source: targetSourceTitle, Invalid: true}
	body = buildTaskPromptBodyWithHints("acme/widgets", "acme/widgets#77", "[v9] Fix", "", "main", false, "", taskPromptRepoHints{Target: bad})
	for _, want := range []string{
		"declares its target branch as 'v9' (its title prefix), but acme/widgets has no such branch",
		"so the base above is the repository's default instead",
		"gh repo view acme/widgets --json defaultBranchRef",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fallback prompt missing %q", want)
		}
	}
	verdictSrc := taskTargetBranch{Branch: "v6", Declared: "v6", Source: targetSourceVerdict}
	if note := verdictSrc.promptNote("acme/widgets"); !strings.Contains(note, "previous contributor's wrong_base verdict") {
		t.Fatalf("verdict source note: %q", note)
	}
	if note := (taskTargetBranch{Declared: "x", Source: "odd"}).promptNote("r"); !strings.Contains(note, "its metadata") {
		t.Fatalf("unknown source note: %q", note)
	}
	if note := (taskTargetBranch{}).promptNote("r"); note != "" {
		t.Fatalf("no declaration must add no note, got %q", note)
	}
	// Nil Target keeps the pre-#13146 wording byte for byte.
	plain := buildTaskPromptBodyWithHints("acme/widgets", "acme/widgets#77", "Fix", "", "main", true, "", taskPromptRepoHints{})
	if !strings.Contains(plain, "Base this work on the 'main' branch") || strings.Contains(plain, "declares its target branch") {
		t.Fatal("nil Target must keep the caller's base and add no note")
	}
}

func TestWrongBaseVerdict_Detection(t *testing.T) {
	cases := []struct {
		name, verdict, reason, kind string
		evidence                    *VerdictEvidence
		wantBranch                  string
		want                        bool
	}{
		{"structured kind with evidence", completionVerdictNoWorkNeeded, "code is on v6", verdictReasonKindWrongBase, &VerdictEvidence{Branch: "v6"}, "v6", true},
		{"structured kind, branch in reason", completionVerdictNoWorkNeeded, "wrong_base: v6 — code lives there", verdictReasonKindWrongBase, nil, "v6", true},
		{"structured kind, no branch", completionVerdictNoWorkNeeded, "no idea", verdictReasonKindWrongBase, nil, "", true},
		{"older relay reason text", completionVerdictNoWorkNeeded, "wrong_base: release-1.2 — files only on that branch", "", nil, "release-1.2", true},
		{"older relay dash spelling", completionVerdictIdle, "Wrong base - `v6` - see Target line", "", nil, "v6", true},
		{"already_done kind with wrong_base text", completionVerdictNoWorkNeeded, "wrong_base: v6 — x", verdictReasonKindAlreadyDone, nil, "v6", true},
		{"shipped is never wrong_base", completionVerdictShipped, "wrong_base: v6 — x", verdictReasonKindWrongBase, &VerdictEvidence{Branch: "v6"}, "", false},
		{"plain already_done", completionVerdictNoWorkNeeded, "merged PR #12 already resolves this", verdictReasonKindAlreadyDone, &VerdictEvidence{PR: 12}, "", false},
		{"other kind", completionVerdictNoWorkNeeded, "wrong_base: v6", "tooling", nil, "", false},
		{"unrelated reason", completionVerdictNoWorkNeeded, "nothing to do", "", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, ok := wrongBaseVerdict(tc.verdict, tc.reason, tc.kind, tc.evidence)
			if ok != tc.want || b != tc.wantBranch {
				t.Fatalf("wrongBaseVerdict = (%q,%v), want (%q,%v)", b, ok, tc.wantBranch, tc.want)
			}
		})
	}
	// The #8477 already-done classifier never fires for wrong_base, so
	// neither the label nor the close path can.
	if refs, done := alreadyDoneVerdictRefs("acme/widgets", 1, "wrong_base: v6 — already fixed by commit abc1234 on v6", verdictReasonKindWrongBase, &VerdictEvidence{Branch: "v6", Commit: "abc1234"}); done || refs != nil {
		t.Fatalf("wrong_base must not be already-done, got (%v,%v)", refs, done)
	}
	if _, done := alreadyDoneVerdictRefs("acme/widgets", 1, "wrong_base: v6 — merged PR #12 already resolves this", "", nil); done {
		t.Fatal("wrong_base reason text must not be already-done even when it cites a merged PR")
	}
	if _, done := alreadyDoneVerdictRefs("acme/widgets", 1, "merged PR #12 already resolves this", verdictReasonKindAlreadyDone, &VerdictEvidence{PR: 12}); !done {
		t.Fatal("a genuine already-done verdict must still classify")
	}
	if got := wrongBaseParkReason("v5", "v9"); got != "code not on v5; agent named v9 which does not exist" {
		t.Fatalf("park reason = %q", got)
	}
	if got := wrongBaseParkReason("", ""); got != "code not on the dispatched base; agent named no branch it lives on" {
		t.Fatalf("empty park reason = %q", got)
	}
	if !strings.Contains(wrongBaseRequeueComment("", "v5", "v6"), "a contributor's agent") || !strings.HasPrefix(wrongBaseParkComment("dev", "v5", "v9", "needs-decision"), wrongBaseMarker) {
		t.Fatal("comment bodies")
	}
}

// wrongBaseFixture captures the park and comment seams.
type wrongBaseFixture struct {
	mu       sync.Mutex
	parks    []string
	comments []string
	parkErr  error
	commErr  error
}

func (f *wrongBaseFixture) park(_ context.Context, repo string, number int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parks = append(f.parks, repo+"#"+itoa(number)+": "+reason)
	return f.parkErr
}

func (f *wrongBaseFixture) comment(_ context.Context, repo string, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, repo+"#"+itoa(number)+": "+body)
	return f.commErr
}

func (f *wrongBaseFixture) snapshot() (parks, comments []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.parks...), append([]string(nil), f.comments...)
}

func wrongBaseTask(base string) *WSTaskAssign {
	return &WSTaskAssign{TaskID: "ct-1", Kind: "issue", Repo: "acme/widgets", Number: 77, Key: "acme/widgets#77", BaseBranch: base}
}

func TestWrongBaseSettle_RequeuesOnExistingBranch(t *testing.T) {
	hub, _ := covK2Hub(t)
	fx := &wrongBaseFixture{}
	hub.wrongBaseParker, hub.wrongBaseCommenter = fx.park, fx.comment
	hub.branchLister = (&branchListFixture{names: map[string][]string{"acme/widgets": {"main", "v6"}}}).list
	task := wrongBaseTask("main")
	key := task.identityKey()
	// What the handler books before the settlement runs.
	hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictIdle, "dev", "wrong_base: v6 — x", completionSignalVerdict)

	hub.settleWrongBaseVerdict(task, "v6", "dev", "wrong_base: v6 — x")

	ov, ok := hub.targetBranchOverride(key)
	if !ok || ov.Branch != "v6" || ov.DispatchedBase != "main" || ov.Reporter != "dev" {
		t.Fatalf("override = %+v (%v)", ov, ok)
	}
	hub.completedMu.Lock()
	_, inCooldown := hub.completedTasks[key]
	_, hasVerdict := hub.noWorkVerdicts[key]
	hub.completedMu.Unlock()
	if inCooldown || hasVerdict {
		t.Fatalf("re-queue must clear the cooldown and any verdict row (cooldown=%v verdict=%v)", inCooldown, hasVerdict)
	}
	parks, comments := fx.snapshot()
	if len(parks) != 0 {
		t.Fatalf("an existing branch must not park: %v", parks)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "re-offered to contributors against `v6`") || !strings.Contains(comments[0], wrongBaseMarker) {
		t.Fatalf("comments = %v", comments)
	}
	// The next offer bases on v6.
	got := hub.resolveTaskTargetBranch(context.Background(), task.Repo, key, "Fix", "")
	if got.Branch != "v6" || got.Source != targetSourceVerdict {
		t.Fatalf("next dispatch must base on the override: %+v", got)
	}
	// A second wrong_base naming the SAME branch parks instead of looping.
	hub.settleWrongBaseVerdict(task, "v6", "dev", "wrong_base: v6 — still nothing")
	parks, _ = fx.snapshot()
	if len(parks) != 1 || !strings.Contains(parks[0], "code not on main; agent named v6 which does not exist") {
		t.Fatalf("repeat must park: %v", parks)
	}
}

func TestWrongBaseSettle_ParksOnMissingBranch(t *testing.T) {
	hub, _ := covK2Hub(t)
	fx := &wrongBaseFixture{}
	hub.wrongBaseParker, hub.wrongBaseCommenter = fx.park, fx.comment
	hub.branchLister = (&branchListFixture{names: map[string][]string{"acme/widgets": {"main", "v6"}}}).list
	task := wrongBaseTask("main")
	key := task.identityKey()

	hub.settleWrongBaseVerdict(task, "v9", "dev", "wrong_base: v9 — x")

	parks, comments := fx.snapshot()
	if len(parks) != 1 || parks[0] != "acme/widgets#77: code not on main; agent named v9 which does not exist" {
		t.Fatalf("parks = %v", parks)
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "Parked for a maintainer under `"+hub.configuredNeedsDecisionLabel()+"`") {
		t.Fatalf("comments = %v", comments)
	}
	hub.completedMu.Lock()
	rec, held := hub.noWorkVerdicts[key]
	hub.completedMu.Unlock()
	if !held || !rec.NeedsDecision || rec.Reason != parks[0][len("acme/widgets#77: "):] {
		t.Fatalf("park must book the needs-decision hold with the one-line reason, got %+v (%v)", rec, held)
	}
	if _, ok := hub.targetBranchOverride(key); ok {
		t.Fatal("a missing branch must not become an override")
	}
	// Naming the dispatched base itself, or no branch at all, parks too.
	hub.settleWrongBaseVerdict(task, "main", "dev", "")
	hub.settleWrongBaseVerdict(task, "", "dev", "")
	parks, _ = fx.snapshot()
	if len(parks) != 3 || !strings.Contains(parks[2], "agent named no branch") {
		t.Fatalf("parks = %v", parks)
	}
	// Seam errors are logged, never fatal.
	fx.parkErr, fx.commErr = errors.New("park"), errors.New("comment")
	hub.settleWrongBaseVerdict(task, "v9", "dev", "")
	// Ineligible tasks are ignored.
	hub.settleWrongBaseVerdict(nil, "v9", "dev", "")
	hub.settleWrongBaseVerdict(&WSTaskAssign{Kind: "pr-review", Repo: "acme/widgets"}, "v9", "dev", "")
	hub.settleWrongBaseVerdict(&WSTaskAssign{Kind: "issue", Repo: "acme/widgets", Number: 1, ExternalID: "ext"}, "v9", "dev", "")
	parks, _ = fx.snapshot()
	if len(parks) != 4 {
		t.Fatalf("ineligible tasks must not park: %v", parks)
	}
}

func TestWrongBaseSettle_UnverifiableBranchLeavesCooldown(t *testing.T) {
	hub, _ := covK2Hub(t)
	fx := &wrongBaseFixture{}
	hub.wrongBaseParker, hub.wrongBaseCommenter = fx.park, fx.comment
	hub.branchLister = (&branchListFixture{err: errors.New("api down")}).list
	task := wrongBaseTask("main")
	hub.settleWrongBaseVerdict(task, "v6", "dev", "")
	parks, comments := fx.snapshot()
	if len(parks) != 0 || len(comments) != 0 {
		t.Fatalf("an unverifiable branch must neither park nor re-queue: %v %v", parks, comments)
	}
	if _, ok := hub.targetBranchOverride(task.identityKey()); ok {
		t.Fatal("no override without confirmation")
	}
	// Without any repository access at all the hub cannot confirm the
	// branch either way; it parks rather than trusting the agent's word.
	bare, _ := covK2Hub(t)
	bare.wrongBaseParker, bare.wrongBaseCommenter = fx.park, fx.comment
	bare.settleWrongBaseVerdict(task, "v6", "dev", "")
	parks, _ = fx.snapshot()
	if len(parks) != 1 {
		t.Fatalf("no-access must park: %v", parks)
	}
	// Default seams without a GitHub client surface ErrNoGitHubClient.
	noSeams, _ := covK2Hub(t)
	if err := noSeams.parkWrongBase(context.Background(), "acme/widgets", 1, "r"); !errors.Is(err, ghpkg.ErrNoGitHubClient) {
		t.Fatalf("park without a client: %v", err)
	}
	if err := noSeams.commentWrongBaseOnce(context.Background(), "acme/widgets", 1, "b"); !errors.Is(err, ghpkg.ErrNoGitHubClient) {
		t.Fatalf("comment without a client: %v", err)
	}
}

// TestWrongBaseVerdict_WireShape drives the real task_complete handler with
// the completion a relay sends for a wrong_base verdict and checks the hub
// booked no already-done disposition, ran no already-done settlement,
// re-queued the issue against the named branch, and dispatched it there.
func TestWrongBaseVerdict_WireShape(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"dev"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register response: %v", err)
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "copilot"})
	readMsg(t, conn) // auth_ok

	issue := noWorkIssue(77, "Scanner misses the agent loader", time.Now().Add(-24*time.Hour))
	issue["body"] = "The loader is skipped.\n\n**Target: v6.**"
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{Name: "widgets", Full: "acme/widgets", ActionableIssues: []any{issue}}}}
	s.statusMu.Unlock()

	hub := s.contributeHub
	fx := &wrongBaseFixture{}
	hub.wrongBaseParker, hub.wrongBaseCommenter = fx.park, fx.comment
	// The repository has no v6 yet, so the declared target falls back and
	// the prompt says so.
	branches := &branchListFixture{names: map[string][]string{"acme/widgets": {"main", "v7"}}}
	hub.branchLister = branches.list
	alreadyDoneCalls := 0
	hub.alreadyDoneMarker = func(context.Context, string, int, ghpkg.IssueClaim, string, bool) error {
		alreadyDoneCalls++
		return nil
	}

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 77 {
		t.Fatalf("expected task_assign for #77, got %+v", assign)
	}
	if assign.BaseBranch != "" {
		t.Fatalf("declared v6 is missing from the repo, so the base must be the default (empty for an external repo): %q", assign.BaseBranch)
	}
	if !strings.Contains(assign.Prompt, "declares its target branch as 'v6'") || !strings.Contains(assign.Prompt, "has no such branch") {
		t.Fatalf("prompt must say the declared target fell back: %q", assign.Prompt)
	}

	// The agent finds the code on v7 and reports wrong_base, with an
	// already-done-looking commit citation that must NOT settle anything.
	hub.branchCache = branchListCache{}
	branches.names["acme/widgets"] = []string{"main", "v7"}
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: assign.TaskID, Result: "completed",
		CompletionSignal: completionSignalVerdict,
		Verdict:          "no_work_needed", VerdictReasonKind: verdictReasonKindWrongBase,
		VerdictReason: "wrong_base: v7 — already fixed by commit abc1234 on v7",
		Evidence:      &VerdictEvidence{Branch: "v7", Commit: "abc1234"},
	})
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	// The handler books idle first; the async settlement then clears it.
	hub.wrongBaseSettlements.Wait()
	next := readMsg(t, conn)
	for next.Type == "task_unavailable" {
		// The settlement may land after the ready was served; ask again.
		conn.WriteJSON(WSMessage{Type: "ready", Seq: 4})
		next = readMsg(t, conn)
	}
	if next.Type != "task_assign" || next.Number != 77 || next.BaseBranch != "v7" {
		t.Fatalf("issue must be re-offered against v7, got type=%s number=%d base=%q", next.Type, next.Number, next.BaseBranch)
	}
	if !strings.Contains(next.Prompt, "Base this work on the 'v7' branch of acme/widgets") || !strings.Contains(next.Prompt, "previous contributor's wrong_base verdict") {
		t.Fatalf("re-offer prompt must base on v7 and say why: %q", next.Prompt)
	}
	if alreadyDoneCalls != 0 {
		t.Fatal("wrong_base must never run the already-done marker")
	}
	parks, comments := fx.snapshot()
	if len(parks) != 0 || len(comments) != 1 {
		t.Fatalf("existing branch: parks=%v comments=%v", parks, comments)
	}
	key := noPRKey("acme/widgets", 77)
	hub.completedMu.Lock()
	_, verdictRow := hub.noWorkVerdicts[key]
	hub.completedMu.Unlock()
	if verdictRow {
		t.Fatal("wrong_base must not book the no_work suppression")
	}
	runs, err := readTaskRunsForUser(hub.taskRunLogPath(), "dev", 24*time.Hour, 10)
	if err != nil || len(runs) == 0 {
		t.Fatalf("task runs: %v (%d)", err, len(runs))
	}
	for _, r := range runs {
		if r.VerdictDisposition != "" || r.Verdict != completionVerdictIdle {
			t.Fatalf("run record must be idle with no already-done disposition: %+v", r)
		}
	}
}

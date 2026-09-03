package main

// Tests for the hold guard (#5589): a hold-gated PR whose branch moves before
// the hold lifts must not reach the merge lanes until a human re-reviews it.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/holdguard"
)

func swapHoldGuardStore(t *testing.T, s *holdguard.Store) {
	t.Helper()
	holdGuardStoreOnce.Do(func() {})
	old := holdGuardStore
	holdGuardStore = s
	t.Cleanup(func() {
		// Mirror swapEscalationStore: the Once is spent for the whole test
		// binary, so never restore a nil pointer — leaving the last injected
		// temp-backed store is safe AND keeps tests off the real /data ledger.
		if old != nil {
			holdGuardStore = old
		}
	})
}

func newTestHoldGuardStore(t *testing.T) *holdguard.Store {
	t.Helper()
	s := holdguard.Load(filepath.Join(t.TempDir(), "hold-guard.json"))
	swapHoldGuardStore(t, s)
	return s
}

// holdGuardServer fakes the three GitHub surfaces the guard touches: the PR
// commit list (snapshot/diff evidence), the drift comment, and the re-applied
// hold label.
type holdGuardServer struct {
	mu       sync.Mutex
	commits  map[int][]map[string]any // PR number -> commit list served
	comments []string
	labels   []string

	failComments bool
	failLabels   bool
	failCommits  bool
}

func (s *holdGuardServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/commits"):
			if s.failCommits {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			number := pathPRNumber(t, r.URL.Path)
			_ = json.NewEncoder(w).Encode(s.commits[number])
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			if s.failComments {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(body, &payload)
			s.comments = append(s.comments, payload.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			if s.failLabels {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var names []string
			if err := json.Unmarshal(body, &names); err != nil {
				var wrapped struct {
					Labels []string `json:"labels"`
				}
				_ = json.Unmarshal(body, &wrapped)
				names = wrapped.Labels
			}
			s.labels = append(s.labels, names...)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func pathPRNumber(t *testing.T, path string) int {
	t.Helper()
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "pulls" && i+1 < len(parts) {
			n, err := strconv.Atoi(parts[i+1])
			if err != nil {
				t.Fatalf("parse PR number from %q: %v", path, err)
			}
			return n
		}
	}
	t.Fatalf("no PR number in %q", path)
	return 0
}

func newHoldGuardClient(t *testing.T, fake *holdGuardServer) *github.Client {
	t.Helper()
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	return github.NewClientForTest(server.URL, "acme", []string{"widgets"}, discardLogger())
}

func ghCommit(sha, login, msg string) map[string]any {
	return map[string]any{
		"sha":    sha,
		"author": map[string]string{"login": login},
		"commit": map[string]any{"message": msg},
	}
}

func heldActionable(items ...github.HoldItem) *github.ActionableResult {
	a := &github.ActionableResult{}
	a.Hold.Items = items
	for _, it := range items {
		if it.Type == "pr" {
			a.Hold.PRs++
		}
	}
	a.Hold.Total = len(items)
	return a
}

// The re-applied label must be one the enumeration hold gate and both
// auto-merge sweeps actually recognize — otherwise re-holding is theater.
func TestReHoldLabelIsARecognizedHoldLabel(t *testing.T) {
	if !github.HasHoldLabel([]string{holdguard.ReHoldLabel}) {
		t.Fatalf("holdguard.ReHoldLabel = %q is not recognized by github.HasHoldLabel", holdguard.ReHoldLabel)
	}
}

func TestEnforceHoldGuardNilSafety(t *testing.T) {
	newTestHoldGuardStore(t)
	cfg := escalationTestConfig()
	if got := enforceHoldGuard(context.Background(), cfg, nil, nil, nil, discardLogger()); len(got) != 0 {
		t.Fatalf("nil actionable must return empty, got %v", got)
	}
}

func TestEnforceHoldGuardSnapshotsFirstHeldObservation(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("c2", "strategist-bot", "docs: refine")},
	}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()

	held := heldActionable(github.HoldItem{Repo: "widgets", Number: 7, Type: "pr", HeadSHA: "c2", Author: "strategist-bot"})
	if got := enforceHoldGuard(context.Background(), cfg, client, client, held, discardLogger()); len(got) != 0 {
		t.Fatalf("holding must not flag anything for re-review, got %v", got)
	}

	rec, ok := store.Recorded("acme/widgets", 7)
	if !ok || rec.HeadSHA != "c2" {
		t.Fatalf("snapshot = %+v ok=%v, want head c2 recorded under the org-qualified repo", rec, ok)
	}
	if len(rec.Authors) != 1 || rec.Authors[0] != "strategist-bot" {
		t.Fatalf("snapshot authors = %v, want [strategist-bot]", rec.Authors)
	}

	// Still held next tick, but the branch moved while held: the FIRST
	// snapshot must remain the baseline so the drift shows at lift time.
	fake.mu.Lock()
	fake.commits[7] = append(fake.commits[7], ghCommit("f1", "other-agent", "sneak: saas.go"))
	fake.mu.Unlock()
	held2 := heldActionable(github.HoldItem{Repo: "widgets", Number: 7, Type: "pr", HeadSHA: "f1", Author: "strategist-bot"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, held2, discardLogger())
	rec, _ = store.Recorded("acme/widgets", 7)
	if rec.HeadSHA != "c2" {
		t.Fatalf("snapshot head = %q after push-while-held, want the first-held baseline c2", rec.HeadSHA)
	}

	// Issue-type holds and headless holds record nothing.
	if len(fake.comments)+len(fake.labels) != 0 {
		t.Fatalf("snapshotting must not comment or label, saw %v / %v", fake.comments, fake.labels)
	}
	_ = enforceHoldGuard(context.Background(), cfg, client, client, heldActionable(
		github.HoldItem{Repo: "widgets", Number: 9, Type: "issue"},
		github.HoldItem{Repo: "widgets", Number: 10, Type: "pr"}, // no HeadSHA
	), discardLogger())
	if _, ok := store.Recorded("acme/widgets", 9); ok {
		t.Fatal("issue holds must not be snapshotted")
	}
	if _, ok := store.Recorded("acme/widgets", 10); ok {
		t.Fatal("headless PR holds must not be snapshotted")
	}
}

func TestEnforceHoldGuardCleanLiftClearsAndReopens(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan")},
	}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()

	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldActionable(github.HoldItem{Repo: "widgets", Number: 7, Type: "pr", HeadSHA: "c1"}), discardLogger())

	lifted := actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "c1", CIStatus: "success"})
	got := enforceHoldGuard(context.Background(), cfg, client, client, lifted, discardLogger())
	if len(got) != 0 {
		t.Fatalf("clean lift must not flag re-review, got %v", got)
	}
	if _, ok := store.Recorded("acme/widgets", 7); ok {
		t.Fatal("clean lift must clear the snapshot")
	}
	if len(fake.comments)+len(fake.labels) != 0 {
		t.Fatalf("clean lift must not comment or label, saw %v / %v", fake.comments, fake.labels)
	}
}

func TestEnforceHoldGuardDriftBlocksCommentsAndReholds(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan")},
	}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()

	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldActionable(github.HoldItem{Repo: "widgets", Number: 7, Type: "pr", HeadSHA: "c1"}), discardLogger())

	// Foreign commits landed while hold-gated; then the hold lifted.
	fake.mu.Lock()
	fake.commits[7] = []map[string]any{
		ghCommit("c1", "strategist-bot", "docs: plan"),
		ghCommit("f1", "other-agent", "sneak: saas.go"),
		ghCommit("f2", "other-agent", "sneak: wrapper"),
	}
	fake.mu.Unlock()
	lifted := actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "f2", CIStatus: "success", Mergeable: github.MergeableYes})

	got := enforceHoldGuard(context.Background(), cfg, client, client, lifted, discardLogger())
	if !got["widgets/7"] {
		t.Fatalf("drift must flag the PR for re-review with writeMergeEligible's keying, got %v", got)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want exactly one drift comment", len(fake.comments))
	}
	comment := fake.comments[0]
	for _, want := range []string{"`other-agent`", "sneak: saas.go", "fresh review required"} {
		if !strings.Contains(comment, want) {
			t.Errorf("drift comment missing %q:\n%s", want, comment)
		}
	}
	if strings.Contains(comment, "@") {
		t.Fatalf("drift comment must never carry raw mentions:\n%s", comment)
	}
	if len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
		t.Fatalf("labels = %v, want exactly the re-applied %q", fake.labels, holdguard.ReHoldLabel)
	}
	rec, ok := store.Recorded("acme/widgets", 7)
	if !ok || rec.HeadSHA != "f2" || rec.Commented {
		t.Fatalf("snapshot after drift = %+v, want re-armed at f2 with a fresh comment budget", rec)
	}

	// The human read the evidence and lifted the re-applied hold without the
	// branch moving again: that IS the fresh approval — the guard clears.
	got = enforceHoldGuard(context.Background(), cfg, client, client, lifted, discardLogger())
	if len(got) != 0 {
		t.Fatalf("post-review lift must reopen the merge lanes, got %v", got)
	}
	if _, ok := store.Recorded("acme/widgets", 7); ok {
		t.Fatal("post-review lift must clear the snapshot")
	}
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want no second comment on the approved lift", len(fake.comments))
	}
}

func TestEnforceHoldGuardPlanningPRForeignCommitsRequireReReview(t *testing.T) {
	newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		55: {ghCommit("d1", "strategist-bot", "docs: add UPGRADE")},
	}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()

	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldActionable(github.HoldItem{Repo: "widgets", Number: 55, Type: "pr", HeadSHA: "d1", Author: "strategist-bot"}), discardLogger())

	fake.mu.Lock()
	fake.commits[55] = []map[string]any{
		ghCommit("d1", "strategist-bot", "docs: add UPGRADE"),
		ghCommit("x1", "sec-check-bot", "fix: harden saas token path"),
		ghCommit("x2", "quality-bot", "test: cover contaminated branch"),
	}
	fake.mu.Unlock()

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{
			Repo:      "widgets",
			Number:    55,
			Title:     "📖 docs: add v5 upgrade plan",
			Author:    "strategist-bot",
			HeadSHA:   "x2",
			CIStatus:  "success",
			Mergeable: github.MergeableYes,
		}), discardLogger())
	if !got["widgets/55"] {
		t.Fatalf("docs/planning PR with foreign commits must require re-review, got %v", got)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want one re-review evidence comment", len(fake.comments))
	}
	comment := fake.comments[0]
	for _, want := range []string{"`sec-check-bot`", "`quality-bot`", "fix: harden saas token path", "test: cover contaminated branch", "fresh review required"} {
		if !strings.Contains(comment, want) {
			t.Errorf("drift comment missing %q:\n%s", want, comment)
		}
	}
	if len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
		t.Fatalf("labels = %v, want re-applied hold", fake.labels)
	}
}

func TestEnforceHoldGuardLeavesUnheldPRsAlone(t *testing.T) {
	newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "agent", "feat: normal PR")},
	}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{
			Repo:      "widgets",
			Number:    7,
			Author:    "agent",
			HeadSHA:   "c1",
			CIStatus:  "success",
			Mergeable: github.MergeableYes,
		}), discardLogger())
	if len(got) != 0 {
		t.Fatalf("unheld PRs with no hold snapshot must not be gated, got %v", got)
	}
	if len(fake.comments)+len(fake.labels) != 0 {
		t.Fatalf("unheld PR must not receive comments or labels, saw %v / %v", fake.comments, fake.labels)
	}
}

func TestEnforceHoldGuardCommentFailureRetriesWithoutSilentRehold(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{
		failComments: true,
		commits: map[int][]map[string]any{
			7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("f1", "other-agent", "sneak")},
		},
	}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}})

	lifted := actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "f1"})
	got := enforceHoldGuard(context.Background(), cfg, client, client, lifted, discardLogger())
	if !got["widgets/7"] {
		t.Fatal("drift must stay excluded even while side effects fail")
	}
	// A re-hold with no evidence comment is exactly the silent state the
	// guard exists to prevent: no label until the comment lands.
	if len(fake.labels) != 0 {
		t.Fatalf("labels = %v, want none while the comment is failing", fake.labels)
	}
	rec, _ := store.Recorded("acme/widgets", 7)
	if rec.HeadSHA != "c1" || rec.Commented {
		t.Fatalf("snapshot = %+v, want untouched baseline for the retry", rec)
	}

	// Comment path recovers: evidence + label + re-arm all land.
	fake.mu.Lock()
	fake.failComments = false
	fake.mu.Unlock()
	_ = enforceHoldGuard(context.Background(), cfg, client, client, lifted, discardLogger())
	if len(fake.comments) != 1 || len(fake.labels) != 1 {
		t.Fatalf("comments=%d labels=%d, want the retried episode completed", len(fake.comments), len(fake.labels))
	}
	if rec, _ := store.Recorded("acme/widgets", 7); rec.HeadSHA != "f1" {
		t.Fatalf("snapshot = %+v, want re-armed at f1 after recovery", rec)
	}
}

func TestEnforceHoldGuardLabelFailureNeverReArmsAndNeverRecomments(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{
		failLabels: true,
		commits: map[int][]map[string]any{
			7: {ghCommit("f1", "other-agent", "sneak")},
		},
	}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}})

	lifted := actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "f1"})
	_ = enforceHoldGuard(context.Background(), cfg, client, client, lifted, discardLogger())
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want evidence posted before the failing label", len(fake.comments))
	}
	// Re-arming before the hold label sticks would make the NEXT tick read
	// the drifted head as clean and merge it — the snapshot must stay put.
	rec, _ := store.Recorded("acme/widgets", 7)
	if rec.HeadSHA != "c1" || !rec.Commented {
		t.Fatalf("snapshot = %+v, want old head kept and comment marked done", rec)
	}

	// Retry: no duplicate comment, label lands, snapshot re-arms.
	fake.mu.Lock()
	fake.failLabels = false
	fake.mu.Unlock()
	_ = enforceHoldGuard(context.Background(), cfg, client, client, lifted, discardLogger())
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want the drift episode commented exactly once", len(fake.comments))
	}
	if len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
		t.Fatalf("labels = %v, want the retried hold label", fake.labels)
	}
	if rec, _ := store.Recorded("acme/widgets", 7); rec.HeadSHA != "f1" {
		t.Fatalf("snapshot = %+v, want re-armed after the label landed", rec)
	}
}

func TestEnforceHoldGuardNilWriterStillExcludes(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{7: {ghCommit("f1", "other-agent", "sneak")}}}
	client := newHoldGuardClient(t, fake)
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", nil)

	lifted := actionableWith(github.PullRequest{Repo: "widgets", Number: 7, HeadSHA: "f1"})
	got := enforceHoldGuard(context.Background(), cfg, client, nil, lifted, discardLogger())
	if !got["widgets/7"] {
		t.Fatal("drift must be excluded even without a forge writer")
	}
	if rec, _ := store.Recorded("acme/widgets", 7); rec.HeadSHA != "c1" || rec.Commented {
		t.Fatalf("snapshot = %+v, want untouched until a writer exists", rec)
	}
}

// Drifted PRs must vanish from BOTH merge-eligible buckets, exactly like
// held PRs: not eligible (no sweep merges them) and not ci-failing (no fix
// agent pushes more commits onto an unreviewed branch).
func TestWriteMergeEligibleExcludesHoldDriftPRs(t *testing.T) {
	dir := t.TempDir()
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
	})

	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "acme/widgets", Number: 7, Title: "drifted-green", Author: "agent", CIStatus: "success", Mergeable: github.MergeableYes},
		{Repo: "acme/widgets", Number: 8, Title: "drifted-red", Author: "agent", CIStatus: "failure", FailingChecks: []string{"test"}},
		{Repo: "acme/widgets", Number: 9, Title: "clean", Author: "agent", CIStatus: "success", Mergeable: github.MergeableYes},
	}}}
	drift := map[string]bool{"acme/widgets/7": true, "acme/widgets/8": true}
	writeMergeEligible(actionable, github.HoldResult{}, "acme", nil, false, nil, false, nil, drift, discardLogger())

	var eligible struct {
		MergeEligible []struct {
			Number int `json:"number"`
		} `json:"merge_eligible"`
	}
	raw, err := os.ReadFile(mergeEligiblePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &eligible); err != nil {
		t.Fatal(err)
	}
	if len(eligible.MergeEligible) != 1 || eligible.MergeEligible[0].Number != 9 {
		t.Fatalf("merge_eligible = %+v, want only the clean PR 9", eligible.MergeEligible)
	}

	var failing struct {
		CIFailing []struct {
			Number int `json:"number"`
		} `json:"ci_failing"`
	}
	raw, err = os.ReadFile(ciFailingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &failing); err != nil {
		t.Fatal(err)
	}
	if len(failing.CIFailing) != 0 {
		t.Fatalf("ci_failing = %+v, want drifted red PR kept away from fix agents", failing.CIFailing)
	}
}

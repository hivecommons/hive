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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/holdguard"
	"github.com/hivecommons/hive/pkg/review"
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
	mu            sync.Mutex
	commits       map[int][]map[string]any // PR number -> commit list served
	issueComments map[int][]map[string]any
	issueEvents   map[int][]map[string]any
	timeline      map[int][]map[string]any    // PR number -> issue timeline (#13603 gate-lift provenance)
	checkSuites   map[string][]map[string]any // head SHA -> check suites (#13603: SHA-keyed, must not prove arrival)
	statuses      map[string][]map[string]any // head SHA -> commit statuses (#13603: SHA-keyed, must not prove arrival)
	comments      []string
	labels        []string

	failComments bool
	failLabels   bool
	failCommits  bool

	// updateBranchCalls counts PUT .../update-branch requests per PR number
	// (#10437: keeping a held PR mergeable).
	updateBranchCalls map[int]int
	failUpdateBranch  bool

	// updateBranchExpected records the expected_head_sha each update-branch
	// request carried; liveHead, when set for a PR, makes the endpoint refuse
	// a request whose expected_head_sha differs, as GitHub does when the head
	// moved after enumeration (#14537).
	updateBranchExpected map[int][]string
	liveHead             map[int]string

	// baseContains lists the SHAs the fake base branch contains; the compare
	// endpoint reports them "behind" the base and anything else "diverged"
	// (#14534: proving the merged parent of a hygiene merge is a base commit).
	baseContains map[string]bool
	compareCalls int

	// commitBySHA serves GET /repos/{o}/{r}/commits/{sha}, pulls serves
	// GET /repos/{o}/{r}/pulls/{n} and compareResult overrides the compare
	// payload for "base...head" (#15649: the decision carry proves a head
	// is the hive's own base refresh before accepting an approval on the
	// previous head). Unknown SHAs and PRs are 404s.
	commitBySHA   map[string]map[string]any
	pulls         map[int]map[string]any
	compareResult map[string]map[string]any
}

func (s *holdGuardServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/compare/"):
			s.compareCalls++
			spec := r.URL.Path[strings.Index(r.URL.Path, "/compare/")+len("/compare/"):]
			if payload, ok := s.compareResult[spec]; ok {
				_ = json.NewEncoder(w).Encode(payload)
				return
			}
			_, head, _ := strings.Cut(spec, "...")
			status := "diverged"
			if s.baseContains[head] {
				status = "behind"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": status})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/commits/") && !strings.Contains(r.URL.Path, "/pulls/"):
			sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			commit, ok := s.commitBySHA[sha]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(commit)
		case r.Method == http.MethodGet && holdGuardPullPath.MatchString(r.URL.Path):
			pr, ok := s.pulls[pathPRNumber(t, r.URL.Path)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(pr)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/commits"):
			if s.failCommits {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			number := pathPRNumber(t, r.URL.Path)
			_ = json.NewEncoder(w).Encode(s.commits[number])
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			number := pathIssueNumber(t, r.URL.Path)
			_ = json.NewEncoder(w).Encode(s.issueComments[number])
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			number := pathIssueNumber(t, r.URL.Path)
			_ = json.NewEncoder(w).Encode(s.issueEvents[number])
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/timeline"):
			number := pathIssueNumber(t, r.URL.Path)
			events := s.timeline[number]
			if events == nil {
				events = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(events)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-suites"):
			suites := s.checkSuites[pathCommitSHA(t, r.URL.Path)]
			if suites == nil {
				suites = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(suites), "check_suites": suites})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/statuses"):
			statuses := s.statuses[pathCommitSHA(t, r.URL.Path)]
			if statuses == nil {
				statuses = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(statuses)
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
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/update-branch"):
			number := pathPRNumber(t, r.URL.Path)
			if s.updateBranchCalls == nil {
				s.updateBranchCalls = map[int]int{}
			}
			s.updateBranchCalls[number]++
			var opts struct {
				ExpectedHeadSHA string `json:"expected_head_sha"`
			}
			if body, _ := io.ReadAll(r.Body); len(body) > 0 {
				_ = json.Unmarshal(body, &opts)
			}
			if s.updateBranchExpected == nil {
				s.updateBranchExpected = map[int][]string{}
			}
			s.updateBranchExpected[number] = append(s.updateBranchExpected[number], opts.ExpectedHeadSHA)
			if live, ok := s.liveHead[number]; ok && opts.ExpectedHeadSHA != "" && opts.ExpectedHeadSHA != live {
				http.Error(w, `{"message":"expected head sha didn't match current head ref."}`, http.StatusUnprocessableEntity)
				return
			}
			if s.failUpdateBranch {
				http.Error(w, `{"message":"Merge conflict"}`, http.StatusUnprocessableEntity)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
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

// holdGuardPullPath matches GET /repos/{o}/{r}/pulls/{n} and nothing under it.
var holdGuardPullPath = regexp.MustCompile(`/pulls/\d+$`)

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

// pathCommitSHA extracts the ref from /repos/{o}/{r}/commits/{ref}/... paths.
func pathCommitSHA(t *testing.T, path string) string {
	t.Helper()
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "commits" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	t.Fatalf("no commit ref in %q", path)
	return ""
}

func pathIssueNumber(t *testing.T, path string) int {
	t.Helper()
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "issues" && i+1 < len(parts) {
			n, err := strconv.Atoi(parts[i+1])
			if err != nil {
				t.Fatalf("parse issue number from %q: %v", path, err)
			}
			return n
		}
	}
	t.Fatalf("no issue number in %q", path)
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

// gateLiftTimeline builds the PR timeline the #13603 cases share: the PR was
// held at c1, Renovate force-pushed the rebase r1 at 06:25 (the server-stamped
// head_ref_force_pushed event names r1), and the gate label was removed at
// liftedAt by liftedBy. The `committed` entry carries a client-set committer
// date far in the past so that a reader trusting it would be caught.
func gateLiftTimeline(gateLabel, liftedBy, liftedByType, liftedAt string) []map[string]any {
	return []map[string]any{{
		"event":      "labeled",
		"label":      map[string]string{"name": gateLabel},
		"actor":      map[string]string{"login": "kubestellar-hive[bot]", "type": "Bot"},
		"created_at": "2026-10-22T05:00:00Z",
	}, {
		"event":     "committed",
		"sha":       "r1",
		"committer": map[string]string{"name": "mergeraptor[bot]", "date": "2020-01-01T00:00:00Z"},
	}, {
		"event":      "head_ref_force_pushed",
		"commit_id":  "r1",
		"actor":      map[string]string{"login": "mergeraptor[bot]", "type": "Bot"},
		"created_at": "2026-10-22T06:25:01Z",
	}, {
		"event":      "unlabeled",
		"label":      map[string]string{"name": gateLabel},
		"actor":      map[string]string{"login": liftedBy, "type": liftedByType},
		"created_at": liftedAt,
	}}
}

// #13603: the PR was rebased while held, then a maintainer reviewed the
// rebased head and lifted the gate. That lift covers the current head — no
// integrity comment, no re-hold, snapshot cleared.
func TestEnforceHoldGuardHumanLiftAfterRebaseAcceptsCurrentHead(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("r1", "mergeraptor[bot]", "chore(deps): update")},
	}}
	fake.timeline = map[int][]map[string]any{
		7: gateLiftTimeline("sentinel-alert", "clubanderson", "User", "2026-10-22T18:19:00Z"),
	}
	client := newHoldGuardClient(t, fake)
	client.SetAppBotLogin("kubestellar-hive[bot]")
	client.SetSentinelAlertLabel("sentinel-alert")
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "mergeraptor[bot]"}})

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "mergeraptor[bot]", HeadSHA: "r1"}), discardLogger())
	if len(got) != 0 {
		t.Fatalf("human lift after the rebase must reopen the merge lanes, got %v", got)
	}
	if len(fake.comments)+len(fake.labels) != 0 {
		t.Fatalf("human lift after the rebase must not comment or re-hold, saw %v / %v", fake.comments, fake.labels)
	}
	if _, ok := store.Recorded("acme/widgets", 7); ok {
		t.Fatal("human lift after the rebase must clear the snapshot")
	}
}

// #13603 acceptance: a human lifted the gate, THEN the branch moved. That is
// the attack the guard exists for — drift handling is unchanged.
func TestEnforceHoldGuardPushAfterHumanLiftStillReholds(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("f1", "other-agent", "sneak")},
	}}
	fake.timeline = map[int][]map[string]any{
		// Lift at 06:00, before the 06:25 rebase/force-push; the force-push
		// event (server-stamped) names f1 as the head it produced.
		7: append(gateLiftTimeline("hold", "clubanderson", "User", "2026-10-22T06:00:00Z"), map[string]any{
			"event":      "head_ref_force_pushed",
			"commit_id":  "f1",
			"actor":      map[string]string{"login": "other-agent", "type": "User"},
			"created_at": "2026-10-22T06:25:02Z",
		}),
	}
	client := newHoldGuardClient(t, fake)
	client.SetAppBotLogin("kubestellar-hive[bot]")
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}})

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "f1"}), discardLogger())
	if !got["widgets/7"] {
		t.Fatalf("push after the human lift must still be re-review-gated, got %v", got)
	}
	if len(fake.comments) != 1 || len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
		t.Fatalf("comments=%v labels=%v, want normal holdguard re-hold", fake.comments, fake.labels)
	}
	if rec, ok := store.Recorded("acme/widgets", 7); !ok || rec.HeadSHA != "f1" {
		t.Fatalf("snapshot after drift = %+v, want re-armed at f1", rec)
	}
}

// #13603 / maintainer decision: an ordinary (non-force) push after the human
// lift whose commit carries a BACKDATED committer date must still re-hold.
// The hive never observed f1 while held and no force-push event names it.
func TestEnforceHoldGuardBackdatedCommitPushedAfterLiftStillReholds(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("f1", "other-agent", "sneak")},
	}}
	tl := gateLiftTimeline("hold", "clubanderson", "User", "2026-10-22T18:19:00Z")
	// Ordinary push of f1 (no force-push event) with a committer date well
	// before the lift.
	tl = append(tl, map[string]any{
		"event":     "committed",
		"sha":       "f1",
		"committer": map[string]string{"name": "other-agent", "date": "2026-10-22T07:00:00Z"},
	})
	fake.timeline = map[int][]map[string]any{7: tl}
	fake.checkSuites = map[string][]map[string]any{
		"f1": {{"id": 1, "head_sha": "f1", "created_at": "2026-10-22T19:00:00Z"}},
	}
	client := newHoldGuardClient(t, fake)
	client.SetAppBotLogin("kubestellar-hive[bot]")
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}})

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "f1"}), discardLogger())
	if !got["widgets/7"] {
		t.Fatalf("backdated commit pushed after the human lift must still be re-review-gated, got %v", got)
	}
	if len(fake.comments) != 1 || len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
		t.Fatalf("comments=%v labels=%v, want normal holdguard re-hold", fake.comments, fake.labels)
	}
	if rec, ok := store.Recorded("acme/widgets", 7); !ok || rec.HeadSHA != "f1" {
		t.Fatalf("snapshot after drift = %+v, want re-armed at f1", rec)
	}
}

// ordinaryPushLiftTimeline is the timeline for an ordinary (non-force) push
// of p1 followed by a human lift at 18:19: no event names p1 by a
// server-stamped time, and its committer date is irrelevant.
func ordinaryPushLiftTimeline() []map[string]any {
	return []map[string]any{{
		"event":      "labeled",
		"label":      map[string]string{"name": "hold"},
		"actor":      map[string]string{"login": "kubestellar-hive[bot]", "type": "Bot"},
		"created_at": "2026-10-22T05:00:00Z",
	}, {
		"event":     "committed",
		"sha":       "p1",
		"committer": map[string]string{"name": "strategist-bot", "date": "2026-10-23T00:00:00Z"},
	}, {
		"event":      "unlabeled",
		"label":      map[string]string{"name": "hold"},
		"actor":      map[string]string{"login": "clubanderson", "type": "User"},
		"created_at": "2026-10-22T18:19:00Z",
	}}
}

// #13603: an ordinary push during the hold that the hive itself observed on
// the PR while the gate was still on, followed by a human lift, is covered.
func TestEnforceHoldGuardHumanLiftAfterOrdinaryPushAcceptsCurrentHead(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("p1", "strategist-bot", "docs: fix typo")},
	}}
	fake.timeline = map[int][]map[string]any{7: ordinaryPushLiftTimeline()}
	client := newHoldGuardClient(t, fake)
	client.SetAppBotLogin("kubestellar-hive[bot]")
	cfg := escalationTestConfig()

	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldActionable(github.HoldItem{Repo: "widgets", Number: 7, Type: "pr", HeadSHA: "c1"}), discardLogger())
	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldActionable(github.HoldItem{Repo: "widgets", Number: 7, Type: "pr", HeadSHA: "p1"}), discardLogger())
	if rec, ok := store.Recorded("acme/widgets", 7); !ok || rec.HeadSHA != "c1" || rec.ObservedHeldHeadSHA != "p1" {
		t.Fatalf("snapshot = %+v ok=%v, want baseline c1 and observed held head p1", rec, ok)
	}

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "p1"}), discardLogger())
	if len(got) != 0 {
		t.Fatalf("human lift on the head observed while held must reopen the merge lanes, got %v", got)
	}
	if len(fake.comments)+len(fake.labels) != 0 {
		t.Fatalf("human lift on the observed head must not comment or re-hold, saw %v / %v", fake.comments, fake.labels)
	}
	if _, ok := store.Recorded("acme/widgets", 7); ok {
		t.Fatal("human lift on the observed head must clear the snapshot")
	}
}

// #13603 review: check suites and statuses are keyed by SHA, not branch. A
// SHA that already existed in the repository before the lift (scratch branch,
// another PR) carries pre-lift created_at times even when it is fast-forwarded
// onto the PR branch after the lift. The hive never saw it on the PR while
// held, so it must re-hold rather than be accepted.
func TestEnforceHoldGuardPreexistingSHAFastForwardedAfterLiftStillReholds(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("p1", "other-agent", "sneak")},
	}}
	fake.timeline = map[int][]map[string]any{7: ordinaryPushLiftTimeline()}
	fake.checkSuites = map[string][]map[string]any{
		"p1": {{"id": 1, "head_sha": "p1", "created_at": "2026-10-22T06:30:00Z"}},
	}
	fake.statuses = map[string][]map[string]any{
		"p1": {{"id": 1, "state": "success", "created_at": "2026-10-22T06:30:00Z"}},
	}
	client := newHoldGuardClient(t, fake)
	client.SetAppBotLogin("kubestellar-hive[bot]")
	cfg := escalationTestConfig()

	_ = enforceHoldGuard(context.Background(), cfg, client, client,
		heldActionable(github.HoldItem{Repo: "widgets", Number: 7, Type: "pr", HeadSHA: "c1"}), discardLogger())

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "p1"}), discardLogger())
	if !got["widgets/7"] {
		t.Fatalf("a pre-existing SHA fast-forwarded after the lift must still be re-review-gated, got %v", got)
	}
	if len(fake.comments) != 1 || len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
		t.Fatalf("comments=%v labels=%v, want normal holdguard re-hold", fake.comments, fake.labels)
	}
	if rec, ok := store.Recorded("acme/widgets", 7); !ok || rec.HeadSHA != "p1" {
		t.Fatalf("snapshot after drift = %+v, want re-armed at p1", rec)
	}
}

// #13603: with no evidence the head predates the lift (no force-push event
// naming it, never observed by the hive while held) the guard fails closed
// even though the commit's committer date predates the human lift.
func TestEnforceHoldGuardNoServerPushEvidenceFailsClosed(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("f1", "other-agent", "sneak")},
	}}
	tl := gateLiftTimeline("hold", "clubanderson", "User", "2026-10-22T18:19:00Z")
	tl = append(tl, map[string]any{
		"event":     "committed",
		"sha":       "f1",
		"committer": map[string]string{"name": "other-agent", "date": "2026-10-22T07:00:00Z"},
	})
	fake.timeline = map[int][]map[string]any{7: tl}
	client := newHoldGuardClient(t, fake)
	client.SetAppBotLogin("kubestellar-hive[bot]")
	cfg := escalationTestConfig()
	store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}})

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "f1"}), discardLogger())
	if !got["widgets/7"] {
		t.Fatalf("no server push evidence must fail closed into re-review, got %v", got)
	}
	if len(fake.comments) != 1 || len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
		t.Fatalf("comments=%v labels=%v, want normal holdguard re-hold", fake.comments, fake.labels)
	}
}

// #13603 acceptance: a lift by one of the hive's own accounts (or any bot)
// is never a human's review of the head — drift handling is unchanged.
func TestEnforceHoldGuardHiveAccountLiftStillReholds(t *testing.T) {
	for _, actor := range []struct{ login, typ string }{
		{"kubestellar-hive[bot]", "Bot"},
		{"renovate[bot]", "Bot"},
	} {
		t.Run(actor.login, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			fake := &holdGuardServer{commits: map[int][]map[string]any{
				7: {ghCommit("r1", "mergeraptor[bot]", "chore(deps): update")},
			}}
			fake.timeline = map[int][]map[string]any{
				7: gateLiftTimeline("hold", actor.login, actor.typ, "2026-10-22T18:19:00Z"),
			}
			client := newHoldGuardClient(t, fake)
			client.SetAppBotLogin("kubestellar-hive[bot]")
			cfg := escalationTestConfig()
			store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "mergeraptor[bot]"}})

			got := enforceHoldGuard(context.Background(), cfg, client, client,
				actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "mergeraptor[bot]", HeadSHA: "r1"}), discardLogger())
			if !got["widgets/7"] {
				t.Fatalf("non-human lift must still be re-review-gated, got %v", got)
			}
			if len(fake.comments) != 1 || len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
				t.Fatalf("comments=%v labels=%v, want normal holdguard re-hold", fake.comments, fake.labels)
			}
		})
	}
}

func TestEnforceHoldGuardSkipsSelfAuthorizationReholdWhenDisabled(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("f1", "other-agent", "sneak")},
	}}
	fake.issueComments = map[int][]map[string]any{
		7: {{
			"body":       github.SelfAuthorizationNoticeMarker,
			"user":       map[string]string{"login": "kubestellar-hive[bot]"},
			"created_at": "2026-09-11T12:00:00Z",
		}},
	}
	fake.issueEvents = map[int][]map[string]any{
		7: {{
			"event":      "labeled",
			"label":      map[string]string{"name": "hold"},
			"actor":      map[string]string{"login": "kubestellar-hive[bot]"},
			"created_at": "2026-09-11T11:59:59Z",
		}, {
			"event":      "unlabeled",
			"label":      map[string]string{"name": "hold"},
			"actor":      map[string]string{"login": "clubanderson"},
			"created_at": "2026-09-11T12:10:00Z",
		}},
	}
	client := newHoldGuardClient(t, fake)
	client.SetAppBotLogin("kubestellar-hive[bot]")
	cfg := escalationTestConfig()
	disabled := false
	cfg.GitHub.SelfAuthorizationHold = &disabled
	store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}})

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "f1"}), discardLogger())
	if len(got) != 0 {
		t.Fatalf("disabled #5117 hold must not re-review-gate drifted self-authorization holds, got %v", got)
	}
	if len(fake.comments)+len(fake.labels) != 0 {
		t.Fatalf("disabled #5117 hold must not comment or re-apply hold, saw %v / %v", fake.comments, fake.labels)
	}
	if _, ok := store.Recorded("acme/widgets", 7); ok {
		t.Fatal("disabled #5117 hold should clear the holdguard snapshot")
	}
}

func TestEnforceHoldGuardPreservesLaterHumanHoldWhenDisabled(t *testing.T) {
	store := newTestHoldGuardStore(t)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "strategist-bot", "docs: plan"), ghCommit("f1", "other-agent", "sneak")},
	}}
	fake.issueComments = map[int][]map[string]any{
		7: {{
			"body":       github.SelfAuthorizationNoticeMarker,
			"user":       map[string]string{"login": "kubestellar-hive[bot]"},
			"created_at": "2026-09-11T12:00:00Z",
		}},
	}
	fake.issueEvents = map[int][]map[string]any{
		7: {{
			"event":      "labeled",
			"label":      map[string]string{"name": "hold"},
			"actor":      map[string]string{"login": "kubestellar-hive[bot]"},
			"created_at": "2026-09-11T11:59:59Z",
		}, {
			"event":      "unlabeled",
			"label":      map[string]string{"name": "hold"},
			"actor":      map[string]string{"login": "clubanderson"},
			"created_at": "2026-09-11T12:10:00Z",
		}, {
			"event":      "labeled",
			"label":      map[string]string{"name": "hold"},
			"actor":      map[string]string{"login": "clubanderson"},
			"created_at": "2026-09-11T12:20:00Z",
		}, {
			"event":      "unlabeled",
			"label":      map[string]string{"name": "hold"},
			"actor":      map[string]string{"login": "clubanderson"},
			"created_at": "2026-09-11T12:30:00Z",
		}},
	}
	client := newHoldGuardClient(t, fake)
	client.SetAppBotLogin("kubestellar-hive[bot]")
	cfg := escalationTestConfig()
	disabled := false
	cfg.GitHub.SelfAuthorizationHold = &disabled
	store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "strategist-bot"}})

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "strategist-bot", HeadSHA: "f1"}), discardLogger())
	if !got["widgets/7"] {
		t.Fatalf("later human hold must still be re-review-gated when #5117 hold is disabled, got %v", got)
	}
	if len(fake.comments) != 1 || len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
		t.Fatalf("comments=%v labels=%v, want normal holdguard re-hold", fake.comments, fake.labels)
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
	writeMergeEligible(actionable, github.HoldResult{}, "acme", nil, false, nil, false, nil, drift, nil, discardLogger())

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

	// Since hivecommons/hive#7438 a held (or drifted) RED PR still rides
	// ci-failing.json flagged held=true so its AUTHOR can repair it; the
	// shared repair queue drops held rows before rendering.
	var failing struct {
		CIFailing []struct {
			Number int  `json:"number"`
			Held   bool `json:"held"`
		} `json:"ci_failing"`
	}
	raw, err = os.ReadFile(ciFailingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &failing); err != nil {
		t.Fatal(err)
	}
	if len(failing.CIFailing) != 1 || failing.CIFailing[0].Number != 8 || !failing.CIFailing[0].Held {
		t.Fatalf("ci_failing = %+v, want only drifted red PR 8 flagged held", failing.CIFailing)
	}
}

// swapHoldGuardHumanChangeRequests points the guard's operator-approval
// lookup at a memory-only decision ledger for the test's lifetime.
func swapHoldGuardHumanChangeRequests(t *testing.T) *review.HumanChangeRequests {
	t.Helper()
	store := review.NewHumanChangeRequests("")
	old := holdGuardHumanChangeRequests
	holdGuardHumanChangeRequests = func() review.HumanChangeRequestStore { return store }
	t.Cleanup(func() { holdGuardHumanChangeRequests = old })
	return store
}

func recordHoldGuardDecision(t *testing.T, store *review.HumanChangeRequests, head, actor, kind string) {
	t.Helper()
	if err := store.Record(review.HumanChangeRequest{
		Repo: "acme/widgets", Number: 7, HeadSHA: head, Source: review.HumanChangeRequestSourceDecision,
		SourceID: 1, Actor: actor, Body: kind, Kind: kind,
	}); err != nil {
		t.Fatal(err)
	}
}

// hive#15482: the contributor pushed while the PR sat behind the sentinel
// gate; the operator then pressed Approve on the dashboard for that exact
// head and the hive removed the gate label on their behalf. The timeline
// shows a hive lift, which can never cover the head — but the head-keyed
// approval record by a trusted merger is the fresh review, so the guard must
// accept the head instead of re-applying `hold` over the approval.
func TestEnforceHoldGuardOperatorApprovalOnCurrentHeadAcceptsHiveLift(t *testing.T) {
	store := newTestHoldGuardStore(t)
	decisions := swapHoldGuardHumanChangeRequests(t)
	recordHoldGuardDecision(t, decisions, "p1", "clubanderson", review.HumanChangeRequestKindApprove)
	fake := &holdGuardServer{commits: map[int][]map[string]any{
		7: {ghCommit("c1", "odttlnt", "deploy: monitoring"), ghCommit("p1", "odttlnt", "deploy: patch secret")},
	}}
	fake.timeline = map[int][]map[string]any{
		7: gateLiftTimeline("sentinel-alert", "kubestellar-hive[bot]", "Bot", "2026-10-22T18:19:00Z"),
	}
	client := newHoldGuardClient(t, fake)
	client.SetAppBotLogin("kubestellar-hive[bot]")
	cfg := escalationTestConfig()
	cfg.Dashboard.AuthorizedUsers = []string{"clubanderson:owner"}
	store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "odttlnt"}})
	store.Touch("acme/widgets", 7, "p1")

	got := enforceHoldGuard(context.Background(), cfg, client, client,
		actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "odttlnt", HeadSHA: "p1"}), discardLogger())
	if got["widgets/7"] {
		t.Fatalf("head approved by a trusted merger must not be re-review-gated, got %v", got)
	}
	if len(fake.comments) != 0 || len(fake.labels) != 0 {
		t.Fatalf("comments=%v labels=%v, want no drift comment and no re-hold", fake.comments, fake.labels)
	}
	if _, ok := store.Recorded("acme/widgets", 7); ok {
		t.Fatal("snapshot must clear once the approved head is accepted")
	}
}

// hivecommons/hive#15649: the operator approved the PR on head c1 and the
// hive's require_current_base gate then merged the base into the branch —
// a merge commit by the App that left the PR's diff untouched. That head
// change is the hive's own doing, so the approval follows the PR onto m1
// and the guard accepts the head. A merge by a person, or a refresh that
// changed the diff, is drift as before.
func TestEnforceHoldGuardOperatorApprovalCarriesOverHiveBaseRefresh(t *testing.T) {
	files := func(blob string) []map[string]any {
		return []map[string]any{{"filename": "deploy/monitoring.yaml", "status": "modified", "sha": blob, "additions": 2, "deletions": 0, "changes": 2, "patch": "@@ " + blob}}
	}
	for _, tc := range []struct {
		name, mergeAuthor, newBlob string
		carried                    bool
	}{
		{name: "pure base refresh by the App", mergeAuthor: "kubestellar-hive[bot]", newBlob: "blob1", carried: true},
		{name: "base merged by a person", mergeAuthor: "odttlnt", newBlob: "blob1"},
		{name: "refresh that changed the diff", mergeAuthor: "kubestellar-hive[bot]", newBlob: "blob9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			decisions := swapHoldGuardHumanChangeRequests(t)
			recordHoldGuardDecision(t, decisions, "c1", "clubanderson", review.HumanChangeRequestKindApprove)
			fake := &holdGuardServer{commits: map[int][]map[string]any{
				7: {ghCommit("c1", "odttlnt", "deploy: monitoring"), ghCommit("m1", tc.mergeAuthor, "Merge branch 'main' into deploy")},
			}}
			fake.timeline = map[int][]map[string]any{
				7: gateLiftTimeline("sentinel-alert", "kubestellar-hive[bot]", "Bot", "2026-10-22T18:19:00Z"),
			}
			fake.pulls = map[int]map[string]any{7: {"number": 7, "head": map[string]any{"sha": "m1"}, "base": map[string]any{"ref": "main"}}}
			fake.commitBySHA = map[string]map[string]any{"m1": {
				"sha": "m1", "author": map[string]any{"login": tc.mergeAuthor}, "committer": map[string]any{"login": "web-flow"},
				"parents": []map[string]any{{"sha": "c1"}, {"sha": "b1"}},
			}}
			fake.compareResult = map[string]map[string]any{
				"main...c1": {"status": "ahead", "merge_base_commit": map[string]any{"sha": "b0"}, "files": files("blob1")},
				"main...m1": {"status": "ahead", "merge_base_commit": map[string]any{"sha": "b1"}, "files": files(tc.newBlob)},
			}
			client := newHoldGuardClient(t, fake)
			client.SetAppBotLogin("kubestellar-hive[bot]")
			cfg := escalationTestConfig()
			cfg.Dashboard.AuthorizedUsers = []string{"clubanderson:owner"}
			store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "odttlnt"}})

			got := enforceHoldGuard(context.Background(), cfg, client, client,
				actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "odttlnt", HeadSHA: "m1"}), discardLogger())
			if got["widgets/7"] == tc.carried {
				t.Fatalf("re-review-gated=%v, want %v", got["widgets/7"], !tc.carried)
			}
			if tc.carried {
				if len(fake.comments) != 0 || len(fake.labels) != 0 {
					t.Fatalf("comments=%v labels=%v, want no drift comment and no re-hold", fake.comments, fake.labels)
				}
				if _, ok := store.Recorded("acme/widgets", 7); ok {
					t.Fatal("snapshot must clear once the carried approval covers the head")
				}
				carried, ok := decisions.For("acme/widgets", 7, "m1")
				if !ok || !carried.IsApproval() || carried.CarriedFromHead != "c1" || carried.Actor != "clubanderson" {
					t.Fatalf("carried decision = %+v ok=%v", carried, ok)
				}
				return
			}
			if len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
				t.Fatalf("labels=%v, want re-hold", fake.labels)
			}
			if _, ok := decisions.For("acme/widgets", 7, "c1"); !ok {
				t.Fatal("the approval must stay on the head the person actually approved")
			}
		})
	}
}

// The approval must name the head on the branch now: one recorded on the
// pre-push head, carried from an adopted PR, or by someone outside the merger
// tier is not a review of the current tree and the drift path stands.
func TestEnforceHoldGuardOperatorApprovalMustCoverCurrentHeadByTrustedMerger(t *testing.T) {
	cases := []struct {
		name, head, actor string
		body              string
		users             []string
	}{
		{name: "approval on the old head", head: "c1", actor: "clubanderson", users: []string{"clubanderson:owner"}},
		{name: "carried approval on the current head", head: "p1", actor: "clubanderson", body: "approve (carried from #6 head original)", users: []string{"clubanderson:owner"}},
		{name: "approver below merger tier", head: "p1", actor: "visitor", users: []string{"visitor:read-write"}},
		{name: "approver not authorized", head: "p1", actor: "stranger", users: []string{"clubanderson:owner"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestHoldGuardStore(t)
			decisions := swapHoldGuardHumanChangeRequests(t)
			body := tc.body
			if body == "" {
				body = review.HumanChangeRequestKindApprove
			}
			if err := decisions.Record(review.HumanChangeRequest{
				Repo: "acme/widgets", Number: 7, HeadSHA: tc.head, Source: review.HumanChangeRequestSourceDecision,
				SourceID: 1, Actor: tc.actor, Body: body, Kind: review.HumanChangeRequestKindApprove,
			}); err != nil {
				t.Fatal(err)
			}
			fake := &holdGuardServer{commits: map[int][]map[string]any{
				7: {ghCommit("c1", "odttlnt", "deploy: monitoring"), ghCommit("p1", "odttlnt", "deploy: patch secret")},
			}}
			fake.timeline = map[int][]map[string]any{
				7: gateLiftTimeline("sentinel-alert", "kubestellar-hive[bot]", "Bot", "2026-10-22T18:19:00Z"),
			}
			client := newHoldGuardClient(t, fake)
			client.SetAppBotLogin("kubestellar-hive[bot]")
			cfg := escalationTestConfig()
			cfg.Dashboard.AuthorizedUsers = tc.users
			store.Snapshot("acme/widgets", 7, "c1", []holdguard.Commit{{SHA: "c1", Author: "odttlnt"}})

			got := enforceHoldGuard(context.Background(), cfg, client, client,
				actionableWith(github.PullRequest{Repo: "widgets", Number: 7, Author: "odttlnt", HeadSHA: "p1"}), discardLogger())
			if !got["widgets/7"] {
				t.Fatalf("must still be re-review-gated, got %v", got)
			}
			if len(fake.labels) != 1 || fake.labels[0] != holdguard.ReHoldLabel {
				t.Fatalf("labels=%v, want re-hold", fake.labels)
			}
		})
	}
}

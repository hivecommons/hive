package mergelane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghub "github.com/hivecommons/hive/pkg/github"
)

const (
	testRepo   = "acme/widgets"
	testBranch = "main"
)

var errBoom = errors.New("boom")

// fakeGH models one repo: a branch tip, PR heads, which tip each head
// contains, check runs per head and merge commits with their parents. Hooks
// fire inside a call (before it answers) on the call's n-th invocation, which
// is how tests change GitHub state in the middle of a round.
type fakeGH struct {
	tip       string
	prs       map[int]*PullRequest
	contained map[string]string
	required  ghub.BranchRulesResult
	runs      map[string]map[string]CheckRun
	parents   map[string][]string
	commitPRs map[string][]int
	errs      map[string]error
	autoLand  bool
	pending   map[int]string
	seq       int
	calls     []string
	counts    map[string]int
	hooks     map[string]map[int]func()
	updates   []string
	merges    []string
}

func newFake() *fakeGH {
	return &fakeGH{
		tip:       "t0",
		prs:       map[int]*PullRequest{},
		contained: map[string]string{},
		required:  ghub.BranchRulesResult{Required: map[string]bool{"build": true}, Known: true},
		runs:      map[string]map[string]CheckRun{},
		parents:   map[string][]string{},
		commitPRs: map[string][]int{},
		errs:      map[string]error{},
		autoLand:  true,
		pending:   map[int]string{},
		counts:    map[string]int{},
		hooks:     map[string]map[int]func(){},
	}
}

// addPR opens a green, up-to-date, mergeable PR into main.
func (f *fakeGH) addPR(n int, head string) *PullRequest {
	p := &PullRequest{Number: n, Head: head, Base: testBranch, Open: true, MergeableState: "clean"}
	f.prs[n] = p
	f.contained[head] = f.tip
	f.pass(head)
	return p
}

func (f *fakeGH) pass(head string) {
	f.setRun(head, "build", "completed", "success")
}

func (f *fakeGH) setRun(head, name, status, conclusion string) {
	if f.runs[head] == nil {
		f.runs[head] = map[string]CheckRun{}
	}
	f.runs[head][name] = CheckRun{Status: status, Conclusion: conclusion}
}

func (f *fakeGH) on(name string, n int, fn func()) {
	if f.hooks[name] == nil {
		f.hooks[name] = map[int]func(){}
	}
	f.hooks[name][n] = fn
}

func (f *fakeGH) call(name string) error {
	f.calls = append(f.calls, name)
	f.counts[name]++
	if fn := f.hooks[name][f.counts[name]]; fn != nil {
		fn()
	}
	return f.errs[name]
}

func (f *fakeGH) land(n int) {
	f.prs[n].Head = f.pending[n]
	delete(f.pending, n)
}

func (f *fakeGH) PullRequest(_ context.Context, _ string, n int) (PullRequest, error) {
	if err := f.call("PullRequest"); err != nil {
		return PullRequest{}, err
	}
	p, ok := f.prs[n]
	if !ok {
		return PullRequest{}, fmt.Errorf("PR #%d not found", n)
	}
	out := *p
	out.Labels = append([]string(nil), p.Labels...)
	return out, nil
}

func (f *fakeGH) BranchTip(_ context.Context, _, _ string) (string, error) {
	if err := f.call("BranchTip"); err != nil {
		return "", err
	}
	return f.tip, nil
}

func (f *fakeGH) HeadContains(_ context.Context, _, tip, head string) (bool, error) {
	if err := f.call("HeadContains"); err != nil {
		return false, err
	}
	return f.contained[head] == tip, nil
}

func (f *fakeGH) RequiredChecks(_ context.Context, _, _ string) ghub.BranchRulesResult {
	if err := f.call("RequiredChecks"); err != nil {
		return ghub.BranchRulesResult{Reason: err.Error()}
	}
	out := f.required
	out.Required = map[string]bool{}
	for name := range f.required.Required {
		out.Required[name] = true
	}
	return out
}

func (f *fakeGH) CheckRuns(_ context.Context, _, head string) (map[string]CheckRun, error) {
	if err := f.call("CheckRuns"); err != nil {
		return nil, err
	}
	out := map[string]CheckRun{}
	for name, run := range f.runs[head] {
		out[name] = run
	}
	return out, nil
}

func (f *fakeGH) UpdateBranch(_ context.Context, _ string, n int, expectedHead string) error {
	if err := f.call("UpdateBranch"); err != nil {
		return err
	}
	f.updates = append(f.updates, fmt.Sprintf("%d:%s", n, expectedHead))
	p := f.prs[n]
	if p.Head != expectedHead {
		return ErrHeadMoved
	}
	f.seq++
	head := fmt.Sprintf("%s-u%d", expectedHead, f.seq)
	f.contained[head] = f.tip
	if f.autoLand {
		p.Head = head
	} else {
		f.pending[n] = head
	}
	return nil
}

func (f *fakeGH) Merge(_ context.Context, _ string, n int, head string) (string, error) {
	if err := f.call("Merge"); err != nil {
		return "", err
	}
	f.merges = append(f.merges, fmt.Sprintf("%d:%s", n, head))
	p := f.prs[n]
	if p.Head != head {
		return "", ErrHeadMoved
	}
	f.seq++
	sha := fmt.Sprintf("m%d", f.seq)
	f.parents[sha] = []string{f.tip, head}
	f.tip = sha
	p.Merged, p.Open = true, false
	return sha, nil
}

func (f *fakeGH) CommitParents(_ context.Context, _, sha string) ([]string, error) {
	if err := f.call("CommitParents"); err != nil {
		return nil, err
	}
	return f.parents[sha], nil
}

func (f *fakeGH) PullRequestsForCommit(_ context.Context, _, sha string) ([]int, error) {
	if err := f.call("PullRequestsForCommit"); err != nil {
		return nil, err
	}
	return f.commitPRs[sha], nil
}

func (f *fakeGH) count(name string) int {
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}

// harness wires a Lane over a temp store, the fake and an injected clock.
type harness struct {
	t         *testing.T
	path      string
	f         *fakeGH
	now       time.Time
	lane      *Lane
	strategy  string
	autoMerge bool
	paused    bool
	override  time.Duration
	blocked   func(repo string, labels []string) string
	authErr   error
	events    []Event
	alerts    []Alert
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:         t,
		path:      filepath.Join(t.TempDir(), "mergelane", "fronts.json"),
		f:         newFake(),
		now:       time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
		strategy:  config.MergeStrategyHiveSerialized,
		autoMerge: true,
	}
	h.restart()
	return h
}

func (h *harness) options(store *Store) Options {
	return Options{
		Store:            store,
		GitHub:           h.f,
		Now:              func() time.Time { return h.now },
		TimeoutFor:       func(string) time.Duration { return h.override },
		Strategy:         func(string) string { return h.strategy },
		AutoMergeAllowed: func(string) bool { return h.autoMerge },
		Paused:           func(string) bool { return h.paused },
		Blocked: func(repo string, labels []string) string {
			if h.blocked != nil {
				return h.blocked(repo, labels)
			}
			return defaultBlocked(labels)
		},
		Audit:  func(e Event) { h.events = append(h.events, e) },
		Alert:  func(a Alert) { h.alerts = append(h.alerts, a) },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// restart simulates a Hive restart: a fresh store handle and a fresh Lane
// over the same file.
func (h *harness) restart() {
	h.t.Helper()
	store, err := OpenStore(h.path)
	if err != nil {
		h.t.Fatalf("OpenStore: %v", err)
	}
	l, err := New(h.options(store))
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	h.lane = l
}

func (h *harness) authorize(context.Context, string) error { return h.authErr }

func (h *harness) acquire(pr int) Decision {
	h.t.Helper()
	d, err := h.lane.Acquire(testRepo, testBranch, pr, "sweep")
	if err != nil {
		h.t.Fatalf("Acquire(#%d): %v", pr, err)
	}
	return d
}

func (h *harness) advance(pr int) Decision {
	h.t.Helper()
	d, err := h.lane.Advance(context.Background(), testRepo, testBranch, pr, h.authorize)
	if err != nil {
		h.t.Fatalf("Advance(#%d): %v", pr, err)
	}
	return d
}

func (h *harness) record() Record {
	h.t.Helper()
	rec, _, err := h.lane.Snapshot(testRepo, testBranch)
	if err != nil {
		h.t.Fatalf("Snapshot: %v", err)
	}
	return rec
}

func (h *harness) frontPR() int {
	if rec := h.record(); rec.Front != nil {
		return rec.Front.PR
	}
	return 0
}

func (h *harness) eventsFor(action string) []Event {
	var out []Event
	for _, e := range h.events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func expectOutcome(t *testing.T, d Decision, want Outcome, reasonContains string) {
	t.Helper()
	if d.Outcome != want {
		t.Fatalf("outcome = %q (reason %q), want %q", d.Outcome, d.Reason, want)
	}
	if reasonContains != "" && !strings.Contains(d.Reason, reasonContains) {
		t.Fatalf("reason = %q, want it to contain %q", d.Reason, reasonContains)
	}
}

func TestNewRequiresStoreAndGitHub(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without a store and client must fail")
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "fronts.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	l, err := New(Options{Store: store, GitHub: newFake()})
	if err != nil {
		t.Fatalf("New with defaults: %v", err)
	}
	if l.opts.DefaultTimeout != DefaultFrontTimeout || l.opts.StaleAfter != DefaultStaleAfter || l.opts.Now == nil || l.opts.Logger == nil {
		t.Fatalf("defaults not applied: %+v", l.opts)
	}
	if got := l.timeoutFor(testRepo); got != 60*time.Minute {
		t.Fatalf("hive-wide default front timeout = %s, want 60m", got)
	}
}

func TestOpenStoreRefusesBadFiles(t *testing.T) {
	if _, err := OpenStore("  "); err == nil {
		t.Fatal("an empty store path must be refused")
	}
	dir := t.TempDir()

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(corrupt); err == nil || !strings.Contains(err.Error(), "unparseable") {
		t.Fatalf("corrupt store: err = %v, want unparseable refusal", err)
	}
	if got, _ := os.ReadFile(corrupt); string(got) != "{nope" {
		t.Fatalf("corrupt store must be left untouched, got %q", got)
	}

	dup := filepath.Join(dir, "dup.json")
	body := `{"version":1,"lanes":[{"repo":"acme/widgets","branch":"main"},{"repo":"ACME/widgets","branch":"main"}]}`
	if err := os.WriteFile(dup, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(dup); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("duplicate lanes: err = %v, want conflicting refusal", err)
	}

	if _, err := OpenStore(dir); err == nil || !strings.Contains(err.Error(), "reading lane record") {
		t.Fatalf("directory as store: err = %v, want read error", err)
	}
}

func TestStoreErrorsFailClosed(t *testing.T) {
	h := newHarness(t)
	h.f.addPR(1, "h1")
	if err := os.WriteFile(h.path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.lane.Acquire(testRepo, testBranch, 1, "sweep"); err == nil {
		t.Fatal("Acquire over a corrupt record must fail")
	}
	if _, err := h.lane.Advance(context.Background(), testRepo, testBranch, 1, h.authorize); err == nil {
		t.Fatal("Advance over a corrupt record must fail")
	}
	if err := h.lane.Release(testRepo, testBranch, 1, ""); err == nil {
		t.Fatal("Release over a corrupt record must fail")
	}
	if _, _, err := h.lane.Snapshot(testRepo, testBranch); err == nil {
		t.Fatal("Snapshot over a corrupt record must fail")
	}
	if len(h.f.calls) != 0 || len(h.f.merges) != 0 {
		t.Fatalf("no GitHub call may happen without a readable front record, got %v", h.f.calls)
	}

	dir := filepath.Dir(h.path)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.lane.Acquire(testRepo, testBranch, 1, "sweep"); err == nil {
		t.Fatal("Acquire must fail when the record directory cannot be created")
	}
}

func TestGateArgumentsValidated(t *testing.T) {
	h := newHarness(t)
	if _, err := h.lane.Acquire("", testBranch, 1, "sweep"); err == nil {
		t.Fatal("Acquire without a repo must fail")
	}
	if _, err := h.lane.Advance(context.Background(), testRepo, " ", 1, h.authorize); err == nil {
		t.Fatal("Advance without a branch must fail")
	}
	if err := h.lane.Release(testRepo, testBranch, 0, "x"); err == nil {
		t.Fatal("Release without a PR must fail")
	}
}

func TestSnapshotMissingLaneAndPersistence(t *testing.T) {
	h := newHarness(t)
	if _, ok, err := h.lane.Snapshot(testRepo, "release"); ok || err != nil {
		t.Fatalf("missing lane: ok=%v err=%v", ok, err)
	}
	h.f.addPR(1, "h1")
	h.f.addPR(2, "h2")
	h.acquire(1)
	h.now = h.now.Add(time.Minute)
	h.acquire(2)
	reopened, err := OpenStore(h.path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	rec, ok, err := reopened.Snapshot(testRepo, testBranch)
	if err != nil || !ok {
		t.Fatalf("Snapshot after reopen: ok=%v err=%v", ok, err)
	}
	if rec.Front == nil || rec.Front.PR != 1 || len(rec.Waiting) != 1 || rec.Waiting[0].PR != 2 {
		t.Fatalf("durable lane = %+v, want front #1 and #2 waiting", rec)
	}
}

func TestNonFrontCallerIsDeferredWithoutGitHubCalls(t *testing.T) {
	h := newHarness(t)
	h.f.addPR(1, "h1")
	p2 := h.f.addPR(2, "h2")
	h.f.contained[p2.Head] = "old"

	expectOutcome(t, h.acquire(1), OutcomeFront, "")
	d := h.acquire(2)
	expectOutcome(t, d, OutcomeDeferred, ReasonNotAtFront)
	if d.Position != 1 {
		t.Fatalf("position = %d, want 1", d.Position)
	}
	d = h.advance(2)
	expectOutcome(t, d, OutcomeDeferred, ReasonNotAtFront)
	if len(h.f.calls) != 0 {
		t.Fatalf("a non-front PR must cause no GitHub call (R16), got %v", h.f.calls)
	}
	expectOutcome(t, h.advance(3), OutcomeDeferred, ReasonNotAtFront)

	h.acquire(2)
	if got := len(h.eventsFor(ActionDeferred)); got != 1 {
		t.Fatalf("deferral audited %d times, want once on joining", got)
	}
}

func TestStaleWaiterIsPruned(t *testing.T) {
	h := newHarness(t)
	for n := 1; n <= 3; n++ {
		h.f.addPR(n, fmt.Sprintf("h%d", n))
	}
	h.acquire(1)
	h.acquire(2)
	h.now = h.now.Add(DefaultStaleAfter + time.Minute)
	h.acquire(1)
	h.acquire(3)
	rec := h.record()
	if len(rec.Waiting) != 1 || rec.Waiting[0].PR != 3 {
		t.Fatalf("waiting = %+v, want only #3 after #2 went stale", rec.Waiting)
	}
}

func TestReleaseWaiterAndFront(t *testing.T) {
	h := newHarness(t)
	for n := 1; n <= 3; n++ {
		h.f.addPR(n, fmt.Sprintf("h%d", n))
		h.acquire(n)
	}
	if err := h.lane.Release(testRepo, testBranch, 3, "no longer eligible"); err != nil {
		t.Fatalf("Release waiter: %v", err)
	}
	if err := h.lane.Release(testRepo, testBranch, 1, ""); err != nil {
		t.Fatalf("Release front: %v", err)
	}
	rec := h.record()
	if rec.Front == nil || rec.Front.PR != 2 || len(rec.Waiting) != 0 {
		t.Fatalf("lane = %+v, want #2 at the front and nobody waiting", rec)
	}
	if rec.LastExit == nil || rec.LastExit.PR != 1 || rec.LastExit.Reason != "released by its merge path" {
		t.Fatalf("last exit = %+v", rec.LastExit)
	}
}

func TestRefusalAuditedOncePerReason(t *testing.T) {
	h := newHarness(t)
	h.f.addPR(1, "h1")
	h.f.setRun("h1", "build", "in_progress", "")
	h.acquire(1)
	h.advance(1)
	h.advance(1)
	if got := len(h.eventsFor(ActionRefusal)); got != 1 {
		t.Fatalf("same wait reason audited %d times, want 1", got)
	}
	if st := h.record().Front.Stage; st != StageWaitingChecks {
		t.Fatalf("stage = %q, want %q", st, StageWaitingChecks)
	}
}

func TestNilPolicyCallbacksFailClosed(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "fronts.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	f.addPR(1, "h1")
	l, err := New(Options{Store: store, GitHub: f, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(testRepo, testBranch, 1, "sweep"); err != nil {
		t.Fatal(err)
	}
	d, err := l.Advance(context.Background(), testRepo, testBranch, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectOutcome(t, d, OutcomeLeft, "merge strategy")
	if len(f.calls) != 0 || len(f.merges) != 0 {
		t.Fatalf("deciding the strategy needs no GitHub call, got %v", f.calls)
	}

	l.opts.Strategy = func(string) string { return config.MergeStrategyHiveSerialized }
	if _, err := l.Acquire(testRepo, testBranch, 1, "sweep"); err != nil {
		t.Fatal(err)
	}
	d, _ = l.Advance(context.Background(), testRepo, testBranch, 1, nil)
	expectOutcome(t, d, OutcomeLeft, "auto-merge is not allowed")

	l.opts.AutoMergeAllowed = func(string) bool { return true }
	if _, err := l.Acquire(testRepo, testBranch, 1, "sweep"); err != nil {
		t.Fatal(err)
	}
	d, _ = l.Advance(context.Background(), testRepo, testBranch, 1, nil)
	expectOutcome(t, d, OutcomeWaiting, "no merge-path authorization")
	if len(f.merges) != 0 {
		t.Fatalf("no merge without an authorization callback, got %v", f.merges)
	}
}

func TestDefaultBlocked(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		want   string
	}{
		{nil, ""},
		{[]string{"enhancement", "hive/abc"}, ""},
		{[]string{"hold"}, "hold label"},
		{[]string{"on-hold"}, "hold label"},
		{[]string{"do-not-merge"}, "do-not-merge"},
		{[]string{"Hive-Pause/hive-1"}, "pause label"},
	} {
		got := defaultBlocked(tc.labels)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("defaultBlocked(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}

func TestMergeability(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		state     string
		mergeable *bool
		reason    string
		leave     bool
	}{
		{"clean", nil, "", false},
		{"unstable", nil, "", false},
		{"unknown", nil, "not yet known", false},
		{"unknown", &yes, "", false},
		{"dirty", nil, "merge conflicts", true},
		{"blocked", nil, "not mergeable", false},
	} {
		reason, leave := mergeability(PullRequest{MergeableState: tc.state, Mergeable: tc.mergeable})
		if leave != tc.leave || (tc.reason == "") != (reason == "") || !strings.Contains(reason, tc.reason) {
			t.Errorf("mergeability(%q) = %q, %v; want %q, %v", tc.state, reason, leave, tc.reason, tc.leave)
		}
	}
}

func TestShort(t *testing.T) {
	if got := short("0123456789abcdef"); got != "0123456789ab" {
		t.Fatalf("short = %q", got)
	}
	if got := short("abc"); got != "abc" {
		t.Fatalf("short = %q", got)
	}
}

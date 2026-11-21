package automerge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// forkGuardGateEnv is a fork PR stage pass over a fake GitHub whose check
// runs, annotations and check digest a test changes between passes.
type forkGuardGateEnv struct {
	t     *testing.T
	api   *stageAPI
	e     *Engine
	clock *contributorLaneClock
	sink  *fakeNudgeSink

	mu          sync.Mutex
	runs        map[string]string   // head → check-runs list
	annotations map[string]string   // check run id → annotations list
	red         map[string][]string // head → the digest's red checks
	reads       map[string]int      // check run id → annotation reads
	digestDown  bool                // the check digest read fails
}

func newForkGuardGateEnv(t *testing.T, at time.Time) *forkGuardGateEnv {
	t.Helper()
	api, closeAPI := seededStageAPI(t)
	t.Cleanup(closeAPI)
	env := &forkGuardGateEnv{
		t: t, api: api,
		clock:       &contributorLaneClock{at: at},
		sink:        &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "queued", Online: true}},
		runs:        map[string]string{},
		annotations: map[string]string{},
		red:         map[string][]string{},
		reads:       map[string]int{},
	}
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/commits/") && strings.HasSuffix(path, "/check-runs"):
			head := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/widget/commits/"), "/check-runs")
			env.mu.Lock()
			body, ok := env.runs[head]
			env.mu.Unlock()
			if !ok {
				return false
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return true
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/check-runs/") && strings.HasSuffix(path, "/annotations"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/widget/check-runs/"), "/annotations")
			env.mu.Lock()
			env.reads[id]++
			body := env.annotations[id]
			env.mu.Unlock()
			if body == "" {
				body = "[]"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return true
		}
		return false
	}
	// The index has read nothing for these heads.
	policy := StageLabelPolicy{Enabled: true, ContributorNudge: true, ForkGuard: func(string, int, string) []string { return nil }}
	env.e = newNudgeEngine(t, apiURLOf(t, api), env.clock, env.sink, policy)
	env.e.checkSummary = func(_ context.Context, _, _, sha string) (hgithub.CommitChecksSummary, error) {
		env.mu.Lock()
		defer env.mu.Unlock()
		if env.digestDown {
			return hgithub.CommitChecksSummary{}, errors.New("GET /commits/" + sha + "/check-runs: 500 Internal Server Error")
		}
		red := append([]string(nil), env.red[sha]...)
		return hgithub.CommitChecksSummary{Total: 5, Green: 5 - len(red), Red: red, DCO: 1}, nil
	}
	return env
}

func (env *forkGuardGateEnv) set(fn func()) {
	env.mu.Lock()
	defer env.mu.Unlock()
	fn()
}

func (env *forkGuardGateEnv) annotationReads(id string) int {
	env.mu.Lock()
	defer env.mu.Unlock()
	return env.reads[id]
}

// decisionComments are the 085 entry comments the pass posted.
func (env *forkGuardGateEnv) decisionComments() []string {
	env.api.mu.Lock()
	defer env.api.mu.Unlock()
	var out []string
	for _, c := range env.api.comments {
		if strings.HasPrefix(c, stagelabels.DecisionCommentMarker) {
			out = append(out, c)
		}
	}
	return out
}

// pass records the contributor lane's view of a fork PR head and runs one
// stage pass.
func (env *forkGuardGateEnv) pass(number int, head, reason string, labels ...string) stagelabels.Record {
	env.t.Helper()
	pr := nudgePR(number, head, labels...)
	pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("karibhou/widget")}
	env.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
	env.e.noteStageRole("acme", "widget", number, stagelabels.RoleContributor)
	if _, err := env.e.ApplyStageLabels(context.Background()); err != nil {
		env.t.Fatal(err)
	}
	rec, _ := env.e.stageStoreOrShared().Get(stageKey("acme", "widget", number))
	return rec
}

func forkGuardGateRuns(runs ...string) string {
	return fmt.Sprintf(`{"total_count":%d,"check_runs":[%s]}`, len(runs), strings.Join(runs, ","))
}

func forkGuardGateRun(id int, name, conclusion string) string {
	return fmt.Sprintf(`{"id":%d,"name":%q,"status":"completed","conclusion":%q}`, id, name, conclusion)
}

var (
	forkGuardGateCanaryAnnotations = fmt.Sprintf(`[{"annotation_level":"failure","message":"Process completed with exit code 1."},{"annotation_level":"failure","message":%q}]`, forkCanaryAnnotation)
	forkGuardGateUnitAnnotations   = `[{"annotation_level":"failure","message":"FAIL: TestWidget (0.01s)"}]`
)

// projectbluefin/utah-packages#1258 and #1263 (2026-11-21): fork PRs red
// only on the required Canary, which refuses fork PRs. The contributor lane
// stopped at a gate or fence before its CI step (`sentinel-blocked`,
// `workflow-path`), so no lane reason named a red check and foldForkGuard
// never read the head's annotations; the human route then read Canary off
// the head's digest as the author's to fix: 072 `check-failing:Canary`, a
// nudge, and on earlier fork PRs an adoption. Such a head now parks at 085
// on the fork guard, and the 085 note still names the gate.
func TestForkGuardGateLaneParksAt085(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		labels       []string
		keeps        string // a line the 085 note keeps besides the guard
	}{
		{"sentinel-blocked", contributorLaneReasonSentinelBlocked, []string{defaultSentinelLabel}, "a sentinel alert is open on this PR"},
		{"workflow-path", contributorLaneReasonWorkflowPath, nil, "CI workflow / action files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newForkGuardGateEnv(t, time.Date(2026, 11, 21, 10, 35, 0, 0, time.UTC))
			env.set(func() {
				env.runs["9c0d23d"] = forkGuardGateRuns(forkGuardGateRun(11, "Canary", "failure"), forkGuardGateRun(12, "Unit tests", "success"))
				env.annotations["11"] = forkGuardGateCanaryAnnotations
				env.red["9c0d23d"] = []string{"Canary"}
			})
			for i := 1; i <= 2; i++ {
				rec := env.pass(1263, "9c0d23d", tc.reason, tc.labels...)
				if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stageReasonForkGuard+"Canary" {
					t.Fatalf("pass %d: record %s %q, want 085 %q", i, rec.Stage, rec.Reason, stageReasonForkGuard+"Canary")
				}
				if len(env.sink.nudges) != 0 || len(nudgeComments(env.api)) != 0 {
					t.Fatalf("pass %d: author nudged for a check only a maintainer can run: sink=%+v comments=%v", i, env.sink.nudges, nudgeComments(env.api))
				}
				if rec.AdoptedHead != "" {
					t.Fatalf("pass %d: adopted %q, want no adoption", i, rec.AdoptedHead)
				}
				env.clock.advance(time.Minute)
			}
			if got := env.annotationReads("11"); got != 1 {
				t.Fatalf("Canary annotations read %d times, want once per head and red set", got)
			}
			notes := env.decisionComments()
			if len(notes) != 1 || !strings.Contains(notes[0], "`Canary` refuses pull requests from a fork") || !strings.Contains(notes[0], tc.keeps) {
				t.Fatalf("085 entry comments = %q, want one naming the Canary guard and %q", notes, tc.keeps)
			}
		})
	}
}

// A pass whose check digest cannot be read (GitHub answers 500; the
// failure is cached for humanChecksTTL) keeps a gate-stopped fork PR parked
// on its guard. The lane's gate reason names no red check, so nothing
// carried the recorded guard and the digest fallback found nothing red: a
// sentinel-blocked PR dropped to 060 "checks could not be read" for up to
// ten minutes and came back, and a workflow-path PR's recorded reason
// flipped between the guard and the fence.
func TestForkGuardGateLaneKeepsGuardWhileDigestUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		labels       []string
	}{
		{"sentinel-blocked", contributorLaneReasonSentinelBlocked, []string{defaultSentinelLabel}},
		{"workflow-path", contributorLaneReasonWorkflowPath, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newForkGuardGateEnv(t, time.Date(2026, 11, 21, 10, 35, 0, 0, time.UTC))
			env.set(func() {
				env.runs["9c0d23d"] = forkGuardGateRuns(forkGuardGateRun(11, "Canary", "failure"), forkGuardGateRun(12, "Unit tests", "success"))
				env.annotations["11"] = forkGuardGateCanaryAnnotations
				env.red["9c0d23d"] = []string{"Canary"}
			})
			want := stageReasonForkGuard + "Canary"
			if rec := env.pass(1263, "9c0d23d", tc.reason, tc.labels...); rec.Stage != stagelabels.NeedsDecision || rec.Reason != want {
				t.Fatalf("readable digest: record %s %q, want 085 %q", rec.Stage, rec.Reason, want)
			}

			env.set(func() { env.digestDown = true })
			env.clock.advance(humanChecksTTL + time.Minute)
			env.api.reset()
			rec := env.pass(1263, "9c0d23d", tc.reason, tc.labels...)
			if rec.Stage != stagelabels.NeedsDecision || rec.Reason != want {
				t.Fatalf("unreadable digest: record %s %q, want 085 %q kept", rec.Stage, rec.Reason, want)
			}
			if len(env.sink.nudges) != 0 || len(nudgeComments(env.api)) != 0 {
				t.Fatalf("unreadable digest: author nudged: sink=%+v comments=%v", env.sink.nudges, nudgeComments(env.api))
			}
			env.api.mu.Lock()
			added, removed := env.api.added, env.api.removed
			env.api.mu.Unlock()
			for _, name := range removed {
				if strings.HasPrefix(name, stagelabels.Prefix) {
					t.Fatalf("unreadable digest: stage label %q removed (removed %v)", name, removed)
				}
			}
			for _, set := range added {
				for _, name := range set {
					if strings.HasPrefix(name, stagelabels.Prefix) && name != stagelabels.NeedsDecision.Label() {
						t.Fatalf("unreadable digest: stage label %q added (added %v)", name, added)
					}
				}
			}

			// Readable again: still the guard.
			env.set(func() { env.digestDown = false })
			env.clock.advance(humanChecksTTL + time.Minute)
			if rec := env.pass(1263, "9c0d23d", tc.reason, tc.labels...); rec.Stage != stagelabels.NeedsDecision || rec.Reason != want {
				t.Fatalf("digest readable again: record %s %q, want 085 %q", rec.Stage, rec.Reason, want)
			}
		})
	}
}

// A gate-stopped fork head red on the guard AND a real failure is still the
// author's: 072 on the red checks, nudged as before.
func TestForkGuardGateLaneWithRealFailureReturnsToAuthor(t *testing.T) {
	env := newForkGuardGateEnv(t, time.Date(2026, 11, 21, 10, 35, 0, 0, time.UTC))
	env.set(func() {
		env.runs["mixed"] = forkGuardGateRuns(forkGuardGateRun(21, "Canary", "failure"), forkGuardGateRun(22, "Unit tests", "failure"))
		env.annotations["21"] = forkGuardGateCanaryAnnotations
		env.annotations["22"] = forkGuardGateUnitAnnotations
		env.red["mixed"] = []string{"Canary", "Unit tests"}
	})
	rec := env.pass(1264, "mixed", contributorLaneReasonSentinelBlocked, defaultSentinelLabel)
	if rec.Stage != stagelabels.ReturnedToContributor || len(env.sink.nudges) != 1 {
		t.Fatalf("guard beside a real failure: stage %s %q nudges %d, want 072 and one nudge", rec.Stage, rec.Reason, len(env.sink.nudges))
	}
	if strings.HasPrefix(rec.Reason, stageReasonForkGuard) {
		t.Fatalf("reason %q, want the red checks, not the fork guard", rec.Reason)
	}
}

// A fork head first read while a real failure was red beside the guard is
// the author's; once that check is re-run green and only the guard is red,
// the head is classified again from the new red set. The "not a fork guard"
// answer for {Canary, Unit tests} used to cover the head whatever was red
// later (knownNotForkGuard), so the PR stayed at 072.
func TestForkGuardHeadReadAgainWhenRedSetShrinksToGuard(t *testing.T) {
	env := newForkGuardGateEnv(t, time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC))
	env.set(func() {
		env.runs["twored"] = forkGuardGateRuns(forkGuardGateRun(31, "Canary", "failure"), forkGuardGateRun(32, "Unit tests", "failure"))
		env.annotations["31"] = forkGuardGateCanaryAnnotations
		env.annotations["32"] = forkGuardGateUnitAnnotations
		env.red["twored"] = []string{"Canary", "Unit tests"}
	})

	rec := env.pass(1300, "twored", "required-check-failing:Unit tests")
	if rec.Stage != stagelabels.ReturnedToContributor || len(env.sink.nudges) != 1 {
		t.Fatalf("real failure beside the guard: stage %s nudges %d, want 072 and one nudge", rec.Stage, len(env.sink.nudges))
	}

	// Unit tests re-run green on the same head; the digest is re-read
	// once its cache entry expires.
	env.set(func() {
		env.runs["twored"] = forkGuardGateRuns(forkGuardGateRun(31, "Canary", "failure"), forkGuardGateRun(32, "Unit tests", "failure"), forkGuardGateRun(33, "Unit tests", "success"))
		env.red["twored"] = []string{"Canary"}
	})
	env.clock.advance(humanChecksTTL + time.Minute)
	rec = env.pass(1300, "twored", "required-check-failing:Canary")
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stageReasonForkGuard+"Canary" {
		t.Fatalf("only the guard red: record %s %q, want 085 %q", rec.Stage, rec.Reason, stageReasonForkGuard+"Canary")
	}
	if len(env.sink.nudges) != 1 {
		t.Fatalf("nudges %d, want only the one for the real failure", len(env.sink.nudges))
	}
}

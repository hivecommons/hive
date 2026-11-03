package github

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#12683: a parked issue carrying a retired park label gets
// the issue parking stage (045; 085 is PR-only) written first, then the
// retired label removed, and the definitions are deleted once nothing open
// carries them. An issue still wearing 085 drops it in the same write.
func TestSyncIssueStageLabels_MigratesLegacyDecisionLabels(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)

	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 1, Labels: []string{"help wanted", "needs-direction"}}, "filtered", "needs-direction")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 2, Labels: []string{"needs-decision", stagelabels.NeedsDecision.Label()}}, "filtered", "needs-decision")
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 3, Labels: []string{"needs-human"}}, "filtered", "needs-human")

	res := syncSeeded(t, fake, c, result, store, stageNow())
	if res.LegacyDecisionMigrated != 2 {
		t.Fatalf("migrated = %d, want 2 (%+v)", res.LegacyDecisionMigrated, res)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := fake.added[1]; strings.Join(got, ",") != stagelabels.IssueNeedsDecision.Label() {
		t.Errorf("issue 1 added %v, want stage/045 only", got)
	}
	if got := fake.removed[1]; strings.Join(got, ",") != "needs-direction" {
		t.Errorf("issue 1 removed %v, want needs-direction", got)
	}
	if got := fake.added[2]; strings.Join(got, ",") != stagelabels.IssueNeedsDecision.Label() {
		t.Errorf("issue 2 added %v, want stage/045 (re-staged off the PR-only 085)", got)
	}
	if got := fake.removed[2]; strings.Join(got, ",") != stagelabels.NeedsDecision.Label()+",needs-decision" {
		t.Errorf("issue 2 removed %v, want stage/085 then needs-decision", got)
	}
	if res.NeedsDecisionRestaged != 1 {
		t.Errorf("NeedsDecisionRestaged = %d, want 1 (issue 2)", res.NeedsDecisionRestaged)
	}
	if len(fake.removed[3]) != 0 {
		t.Errorf("needs-human must stay: removed %v", fake.removed[3])
	}
	if got := fake.added[3]; strings.Join(got, ",") != stagelabels.IssueNeedsDecision.Label() {
		t.Errorf("issue 3 added %v, want stage/045 (needs-human implies the park)", got)
	}
}

func TestSyncIssueStageLabels_RetiresLegacyDefinitionsOnlyWhenUncarried(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)

	// First cycle: issue 1 migrates, but an item the issue cycle never
	// enumerates (a PR, say) still carries needs-decision, so the confirming
	// list-by-label call blocks deletion of both definitions.
	fake.mu.Lock()
	fake.truth[77] = []string{"needs-decision"}
	fake.mu.Unlock()
	result := &ActionableResult{}
	appendRepoWorkIssueDetail(result, Issue{Repo: "acme/widgets", Number: 1, Labels: []string{"needs-direction"}}, "filtered", "needs-direction")
	res := syncSeeded(t, fake, c, result, store, stageNow())
	if res.LegacyDecisionDefinitionsDeleted != 0 {
		t.Fatalf("definitions deleted on a cycle that saw a carrier: %+v", res)
	}
	fake.mu.Lock()
	if len(fake.deletedDefs) != 0 {
		t.Fatalf("deleted %v while a carrier was open", fake.deletedDefs)
	}
	fake.mu.Unlock()

	// Second cycle: nothing carries the retired labels → both definitions go.
	fake.mu.Lock()
	delete(fake.truth, 77)
	fake.mu.Unlock()
	clean := &ActionableResult{}
	appendRepoWorkIssueDetail(clean, Issue{Repo: "acme/widgets", Number: 1, Labels: []string{stagelabels.NeedsDecision.Label()}}, "filtered", NeedsDecisionStageLabel)
	res = syncSeeded(t, fake, c, clean, store, stageNow())
	if res.LegacyDecisionDefinitionsDeleted != 2 {
		t.Fatalf("definitions deleted = %d, want 2 (%+v)", res.LegacyDecisionDefinitionsDeleted, res)
	}
	fake.mu.Lock()
	got := strings.Join(fake.deletedDefs, ",")
	fake.mu.Unlock()
	if got != "needs-decision,needs-direction" {
		t.Fatalf("deleted definitions = %q", got)
	}

	// Third cycle: once per process.
	res = syncSeeded(t, fake, c, clean, store, stageNow())
	if res.LegacyDecisionDefinitionsDeleted != 0 {
		t.Fatalf("definition retirement re-ran: %+v", res)
	}
}

// hivecommons/hive#12983: definition retirement is housekeeping, so while the
// shared rate-limit caution window is engaged it makes no request (and does
// not mark the repo done); once the window lapses it runs as before.
func TestRetireLegacyDecisionLabelDefinitions_DefersDuringCautionWindow(t *testing.T) {
	t.Cleanup(ResetRateLimitPacingForTest)
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)

	sharedSlowStartState.mu.Lock()
	sharedSlowStartState.cautiousUntil = time.Now().Add(time.Hour)
	sharedSlowStartState.mu.Unlock()
	if got := c.retireLegacyDecisionLabelDefinitions(context.Background(), "acme/widgets", testLogger()); got != 0 {
		t.Fatalf("deleted %d definitions during the caution window, want 0", got)
	}
	fake.mu.Lock()
	deferred := len(fake.deletedDefs)
	fake.mu.Unlock()
	if deferred != 0 {
		t.Fatalf("deleted %d definitions during the caution window", deferred)
	}

	ResetRateLimitPacingForTest()
	if got := c.retireLegacyDecisionLabelDefinitions(context.Background(), "acme/widgets", testLogger()); got != 2 {
		t.Fatalf("deleted %d definitions after the window lapsed, want 2", got)
	}
}

func TestSlowStartState_Cautious(t *testing.T) {
	st := newSlowStartState()
	now := time.Now()
	if st.cautious(now) {
		t.Fatal("fresh state reports cautious")
	}
	st.cautiousUntil = now.Add(time.Minute)
	if !st.cautious(now) {
		t.Fatal("engaged window not reported")
	}
	if st.cautious(now.Add(2 * time.Minute)) {
		t.Fatal("lapsed window still reported")
	}
}

// hivecommons/hive#14171: a deadline is a deferral, not a failure. With the
// caller's deadline already spent the pass makes no request; with one that
// expires mid-list it logs at Debug, not Warn (the Warn self-reported 42
// times in ten minutes), and in both cases leaves the repo unmarked so the
// next cycle retries.
func TestRetireLegacyDecisionLabelDefinitions_DeadlineIsQuietDeferral(t *testing.T) {
	t.Cleanup(ResetRateLimitPacingForTest)
	ResetRateLimitPacingForTest()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)

	spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancel()
	if got := c.retireLegacyDecisionLabelDefinitions(spent, "acme/widgets", logger); got != 0 {
		t.Fatalf("deleted %d with a spent deadline, want 0", got)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("made %d requests with a spent deadline, want 0", n)
	}

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if got := c.retireLegacyDecisionLabelDefinitions(short, "acme/widgets", logger); got != 0 {
		t.Fatalf("deleted %d after a deadline, want 0", got)
	}
	out := buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("deadline logged at Warn:\n%s", out)
	}
	if !strings.Contains(out, "retirement deferred by deadline") {
		t.Fatalf("deadline deferral not logged at Debug:\n%s", out)
	}
	if c.legacyDecisionLabels.has("acme/widgets") {
		t.Fatal("repo marked done after a deadline; the next cycle would never retry")
	}
}

// Permission denial is permanent for this client, but rate limits and server
// failures must remain retryable. A denied repo must not disable other repos.
func TestRetireLegacyDecisionLabelDefinitions_DeleteFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		message  string
		disabled bool
	}{
		{"integration permission", http.StatusForbidden, "Resource not accessible by integration", true},
		{"rate limit", http.StatusForbidden, "API rate limit exceeded", false},
		{"server failure", http.StatusInternalServerError, "Internal Server Error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(ResetRateLimitPacingForTest)
			ResetRateLimitPacingForTest()
			var deletes int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodDelete {
					deletes++
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"message":"` + tc.message + `"}`))
					return
				}
				_, _ = w.Write([]byte("[]"))
			}))
			t.Cleanup(srv.Close)
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, logger)
			for i := 0; i < 2; i++ {
				ResetRateLimitPacingForTest()
				if got := c.retireLegacyDecisionLabelDefinitions(context.Background(), "acme/widgets", logger); got != 0 {
					t.Fatalf("deleted = %d, want 0", got)
				}
			}
			want := 2
			if tc.disabled {
				want = 1
			}
			if deletes != want {
				t.Fatalf("delete attempts = %d, want %d", deletes, want)
			}
			if got := c.legacyDecisionLabels.has("acme/widgets"); got != tc.disabled {
				t.Fatalf("repo disabled = %v, want %v", got, tc.disabled)
			}
			if tc.disabled {
				if strings.Contains(buf.String(), "level=WARN") {
					t.Fatalf("permission denial logged as warning: %s", buf.String())
				}
				c.retireLegacyDecisionLabelDefinitions(context.Background(), "acme/other", logger)
				if deletes != 2 {
					t.Fatalf("other repo did not attempt cleanup: deletes = %d", deletes)
				}
			}
		})
	}
}

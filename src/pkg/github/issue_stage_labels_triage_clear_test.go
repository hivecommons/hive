package github

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14586: a hive-filed self-report wears `needs-triage` from
// filing; the scanner claims it from 015 (not a gate) and the label survived
// the 015 → 020 write, so a claimed — then closed — issue still read "Triage
// needed". The sweep now takes `needs-triage` off in the same write as the
// stage, and reconciles an issue already at 020 that still wears it.
func TestSyncIssueStageLabels_ClaimClearsNeedsTriage(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	result := &ActionableResult{}
	// 14586: direct claim from 015.
	appendRepoWorkIssueDetail(result, Issue{
		Repo: "acme/widgets", Number: 14586, Author: "hive-app[bot]",
		Labels:    []string{"kind/bug", triageAwaitingLabel, stagelabels.TriageNeeded.Label(), "hive/claimed-by-scanner"},
		ClaimKind: string(claims.KindAgent),
	}, workBucketClaimed, "live claim")
	// 14587: already 020 and correct, but still wearing the stale label.
	appendRepoWorkIssueDetail(result, Issue{
		Repo: "acme/widgets", Number: 14587, Author: "hive-app[bot]",
		Labels:    []string{triageAwaitingLabel, stagelabels.Claimed.Label()},
		ClaimKind: string(claims.KindAgent),
	}, workBucketClaimed, "live claim")
	// 14588: untriaged and unclaimed stays 015 and keeps its label.
	result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 14588, Author: "alice", AuthorIsHuman: true, Labels: []string{triageAwaitingLabel}}})

	res := syncSeeded(t, fake, c, result, stagelabels.NewStore("", stageNow), stageNow())
	if res.Items != 3 {
		t.Fatalf("items = %d, want 3", res.Items)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.added[14586], ","); got != stagelabels.Claimed.Label() {
		t.Fatalf("direct claim must land 020: added=%v", fake.added[14586])
	}
	if !slices.Contains(fake.removed[14586], triageAwaitingLabel) || !slices.Contains(fake.removed[14586], stagelabels.TriageNeeded.Label()) {
		t.Fatalf("015 and needs-triage must come off in the claim write: removed=%v", fake.removed[14586])
	}
	if slices.Contains(fake.removed[14586], "kind/bug") || slices.Contains(fake.removed[14586], "hive/claimed-by-scanner") {
		t.Fatalf("unrelated labels must not be touched: removed=%v", fake.removed[14586])
	}
	if len(fake.added[14587]) != 0 || !slices.Equal(fake.removed[14587], []string{triageAwaitingLabel}) {
		t.Fatalf("already-claimed issue must shed only the stale label: added=%v removed=%v", fake.added[14587], fake.removed[14587])
	}
	if got := strings.Join(fake.added[14588], ","); got != stagelabels.TriageNeeded.Label() || len(fake.removed[14588]) != 0 {
		t.Fatalf("untriaged issue keeps needs-triage: added=%v removed=%v", fake.added[14588], fake.removed[14588])
	}
}

// hivecommons/hive#18387: on a repository whose own triage tooling (Prow's
// require-matching-label action) owns `needs-triage`, the tool re-adds the
// label seconds after the hive takes it off a claimed or covered issue, and
// the sweep used to remove it again on every pass (312 removals on 16
// issues in one night). The hive now removes it once; when it is back on a
// later read, the hive yields it on that issue for good, logs that once,
// and remembers the yield across a restart.
func TestSyncIssueStageLabels_NeedsTriageReAddedIsLeftOn(t *testing.T) {
	const claimed, covered = 18387, 18388
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	prStarted := false
	pass := func(store *stagelabels.Store) {
		t.Helper()
		result := &ActionableResult{}
		// Every pass reads the label on again: the other tool put it back.
		// The claimed issue keeps wearing 020, so a PR start (prStarted)
		// derives 030 and makes the engine write.
		appendRepoWorkIssueDetail(result, Issue{
			Repo: "acme/widgets", Number: claimed, Author: "hive-app[bot]",
			Labels:    []string{triageAwaitingLabel, stagelabels.Claimed.Label()},
			ClaimKind: string(claims.KindAgent), ClaimPRStarted: prStarted,
		}, workBucketClaimed, "live claim")
		appendRepoWorkIssueDetail(result, Issue{
			Repo: "acme/widgets", Number: covered,
			Labels:       []string{triageAwaitingLabel, stagelabels.CoveredByPR.Label()},
			ClaimContext: &IssueClaimContext{PRNumber: 40},
		}, workBucketClaimedByPR, "open PR")
		fake.seed(result)
		ResetRateLimitPacingForTest()
		resetRESTAccountingForTest(t, stageNow())
		SyncIssueStageLabels(context.Background(), c, result, store, stageNow(), logger)
	}
	triageRemovals := func(n int) int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		count := 0
		for _, l := range fake.removed[n] {
			if strings.EqualFold(l, triageAwaitingLabel) {
				count++
			}
		}
		return count
	}
	yieldLogs := func() int { return strings.Count(logBuf.String(), "came back after the hive removed it") }
	expect := func(when string, store *stagelabels.Store, yielded bool, logs int) {
		t.Helper()
		for _, n := range []int{claimed, covered} {
			if got := triageRemovals(n); got != 1 {
				t.Fatalf("%s: #%d had %d %s removals, want exactly 1: removed=%v", when, n, got, triageAwaitingLabel, fake.removed[n])
			}
			rec, ok := store.Get(stagelabels.Key("acme/widgets", n))
			if !ok || rec.TriageClearedAt.IsZero() || rec.TriageClearYielded != yielded {
				t.Fatalf("%s: #%d record %+v: want the removal booked and yielded=%v", when, n, rec, yielded)
			}
		}
		if got := yieldLogs(); got != logs {
			t.Fatalf("%s: %d yield log lines, want %d:\n%s", when, got, logs, logBuf.String())
		}
	}

	path := filepath.Join(t.TempDir(), "stages.json")
	store := stagelabels.NewStore(path, stageNow)
	pass(store)
	expect("pass 1 (first removal)", store, false, 0)
	pass(store)
	expect("pass 2 (label re-added: yield)", store, true, 2)
	if !strings.Contains(logBuf.String(), "issue=18387") || !strings.Contains(logBuf.String(), "label="+triageAwaitingLabel) {
		t.Fatalf("yield log must name the repo, issue and label:\n%s", logBuf.String())
	}
	pass(store)
	expect("pass 3 (still yielded, no new log)", store, true, 2)

	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	restarted := stagelabels.NewStore(path, stageNow)
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	pass(restarted)
	expect("after restart", restarted, true, 2)

	// A stage write on a yielded issue (020 → 030) goes through the engine:
	// the deriver must not ask for the label to come off with it.
	prStarted = true
	pass(restarted)
	expect("stage write on a yielded issue", restarted, true, 2)
	fake.mu.Lock()
	added := slices.Clone(fake.added[claimed])
	fake.mu.Unlock()
	if !slices.Contains(added, stagelabels.PRInProgress.Label()) {
		t.Fatalf("020 → 030 must still be written: added=%v", added)
	}
}

// hivecommons/hive#18388 review: a released claim drops the issue back to
// 015, where `needs-triage` is legitimately on again. That read must not
// book the sticky yield; only a stage that settles triage judges it.
func TestSyncIssueStageLabels_NeedsTriageBackAtUnsettledStageDoesNotYield(t *testing.T) {
	const n = 18389
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	key := stagelabels.Key("acme/widgets", n)

	claimedResult := &ActionableResult{}
	appendRepoWorkIssueDetail(claimedResult, Issue{
		Repo: "acme/widgets", Number: n, Author: "alice", AuthorIsHuman: true,
		Labels:    []string{triageAwaitingLabel, stagelabels.Claimed.Label()},
		ClaimKind: string(claims.KindAgent),
	}, workBucketClaimed, "live claim")
	syncSeeded(t, fake, c, claimedResult, store, stageNow())
	if rec, _ := store.Get(key); rec.TriageClearedAt.IsZero() {
		t.Fatalf("claimed pass must book the removal: %+v", rec)
	}

	released := &ActionableResult{}
	released.Issues = IssueResultFromItems([]Issue{{
		Repo: "acme/widgets", Number: n, Author: "alice", AuthorIsHuman: true,
		Labels: []string{triageAwaitingLabel, stagelabels.Claimed.Label()},
	}})
	syncSeeded(t, fake, c, released, store, stageNow())
	rec, _ := store.Get(key)
	if rec.Stage.SettlesTriage() {
		t.Fatalf("released claim must leave a stage that does not settle triage, got %s", rec.Stage.Label())
	}
	if rec.TriageClearYielded {
		t.Fatalf("needs-triage back at %s must not book the yield: %+v", rec.Stage.Label(), rec)
	}
}

// hivecommons/hive#18388 review: the FactSink path. A claim fact takes
// `needs-triage` off and books the removal through Reconcile; when the
// repository's tooling puts it back, the next sink fact reads it back
// (enrichTriageClearYield), yields, and leaves it on.
func TestStageFacts_NeedsTriageReAddedIsLeftOnBySink(t *testing.T) {
	const n = 18390
	srv := newStageFactsServer(t, triageAwaitingLabel, stagelabels.Actionable.Label())
	c := testClient(t, srv.srv.URL)
	ref := c.IssueStageRef("o/r", n)
	key := ref.Key()
	store := stagelabels.Shared()
	t.Cleanup(func() { store.ForgetStage(key) })

	if err := c.StageFacts().Claimed(context.Background(), ref, stageengine.ClaimFact{Holder: "hive-a"}); err != nil {
		t.Fatal(err)
	}
	labels, _, removed := srv.snapshot()
	if issueHasLabel(labels, triageAwaitingLabel) || !slices.Contains(removed, triageAwaitingLabel) {
		t.Fatalf("claim fact must take %s off: labels=%v removed=%v", triageAwaitingLabel, labels, removed)
	}
	if rec, _ := store.Get(key); rec.TriageClearedAt.IsZero() || rec.TriageClearYielded {
		t.Fatalf("sink write must book the removal without a yield: %+v", rec)
	}

	srv.mu.Lock()
	srv.labels = append(srv.labels, triageAwaitingLabel)
	srv.mu.Unlock()
	if err := c.StageFacts().InProgress(context.Background(), ref, "hive-a", stageNow().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	labels, _, removed = srv.snapshot()
	if !issueHasLabel(labels, triageAwaitingLabel) {
		t.Fatalf("re-added %s must stay on: labels=%v", triageAwaitingLabel, labels)
	}
	if got := strings.Count(strings.Join(removed, ","), triageAwaitingLabel); got != 1 {
		t.Fatalf("%s removed %d times, want 1: removed=%v", triageAwaitingLabel, got, removed)
	}
	if rec, _ := store.Get(key); !rec.TriageClearYielded {
		t.Fatalf("sink must book the yield when the label is back: %+v", rec)
	}
}

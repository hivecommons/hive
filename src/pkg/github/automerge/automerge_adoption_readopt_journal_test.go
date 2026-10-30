package automerge

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

// Re-adoption after a closed copy under a journaling mutation boundary: the
// second adoption must not be answered by the journal entry of the first.

// journalBoundary mimics the mutation boundary's journal for adoption
// claims: a branch_push or pull_request_create claim whose logical effect
// (repo, kind, target, inputs) already applied is answered with the recorded
// provenance and its effect does not run.
type journalBoundary struct {
	mu      sync.Mutex
	applied map[string]string
	claims  []effects.Claim
	replays []effects.Claim
	// seed answers a claim the journal has not seen with recorded
	// provenance, as if an earlier process had applied it.
	seed func(effects.Claim) (string, bool)
}

func journalKey(claim effects.Claim) string {
	keys := make([]string, 0, len(claim.Inputs))
	for k := range claim.Inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{claim.Repo, claim.Kind, claim.Target}
	for _, k := range keys {
		parts = append(parts, k+"="+claim.Inputs[k])
	}
	return strings.Join(parts, "|")
}

func (b *journalBoundary) Execute(ctx context.Context, claim effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	if claim.Kind != effects.KindBranchPush && claim.Kind != effects.KindPullRequestCreate {
		return effect(ctx)
	}
	key := journalKey(claim)
	b.mu.Lock()
	b.claims = append(b.claims, claim)
	prov, ok := b.applied[key]
	if !ok && b.seed != nil {
		prov, ok = b.seed(claim)
	}
	if ok {
		b.replays = append(b.replays, claim)
		b.mu.Unlock()
		return effects.Result{Provenance: prov}, nil
	}
	b.mu.Unlock()
	res, err := effect(ctx)
	if err == nil {
		b.mu.Lock()
		b.applied[key] = res.Provenance
		b.mu.Unlock()
	}
	return res, err
}

func (b *journalBoundary) replayed(kind string) []effects.Claim {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []effects.Claim
	for _, c := range b.replays {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (b *journalBoundary) lastClaim(kind string) effects.Claim {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.claims) - 1; i >= 0; i-- {
		if b.claims[i].Kind == kind {
			return b.claims[i]
		}
	}
	return effects.Claim{}
}

// adoptAt074Journaled is adoptAt074 with the journal wired in before the
// first adoption, so the claim that opened copy #90 is on record.
func adoptAt074Journaled(t *testing.T) (*adoptHarness, *journalBoundary, func(), []string) {
	t.Helper()
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	jb := &journalBoundary{applied: map[string]string{}}
	h.e.mutation = jb
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 || len(jb.replays) != 0 {
		t.Fatalf("setup: pulls=%d replays=%v", len(h.adopt.pulls), jb.replays)
	}
	return h, jb, done, []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}
}

// The copy is closed unmerged; the re-adoption claims a distinct effect
// (prior_copy=90), so the journal does not replay the first claim and a
// fresh copy opens instead of the adoption being reverted.
func TestAdoptionReadoptAfterClosedCopyIsNotJournalReplayed(t *testing.T) {
	h, jb, done, adopted := adoptAt074Journaled(t)
	defer done()
	h.adopt.setSuperseder(90, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	if r := jb.replayed(effects.KindPullRequestCreate); len(r) != 0 {
		t.Fatalf("re-adoption replayed the first adoption's claim: %v", r)
	}
	if r := jb.replayed(effects.KindBranchPush); len(r) != 0 {
		t.Fatalf("re-adoption replayed the first adoption's push: %v", r)
	}
	for _, kind := range []string{effects.KindBranchPush, effects.KindPullRequestCreate} {
		if got := jb.lastClaim(kind).Inputs["prior_copy"]; got != "90" {
			t.Fatalf("%s claim prior_copy = %q, want 90", kind, got)
		}
	}
	if len(h.adopt.pulls) != 2 {
		t.Fatalf("no fresh copy opened: pulls=%d", len(h.adopt.pulls))
	}
	if n := commentsWithPrefix(h.api, AdoptionRevertedMarker); len(n) != 0 {
		t.Fatalf("adoption reverted: %v", n)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 91 || rec.AdoptedHead != "aaa" || rec.AdoptOrphanedBy != 90 {
		t.Fatalf("record after re-adoption = %+v", rec)
	}
}

// The journal replays the PR-create claim and hands back a copy that is
// closed: the adoption claims a fresh copy rather than reading the replay
// as a failure and clearing the adoption.
func TestAdoptionReadoptReplaysBaseMergeKeyedByPriorCopy(t *testing.T) {
	h, jb, done, adopted := adoptAt074Journaled(t)
	defer done()
	h.adopt.setSuperseder(90, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", append(adopted, "needs-rebase")...), "check-failing:unit")
	var replay effects.Claim
	for _, c := range jb.claims {
		if c.Kind == effects.KindBranchPush && c.Inputs["reason"] == "supersede-replay" {
			replay = c
		}
	}
	if got := replay.Inputs["prior_copy"]; got != "90" {
		t.Fatalf("supersede-replay claim prior_copy = %q, want 90 (claims %v)", got, jb.claims)
	}
	if len(h.adopt.merges) != 1 || h.adopt.merges[0] != "hive/adopt-7<-v5" {
		t.Fatalf("base not merged on re-adoption: %v", h.adopt.merges)
	}
}

func TestAdoptionJournalReplayOfClosedCopyOpensFreshCopy(t *testing.T) {
	h, jb, done, adopted := adoptAt074Journaled(t)
	defer done()
	h.adopt.setSuperseder(55, "closed", false)
	jb.seed = func(c effects.Claim) (string, bool) {
		if c.Kind == effects.KindPullRequestCreate && c.Inputs["prior_copy"] == "90" {
			return "https://github.com/acme/widget/pull/55", true
		}
		return "", false
	}
	h.adopt.setSuperseder(90, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	if len(h.adopt.pulls) != 2 {
		t.Fatalf("closed replay did not lead to a fresh copy: pulls=%d", len(h.adopt.pulls))
	}
	if got := jb.lastClaim(effects.KindPullRequestCreate).Inputs["prior_copy"]; got != "55" {
		t.Fatalf("fresh claim prior_copy = %q, want 55", got)
	}
	if n := commentsWithPrefix(h.api, AdoptionRevertedMarker); len(n) != 0 {
		t.Fatalf("adoption cleared on a closed replay: %v", n)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 91 || rec.AdoptedHead != "aaa" {
		t.Fatalf("record after closed replay = %+v", rec)
	}
}

// The journal replays the PR-create claim and hands back a copy that is
// still open: that copy is the adoption, and no second one is opened.
func TestAdoptionJournalReplayOfOpenCopyAdoptsIt(t *testing.T) {
	h, jb, done, adopted := adoptAt074Journaled(t)
	defer done()
	h.adopt.setSuperseder(55, "open", false)
	jb.seed = func(c effects.Claim) (string, bool) {
		if c.Kind == effects.KindPullRequestCreate && c.Inputs["prior_copy"] == "90" {
			return "https://github.com/acme/widget/pull/55", true
		}
		return "", false
	}
	h.adopt.setSuperseder(90, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("a copy was opened beside the replayed open one: pulls=%d", len(h.adopt.pulls))
	}
	markers := adoptionComments(h.api)
	if len(markers) != 1 || !strings.Contains(markers[0], "superseded_by=55") {
		t.Fatalf("adoption marker: %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 55 || rec.AdoptedHead != "aaa" {
		t.Fatalf("record after open replay = %+v", rec)
	}
}

func TestAdoptionReplayedPRNumber(t *testing.T) {
	for in, want := range map[string]int{
		"https://github.com/acme/widget/pull/55":  55,
		"https://github.com/acme/widget/pull/55/": 55,
		"":                                      0,
		"abc123":                                0,
		"https://github.com/acme/widget/pull/x": 0,
	} {
		if got := adoptionReplayedPRNumber(in); got != want {
			t.Fatalf("adoptionReplayedPRNumber(%q) = %d, want %d", in, got, want)
		}
	}
}

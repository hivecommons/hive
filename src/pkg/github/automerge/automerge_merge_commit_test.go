package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Merge commit for mixed authorship (automerge_merge_commit.go): the hive
// adopted hivecommons/hive#13777 in place, pushed a self-signed fix commit,
// and then parked the PR at 085 asking a maintainer to merge with a merge
// commit. A role/trusted PR now gets that merge commit from the lane.

var adoptedTrustedLabels = []string{"stage/074-adopted-by-hive", "role/trusted", "author/clanker"}

// mixedAuthorshipCommits is #13777's head: the contributor's signed commit
// and the hive's fix commit signed as the App.
var mixedAuthorshipCommits = []stageCommitFixture{
	{message: "✨ feature: add offline repository code-search index\n\nSigned-off-by: Mallory <mallory@example.com>", authorLogin: "mallory", authorName: "Mallory", authorEmail: "mallory@example.com"},
	{message: "test(codesearch): cover file error, budget, cancellation\n\nSigned-off-by: hive[bot] <12345+hive[bot]@users.noreply.github.com>", authorLogin: testHiveAppBotLogin, authorName: "hive[bot]", authorEmail: "12345+hive[bot]@users.noreply.github.com"},
}

// observeTrusted is adoptHarness.observe with the trusted-author lane's
// role on the PR (RoleTrusted wins the merge).
func (h *adoptHarness) observeTrusted(pr *gh.PullRequest, reason string) StageLabelResult {
	h.t.Helper()
	h.adopt.mu.Lock()
	h.adopt.heads[pr.GetNumber()] = pr.GetHead().GetSHA()
	h.adopt.mu.Unlock()
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
	h.e.noteStageRole("acme", "widget", pr.GetNumber(), stagelabels.RoleTrusted)
	res, err := h.e.ApplyStageLabels(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func mergeCommitComments(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, MergeCommitMarkerPrefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestMergeCommitElection(t *testing.T) {
	squashOnly := StageLabelPolicy{SquashOnly: func(string) bool { return true }}
	tests := []struct {
		name   string
		stats  dcoStats
		role   stagelabels.Role
		policy StageLabelPolicy
		want   mergeCommitDecision
	}{
		{"pure contributor head: nothing to elect", dcoStats{total: 2}, stagelabels.RoleTrusted, StageLabelPolicy{}, mergeCommitDecision{}},
		{"mixed, self-signed, trusted", dcoStats{total: 2, squashUnsafeHive: 1}, stagelabels.RoleTrusted, StageLabelPolicy{}, mergeCommitDecision{Elected: true}},
		{"mixed but role/contributor", dcoStats{total: 2, squashUnsafeHive: 1}, stagelabels.RoleContributor, StageLabelPolicy{}, mergeCommitDecision{Refused: mergeCommitRefusedRole}},
		{"mixed with an unsigned commit", dcoStats{total: 3, unsigned: 1, squashUnsafeHive: 1}, stagelabels.RoleTrusted, StageLabelPolicy{}, mergeCommitDecision{Refused: mergeCommitRefusedUnsigned}},
		{"only hive commits", dcoStats{total: 1, squashUnsafeHive: 1}, stagelabels.RoleTrusted, StageLabelPolicy{}, mergeCommitDecision{Refused: mergeCommitRefusedHiveOnly}},
		{"squash-only repo", dcoStats{total: 2, squashUnsafeHive: 1}, stagelabels.RoleTrusted, squashOnly, mergeCommitDecision{Refused: mergeCommitRefusedSquashOnly}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeCommitElection(tt.stats, tt.role, tt.policy, "acme/widget"); got != tt.want {
				t.Fatalf("mergeCommitElection = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// #13777 as it should have gone: adopted in place, the hive's self-signed
// fix beside the contributor's commit, role/trusted. The lane elects a
// merge commit: `merge-method/merge` goes on, one note explains it, the PR
// stays 074 and no stall note or 085 follows.
func TestAdoptedInPlaceMixedAuthorshipElectsMergeCommit(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.api.commitDetails = map[int][]stageCommitFixture{7: mixedAuthorshipCommits}
	key := adoptInPlaceAt(h, "a1899cc", contributorLaneReasonNoVerdict)
	h.adopt.setCommitAuthor("a1899cc", testHiveAppBotLogin)

	h.clock.advance(time.Minute)
	h.observeTrusted(adoptPR(7, "a1899cc", adoptedTrustedLabels...), contributorLaneReasonNoVerdict)
	if n := stalledComments(h.api); len(n) != 0 {
		t.Fatalf("stalled on its own commit: %v", n)
	}
	if len(h.adopt.pulls) != 0 {
		t.Fatalf("supersede opened: %v", h.adopt.pulls)
	}
	if added := h.addedLabels(); !containsLabel(added, stagelabels.MergeCommitLabel) || containsLabel(added, stagelabels.NeedsDecision.Label()) {
		t.Fatalf("labels: added=%v removed=%v", h.api.added, h.api.removed)
	}
	notes := mergeCommitComments(h.api)
	if len(notes) != 1 {
		t.Fatalf("merge-commit note: %v", h.api.comments)
	}
	for _, want := range []string{"head=a1899cc", "1 of 2 commits", "@mallory", "**merge commit**", stagelabels.MergeCommitLabel, "--merge"} {
		if !strings.Contains(notes[0], want) {
			t.Fatalf("note missing %q:\n%s", want, notes[0])
		}
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if !rec.AdoptedInPlace() || rec.Stage != stagelabels.AdoptedByHive || rec.AdoptionEscalated("a1899cc") || !rec.MergeCommitNoted("a1899cc") {
		t.Fatalf("record: %+v", rec)
	}
	if rec.Reason == stagelabels.LaneReasonHiveSignoffAttribution {
		t.Fatalf("attribution override applied: %+v", rec)
	}

	// Next sweep, label on: nothing new, and the dwell does not run out
	// against a head that CI is re-checking under the label.
	h.api.reset()
	h.clock.advance(adoptedDwell + time.Minute)
	labelled := append(append([]string{}, adoptedTrustedLabels...), stagelabels.MergeCommitLabel, stagelabels.DCOYes.Label())
	h.observeTrusted(adoptPR(7, "a1899cc", labelled...), "check-pending")
	if h.api.writes() != 0 {
		t.Fatalf("second sweep wrote: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
}

// hivecommons/hive#15710: adopted fork PR #15521, the same mixed head on a
// role/trusted PR, but the write-back gate keeps the hive's labels off the
// contributor PR. The note used to go out before the write, promising a
// `merge-method/merge` that never landed while the attribution check
// stayed red. Now no note follows a suppressed write, and the next pass
// refuses the election and takes the lane's fallback (re-home).
func TestAdoptedInPlaceMixedAuthorshipLabelsSuppressedRefusesMergeCommit(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	client, ok := h.e.transport.(*hgithub.Client)
	if !ok {
		t.Fatalf("transport %T is not *hgithub.Client", h.e.transport)
	}
	inner := client.StageWriter()
	suppress := false
	h.e.stageEngine = &stageengine.Options{Writer: prStageWriterFunc(func(ctx context.Context, ref stageengine.Ref, hint []string, want stagelabels.Desired) (stagelabels.Outcome, error) {
		if suppress {
			return stagelabels.Outcome{Stage: want.Stage, Suppressed: true}, nil
		}
		return inner.Apply(ctx, ref, hint, want)
	})}
	h.api.commitDetails = map[int][]stageCommitFixture{7: mixedAuthorshipCommits}
	key := adoptInPlaceAt(h, "e6d0ccb", contributorLaneReasonNoVerdict)
	h.adopt.setCommitAuthor("e6d0ccb", testHiveAppBotLogin)
	suppress = true

	h.clock.advance(time.Minute)
	h.observeTrusted(adoptPR(7, "e6d0ccb", adoptedTrustedLabels...), contributorLaneReasonNoVerdict)
	if notes := mergeCommitComments(h.api); len(notes) != 0 {
		t.Fatalf("merge-commit note promised a label the gate kept off: %v", notes)
	}
	if containsLabel(h.addedLabels(), stagelabels.MergeCommitLabel) {
		t.Fatalf("label written through a suppressing gate: %v", h.api.added)
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if !rec.LabelsSuppressed || rec.MergeCommitNoted("e6d0ccb") {
		t.Fatalf("record after suppressed write: %+v", rec)
	}

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observeTrusted(adoptPR(7, "e6d0ccb", adoptedTrustedLabels...), contributorLaneReasonNoVerdict)
	if notes := mergeCommitComments(h.api); len(notes) != 0 {
		t.Fatalf("merge commit elected with labels suppressed: %v", notes)
	}
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("expected the re-home fallback: pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.AdoptReason != AdoptReasonSignoffRehome {
		t.Fatalf("record: %+v", rec)
	}
}

func TestMergeCommitRefusalTextNamesSuppressedLabels(t *testing.T) {
	obs := stageObservation{Number: 15521, Display: "acme/widget"}
	obs.Detail.MergeCommitRefused = mergeCommitRefusedLabelsSuppressed
	got := mergeCommitRefusalText(obs, "mallory")
	for _, want := range []string{"write-back gate", stagelabels.MergeCommitLabel, "gh pr merge 15521 --merge", "@mallory"} {
		if !strings.Contains(got, want) {
			t.Fatalf("refusal text missing %q:\n%s", want, got)
		}
	}
}

// The same head in a squash-only repo: the hive cannot pick a merge
// commit, so re-home under App attribution without lifting that policy.
func TestAdoptedInPlaceMixedAuthorshipSquashOnlySupersedes(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	p.SquashOnly = func(display string) bool { return display == "acme/widget" }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.api.commitDetails = map[int][]stageCommitFixture{7: mixedAuthorshipCommits}
	key := adoptInPlaceAt(h, "a1899cc", contributorLaneReasonNoVerdict)
	h.adopt.setCommitAuthor("a1899cc", testHiveAppBotLogin)

	h.clock.advance(time.Minute)
	h.observeTrusted(adoptPR(7, "a1899cc", adoptedTrustedLabels...), contributorLaneReasonNoVerdict)
	if len(mergeCommitComments(h.api)) != 0 {
		t.Fatalf("merge commit elected in a squash-only repo: %v", h.api.comments)
	}
	for _, a := range h.addedLabels() {
		if a == stagelabels.MergeCommitLabel {
			t.Fatalf("label written in a squash-only repo: %v", h.api.added)
		}
	}
	if len(stalledComments(h.api)) != 0 || len(h.adopt.pulls) != 1 {
		t.Fatalf("expected supersede, not a policy waiver: pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.AdoptedByHive || rec.AdoptReason != AdoptReasonSignoffRehome {
		t.Fatalf("record: %+v", rec)
	}
}

// A role/contributor PR cannot elect a merge commit, but can supersede.
func TestAdoptedInPlaceMixedAuthorshipContributorRoleSupersedes(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.api.commitDetails = map[int][]stageCommitFixture{7: mixedAuthorshipCommits}
	adoptInPlaceAt(h, "a1899cc", contributorLaneReasonNoVerdict)
	h.adopt.setCommitAuthor("a1899cc", testHiveAppBotLogin)

	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "a1899cc", adoptedLabels...), contributorLaneReasonNoVerdict)
	if len(stalledComments(h.api)) != 0 || len(h.adopt.pulls) != 1 {
		t.Fatalf("expected supersede: pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
	}
	for _, a := range h.addedLabels() {
		if a == stagelabels.MergeCommitLabel {
			t.Fatalf("label written for role/contributor: %v", h.api.added)
		}
	}
}

// A pure contributor head on a trusted PR: no label, no note, and a label
// left over from an earlier head comes off.
func TestAdoptedInPlacePureContributorHeadDropsMergeCommitLabel(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.api.commitDetails = map[int][]stageCommitFixture{7: {mixedAuthorshipCommits[0]}}
	adoptInPlaceAt(h, "c0ffee1", contributorLaneReasonNoVerdict)
	h.adopt.setCommitAuthor("c0ffee1", testHiveAppBotLogin)

	h.clock.advance(time.Minute)
	labelled := append(append([]string{}, adoptedTrustedLabels...), stagelabels.MergeCommitLabel, stagelabels.DCOYes.Label())
	h.observeTrusted(adoptPR(7, "c0ffee1", labelled...), contributorLaneReasonNoVerdict)
	if len(mergeCommitComments(h.api)) != 0 || len(stalledComments(h.api)) != 0 {
		t.Fatalf("comments: %v", h.api.comments)
	}
	if len(h.api.removed) != 1 || h.api.removed[0] != stagelabels.MergeCommitLabel || containsLabel(h.addedLabels(), stagelabels.MergeCommitLabel) {
		t.Fatalf("labels: added=%v removed=%v", h.api.added, h.api.removed)
	}
}

// #13777 as it sits when this ships: parked at 085 on
// adoption-stalled:hive-signoff-attribution by the previous build, head
// unchanged, nobody un-parked it. The first sweep after upgrade elects the
// merge commit, releases the escalation and puts the PR back at 074 with
// the label — no new head and no human needed.
func TestParkedSignoffAttributionEscalationReleasesOnMergeCommitElection(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.api.commitDetails = map[int][]stageCommitFixture{7: mixedAuthorshipCommits}
	key := adoptInPlaceAt(h, "a1899cc", contributorLaneReasonNoVerdict)
	h.adopt.setCommitAuthor("a1899cc", testHiveAppBotLogin)
	store := h.e.stageStoreOrShared()
	stalled := stagelabels.LaneReasonAdoptionStalledPrefix + stagelabels.LaneReasonHiveSignoffAttribution
	// The previous build's record: escalated and noted on this head, the
	// adopted-head clock long past the dwell, stage 085 written.
	store.MarkAdoptionEscalated(key, "a1899cc", stalled)
	store.Observe(key, stagelabels.NeedsDecision, stalled)
	h.clock.advance(adoptedDwell + time.Hour)
	if rec, _ := store.Get(key); !rec.AdoptionEscalated("a1899cc") || rec.Stage != stagelabels.NeedsDecision {
		t.Fatalf("precondition: %+v", rec)
	}
	parked := []string{stagelabels.NeedsDecision.Label(), "role/trusted", "author/clanker"}

	h.observeTrusted(adoptPR(7, "a1899cc", parked...), contributorLaneReasonNoVerdict)
	rec, _ := store.Get(key)
	if rec.AdoptionEscalated("a1899cc") || rec.Stage != stagelabels.AdoptedByHive || rec.Reason == stalled {
		t.Fatalf("escalation not released: %+v", rec)
	}
	if !rec.AdoptionEscalationNoted("a1899cc") {
		t.Fatalf("noted head forgotten: %+v", rec)
	}
	if added := h.addedLabels(); !containsLabel(added, stagelabels.MergeCommitLabel) || !containsLabel(added, stagelabels.AdoptedByHive.Label()) {
		t.Fatalf("085 → 074 with label: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if len(h.api.removed) != 1 || h.api.removed[0] != stagelabels.NeedsDecision.Label() {
		t.Fatalf("085 not removed: %v", h.api.removed)
	}
	if len(stalledComments(h.api)) != 0 {
		t.Fatalf("re-stalled: %v", h.api.comments)
	}
	if len(mergeCommitComments(h.api)) != 1 {
		t.Fatalf("merge-commit note: %v", h.api.comments)
	}

	// Same head again, label on, CI re-running under it: quiet.
	h.api.reset()
	h.clock.advance(time.Minute)
	labelled := append(append([]string{}, adoptedTrustedLabels...), stagelabels.MergeCommitLabel, stagelabels.DCOYes.Label())
	h.observeTrusted(adoptPR(7, "a1899cc", labelled...), "check-pending")
	if h.api.writes() != 0 {
		t.Fatalf("follow-up sweep wrote: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
}

func TestMergeMethodHonoursMergeCommitLabel(t *testing.T) {
	pr := &gh.PullRequest{
		Title:  gh.String("Add the widget"),
		Head:   &gh.PullRequestBranch{Ref: gh.String("feature/add-widget")},
		Labels: []*gh.Label{{Name: gh.String("role/trusted")}, {Name: gh.String(stagelabels.MergeCommitLabel)}},
	}
	method, reason := (&Engine{}).mergeMethodWithReason(context.Background(), "o", "r", pr)
	if method != "merge" || reason != mergeMethodReasonMixedAuthorship {
		t.Fatalf("labelled: method=%q reason=%q", method, reason)
	}
	if got := (&Engine{}).mergeMethodFor(context.Background(), "o", "r", pr); got != "merge" {
		t.Fatalf("mergeMethodFor = %q", got)
	}
	pr.Labels = []*gh.Label{{Name: gh.String("role/trusted")}}
	method, reason = (&Engine{}).mergeMethodWithReason(context.Background(), "o", "r", pr)
	if method != "squash" || reason != "" {
		t.Fatalf("unlabelled: method=%q reason=%q", method, reason)
	}
	in := mergeClaimInputs("merge", mergeMethodReasonMixedAuthorship, "abc", "trusted-author")
	if in["method"] != "merge" || in["method_reason"] != mergeMethodReasonMixedAuthorship || in["expect_sha"] != "abc" || in["lane"] != "trusted-author" {
		t.Fatalf("inputs: %v", in)
	}
	if _, ok := mergeClaimInputs("squash", "", "abc", "queued")["method_reason"]; ok {
		t.Fatal("empty reason recorded")
	}
}

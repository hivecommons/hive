package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin, 10-30 → 11-05: the write-back gate kept every 072/074/085
// label off 24 contributor and fork PRs while the stage engine recorded the
// stage. Bands and your-turn read labels, so a PR parked at 085 showed
// "blocked" with no reason and your_turn=false
// (projectbluefin/fsdk-containers#491, projectbluefin/utah-packages#812,
// which still wore a stale stage/074). The record now stands in for the
// label it could not write: band and ownership match the written label.
func TestSuppressedStageDrivesBandAndOwnership(t *testing.T) {
	now := ownershipNow11565
	mk := func(labels ...string) FrontendPR {
		return FrontendPR{PullRequest: github.PullRequest{
			Repo: "projectbluefin/utah-packages", Number: 812, Title: "fix: pin", Author: "arteeh",
			Labels: labels, Mergeable: github.MergeableNo, CreatedAt: now.Add(-6 * 24 * time.Hour), UpdatedAt: now.Add(-time.Hour),
		}}
	}
	decide := func(fp FrontendPR) (Ownership, string) {
		repos := []FrontendRepo{{Name: "utah-packages", Full: "projectbluefin/utah-packages", OpenPrs: []any{fp}}}
		decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
		o := repos[0].Ownership[ownershipKey(ownershipKindPR, 812)]
		entry, _ := frontendPullRequest(repos[0].OpenPrs[0])
		return o, prBand(entry.pr, entry.verdict, false, config.DashboardIssueBandsConfig{}, now, github.AutoMergeQueuedLabel, "").Band
	}

	written, writtenBand := decide(mk("role/trusted", stagelabels.NeedsDecision.Label()))
	if !written.YourTurn || written.ReasonCode != OwnershipReasonParkedNeedsDecision {
		t.Fatalf("fixture: written 085 = %+v, want your turn, parked_needs_decision", written)
	}

	store := stagelabels.NewStore("", func() time.Time { return now })
	key := stagelabels.Key("projectbluefin/utah-packages", 812)
	store.Observe(key, stagelabels.NeedsDecision, "adoption-stalled:reviewer-verdict:changes_requested")
	store.NoteLabelsSuppressed(key, true, "needs-opt-in-label — #812 carries none of hive-review")
	repos := []FrontendRepo{{Name: "utah-packages", Full: "projectbluefin/utah-packages", OpenPrs: []any{mk("role/trusted", stagelabels.AdoptedByHive.Label())}}}
	attachSuppressedStages(repos, store)
	fp := repos[0].OpenPrs[0].(FrontendPR)
	if fp.SuppressedStage != stagelabels.NeedsDecision.Label() || !strings.Contains(fp.SuppressedStageReason, "needs-opt-in-label") {
		t.Fatalf("attached = %q / %q, want the recorded 085 and the gate's reason", fp.SuppressedStage, fp.SuppressedStageReason)
	}
	if len(fp.Labels) != 2 || fp.Labels[1] != stagelabels.AdoptedByHive.Label() {
		t.Fatalf("the snapshot keeps GitHub's labels: %v", fp.Labels)
	}

	got, gotBand := decide(fp)
	if !got.YourTurn || got.ReasonCode != written.ReasonCode || got.Verb != written.Verb {
		t.Fatalf("suppressed 085 = %+v, want the written label's decision %+v", got, written)
	}
	if !strings.HasPrefix(got.Reason, written.Reason) || !strings.Contains(got.Reason, "kept the label off GitHub") {
		t.Fatalf("reason = %q, want %q plus why GitHub shows no label", got.Reason, written.Reason)
	}
	if gotBand != writtenBand || gotBand != "waiting" {
		t.Fatalf("band = %q, written label gives %q; want both waiting", gotBand, writtenBand)
	}

	// The same record with its write landed (labels carry the stage) or no
	// record at all: nothing is attached, the labels decide as before.
	store.NoteLabelsSuppressed(key, false, "")
	repos[0].OpenPrs[0] = mk("role/trusted", stagelabels.AdoptedByHive.Label())
	attachSuppressedStages(repos, store)
	if fp := repos[0].OpenPrs[0].(FrontendPR); fp.SuppressedStage != "" {
		t.Fatalf("landed write still attached %q", fp.SuppressedStage)
	}
	if o, _ := decide(repos[0].OpenPrs[0].(FrontendPR)); o.ReasonCode == OwnershipReasonParkedNeedsDecision {
		t.Fatalf("no suppression: the stale record must not park the PR: %+v", o)
	}
}

// The same PR in production order: cmd/hive builds the snapshot with
// BuildFrontendStatus (the your-turn decision runs inside it) and then
// publishes it, and the publish used to be the first place the record was
// stamped. Ownership never saw the recorded 085, so the PR stayed out of
// the inbox while the card and bands read the stage.
func TestBuildFrontendStatusOwnershipSeesSuppressedStage(t *testing.T) {
	now := time.Now()
	cfg := &config.Config{Project: config.ProjectConfig{Org: "projectbluefin", Repos: []string{"utah-packages"}}}
	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	srv := newTestServer()
	srv.contributorsDir = t.TempDir()
	store := stagelabels.NewStore("", time.Now)
	srv.stageStore = store
	hub := NewContributeWSHub(srv.logger, srv)
	mk := func(labels ...string) github.PullRequest {
		return github.PullRequest{
			Repo: "projectbluefin/utah-packages", Number: 812, Title: "fix: pin", Author: "arteeh",
			Labels: labels, Mergeable: github.MergeableNo, CreatedAt: now.Add(-6 * 24 * time.Hour), UpdatedAt: now.Add(-time.Hour),
		}
	}
	build := func(pr github.PullRequest) (*StatusPayload, Ownership) {
		t.Helper()
		actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{pr}}}
		payload := BuildFrontendStatus(gov.GetState(), actionable, nil, cfg, nil, gov, nil, nil, nil, nil, WithContributeHub(hub))
		if len(payload.Repos) != 1 {
			t.Fatalf("repos = %d, want 1", len(payload.Repos))
		}
		return payload, payload.Repos[0].Ownership[ownershipKey(ownershipKindPR, 812)]
	}

	_, written := build(mk("role/trusted", stagelabels.NeedsDecision.Label()))
	if !written.YourTurn || written.ReasonCode != OwnershipReasonParkedNeedsDecision {
		t.Fatalf("fixture: written 085 = %+v, want your turn, parked_needs_decision", written)
	}

	key := stagelabels.Key("projectbluefin/utah-packages", 812)
	store.Observe(key, stagelabels.NeedsDecision, "adoption-stalled:reviewer-verdict:changes_requested")
	store.NoteLabelsSuppressed(key, true, "needs-opt-in-label: #812 carries none of hive-review")
	payload, got := build(mk("role/trusted", stagelabels.AdoptedByHive.Label()))
	if !got.YourTurn || got.ReasonCode != written.ReasonCode || got.Verb != written.Verb {
		t.Fatalf("suppressed 085 built for publish = %+v, want the written label's decision %+v", got, written)
	}
	if !strings.HasPrefix(got.Reason, written.Reason) || !strings.Contains(got.Reason, "kept the label off GitHub") {
		t.Fatalf("reason = %q, want %q plus why GitHub shows no label", got.Reason, written.Reason)
	}

	srv.UpdateStatus(payload)
	srv.statusMu.RLock()
	published := srv.status
	srv.statusMu.RUnlock()
	fp, ok := published.Repos[0].OpenPrs[0].(FrontendPR)
	if !ok || fp.SuppressedStage != stagelabels.NeedsDecision.Label() {
		t.Fatalf("published PR = %#v, want the recorded 085 attached", published.Repos[0].OpenPrs[0])
	}
	if o := published.Repos[0].Ownership[ownershipKey(ownershipKindPR, 812)]; o.ReasonCode != OwnershipReasonParkedNeedsDecision {
		t.Fatalf("published ownership = %+v, want parked_needs_decision", o)
	}
}

func TestStageTrackerRowNamesSuppressedLabels(t *testing.T) {
	now := time.Date(2026, 10, 29, 20, 30, 0, 0, time.UTC)
	s, store, status := offlineClankerFixture(t, now, now.Add(-3*time.Hour))
	key := stagelabels.Key("hivecommons/hive", 15312)
	store.Observe(key, stagelabels.NeedsDecision, "contributor-lane:decision-requested")
	store.NoteLabelsSuppressed(key, true, "needs-opt-in-label — #15312 carries none of hive-review")
	if it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 15312); !strings.HasPrefix(it.LabelsSuppressed, "needs-opt-in-label") {
		t.Fatalf("row labels_suppressed = %q, want the gate's reason", it.LabelsSuppressed)
	}
	store.NoteLabelsSuppressed(key, false, "")
	if it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 15312); it.LabelsSuppressed != "" {
		t.Fatalf("landed write: row labels_suppressed = %q, want empty", it.LabelsSuppressed)
	}

	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, "const assert = require('assert');\n"+jsFunc(t, html, "stageTrackerLabelsSuppressedText")+`
assert.strictEqual(stageTrackerLabelsSuppressedText({}), '');
const tip = stageTrackerLabelsSuppressedText({ labels_suppressed: 'needs-opt-in-label — #812 carries none of hive-review' });
assert.ok(tip.startsWith('Stage label not on GitHub'), tip);
assert.ok(tip.includes('needs-opt-in-label'), tip);
`)
}

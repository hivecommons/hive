package automerge

// Tests for the 090 ready-for-human-merge entry's quiet-mode delivery
// (hivecommons/hive#18606): a PR whose only objection is the author's trust
// tier gets a stage-ready line on its hive:status card instead of a comment
// of its own; a held 090 (sentinel alert, hold label) still posts its own
// notifying comment, as does every 085 needs-decision entry.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func quietReadyEngine(t *testing.T) (*stageAPI, func(reason string, labels ...string), *stagelabels.Store) {
	t.Helper()
	api, closeAPI := seededStageAPI(t)
	t.Cleanup(closeAPI)
	api.statefulComments = true
	api.posted = map[int][]map[string]any{9: {{
		"id":   int64(900),
		"body": prstatus.Upsert("", prstatus.KeyStageStalled, "<!-- hive:stage-stalled --> waiting"),
		"user": map[string]any{"login": testHiveAppBotLogin},
	}}}
	clock := &contributorLaneClock{at: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	e.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, QuietNotifications: true} }
	sweep := func(reason string, labels ...string) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(9, labels...), "untrusted-author-role", false, nil)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(9, labels...), reason, false, nil)
		e.noteStageRole("acme", "widget", 9, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return api, sweep, store
}

func cardOf(t *testing.T, api *stageAPI) string {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, c := range api.posted[9] {
		if body, _ := c["body"].(string); prstatus.IsStatusComment(body) {
			return body
		}
	}
	t.Fatal("PR 9 has no status card")
	return ""
}

func TestReadyEntryQuietFoldsIntoStatusCard(t *testing.T) {
	api, sweep, store := quietReadyEngine(t)
	sweep(contributorLaneReasonClaimTier, "role/contributor", "author/human", "stage/050-review-pending")
	if got := stageOf(t, store, 9); got != stagelabels.ReadyForHumanMerge {
		t.Fatalf("stage=%v want 090", got)
	}
	if len(api.comments) != 0 {
		t.Fatalf("quiet 090 posted %d comments, want none: %q", len(api.comments), api.comments)
	}
	card := cardOf(t, api)
	if _, ok := prstatus.Get(card, prstatus.KeyStageStalled); !ok {
		t.Fatalf("existing card section lost:\n%s", card)
	}
	section, ok := prstatus.Get(card, prstatus.KeyStageReady)
	if !ok || !strings.Contains(section, "Ready for a human merge: verdict approved `sha`") || !strings.Contains(section, "Blocked only by `role/contributor`") || !strings.Contains(section, "`/lgtm` `/approve`") {
		t.Fatalf("stage-ready section = %q (found %v)", section, ok)
	}
	if strings.Contains(card, stagelabels.ReadyCommentMarker) {
		t.Fatalf("the card must not carry the entry-comment marker (the entry upsert would overwrite it):\n%s", card)
	}

	// Same head and cause again: nothing is written.
	edits := len(api.commentEdits)
	sweep(contributorLaneReasonClaimTier, "role/contributor", "author/human", "stage/090-ready-for-human-merge")
	if len(api.comments) != 0 || len(api.commentEdits) != edits {
		t.Fatalf("re-sweep wrote: comments=%q edits=%d→%d", api.comments, edits, len(api.commentEdits))
	}
}

// A held 090 asks a person to clear the gate, so it still posts its own
// comment in quiet mode, and the plain ready line is dropped from the card.
func TestReadyEntryQuietHeldStillComments(t *testing.T) {
	api, sweep, _ := quietReadyEngine(t)
	sweep(contributorLaneReasonClaimTier, "role/contributor", "author/human", "stage/050-review-pending")
	if _, ok := prstatus.Get(cardOf(t, api), prstatus.KeyStageReady); !ok {
		t.Fatal("setup: no ready line on the card")
	}
	sweep(contributorLaneReasonSentinelBlocked, "role/contributor", "author/human", "stage/090-ready-for-human-merge", "sentinel-alert")
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.ReadyCommentMarker) || !strings.Contains(api.comments[0], "Held: a sentinel alert is open on this PR.") {
		t.Fatalf("held 090 must post its own comment: %q", api.comments)
	}
	if prstatus.IsStatusComment(api.comments[0]) {
		t.Fatalf("held comment is the card: %q", api.comments[0])
	}
	if section, ok := prstatus.Get(cardOf(t, api), prstatus.KeyStageReady); ok {
		t.Fatalf("stale ready line kept next to the hold: %q", section)
	}
}

func TestReadyEntryFolds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route stagelabels.Route
		want  bool
	}{
		{"plain ready", stagelabels.Route{Stage: stagelabels.ReadyForHumanMerge}, true},
		{"held ready", stagelabels.Route{Stage: stagelabels.ReadyForHumanMerge, Gates: []string{"a sentinel alert is open on this PR"}}, false},
		{"manual merge", stagelabels.Route{Stage: stagelabels.ReadyForHumanMerge, ManualMerge: true}, false},
		{"needs decision", stagelabels.Route{Stage: stagelabels.NeedsDecision}, false},
	} {
		if got := readyEntryFolds(tc.route); got != tc.want {
			t.Errorf("%s: readyEntryFolds=%v want %v", tc.name, got, tc.want)
		}
	}
}

package automerge

// Tests for the auto-merge note's delivery (hivecommons/hive#18452): in
// github.notifications.mode: quiet it is a section of the PR's hive:status
// card, edited in place; in mode normal it stays its own comment; and a
// merge that waits on a person still gets its own notifying comment.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func quietStagePolicy(quiet bool) func() StageLabelPolicy {
	return func() StageLabelPolicy { return StageLabelPolicy{QuietNotifications: quiet} }
}

func runTrustedMerge(t *testing.T, fx *trustedSweepFixture, opts Options) AutoMergeSweepEvent {
	t.Helper()
	api := newTrustedSweepAPI(t, fx)
	t.Cleanup(api.Close)
	policy := TrustedAuthorPolicy{Enabled: true}
	engine := newTrustedSweepEngine(api.URL, policy, opts)
	event, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil || reason != "" {
		t.Fatalf("reason=%q err=%v, want a merge", reason, err)
	}
	if fx.merges != 1 || event.MergeSHA != "merge7" {
		t.Fatalf("merges=%d event=%+v, want one merge", fx.merges, event)
	}
	return event
}

func TestAutoMergeNoteQuietCreatesStatusCard(t *testing.T) {
	fx := trustedSweepFixture{}
	runTrustedMerge(t, &fx, Options{StageLabelPolicy: quietStagePolicy(true)})
	if fx.comments != 1 {
		t.Fatalf("comments=%d, want the one status card created", fx.comments)
	}
	if !prstatus.IsStatusComment(fx.lastComment) {
		t.Fatalf("quiet note must be the hive:status card, got %q", fx.lastComment)
	}
	section, ok := prstatus.Get(fx.lastComment, prstatus.KeyAutoMerge)
	if !ok || !strings.Contains(section, "merged by Hive trusted-author auto-merge") || !strings.Contains(section, "CI green at sha7") {
		t.Fatalf("auto-merge section = %q (found %v)", section, ok)
	}
	if strings.Contains(section, "@alice") {
		t.Fatalf("folded note must not mention the author live: %q", section)
	}
}

func TestAutoMergeNoteQuietEditsExistingCardInPlace(t *testing.T) {
	existing := prstatus.Upsert("", prstatus.KeyStageStalled, "<!-- hive:stage-stalled --> waiting")
	fx := trustedSweepFixture{issueComments: []map[string]any{
		{"id": 501, "body": "a person's comment", "user": map[string]string{"login": "bob"}},
		{"id": 502, "body": existing, "user": map[string]string{"login": testHiveAppBotLogin}},
	}}
	runTrustedMerge(t, &fx, Options{StageLabelPolicy: quietStagePolicy(true)})
	if fx.comments != 0 || fx.edits != 1 {
		t.Fatalf("comments=%d edits=%d, want the card edited in place and no new comment", fx.comments, fx.edits)
	}
	if _, ok := prstatus.Get(fx.lastEdit, prstatus.KeyStageStalled); !ok {
		t.Fatalf("existing section lost: %q", fx.lastEdit)
	}
	if section, ok := prstatus.Get(fx.lastEdit, prstatus.KeyAutoMerge); !ok || !strings.Contains(section, "merged by Hive trusted-author auto-merge") {
		t.Fatalf("auto-merge section = %q (found %v)", section, ok)
	}
}

func TestAutoMergeNoteQuietOperatorApprovalFoldsWithoutMention(t *testing.T) {
	fx := trustedSweepFixture{author: "mallory"}
	api := newTrustedSweepAPI(t, &fx)
	defer api.Close()
	policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}
	store := stagelabels.NewStore("", nil)
	key := stageKey("acme", "widget", 7)
	store.Observe(key, stagelabels.ReadyForHumanMerge, "untrusted-author-role")
	if !store.NoteAdmission(key, "sha7", stagelabels.OperatorApprovalAdmission("clubanderson"), time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("NoteAdmission refused the stage record")
	}
	engine := newTrustedSweepEngine(api.URL, policy, Options{StageStore: store, StageLabelPolicy: quietStagePolicy(true)})
	if _, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil || reason != "" || fx.merges != 1 {
		t.Fatalf("reason=%q err=%v merges=%d, want a merge", reason, err, fx.merges)
	}
	section, ok := prstatus.Get(fx.lastComment, prstatus.KeyAutoMerge)
	if fx.comments != 1 || !ok || !strings.Contains(section, "on behalf of") || !strings.Contains(section, "clubanderson") {
		t.Fatalf("comments=%d section=%q", fx.comments, section)
	}
	if strings.Contains(section, "@clubanderson") || strings.Contains(section, "@mallory") {
		t.Fatalf("folded operator note must not mention anyone live: %q", section)
	}
}

func TestAutoMergeNoteNormalModeKeepsStandaloneComment(t *testing.T) {
	fx := trustedSweepFixture{}
	runTrustedMerge(t, &fx, Options{StageLabelPolicy: quietStagePolicy(false)})
	if fx.comments != 1 || fx.edits != 0 {
		t.Fatalf("comments=%d edits=%d, want one standalone comment", fx.comments, fx.edits)
	}
	if prstatus.IsStatusComment(fx.lastComment) || !strings.HasPrefix(fx.lastComment, "merged by Hive trusted-author auto-merge: author @alice") {
		t.Fatalf("normal-mode note changed: %q", fx.lastComment)
	}
}

func TestAutoMergeNoteQuietWriteFailureDoesNotFailMerge(t *testing.T) {
	fx := trustedSweepFixture{listCommentsStatus: http.StatusInternalServerError}
	runTrustedMerge(t, &fx, Options{StageLabelPolicy: quietStagePolicy(true)})
	if fx.comments != 0 || fx.edits != 0 {
		t.Fatalf("comments=%d edits=%d, want no write when the card cannot be read", fx.comments, fx.edits)
	}
}

// TestNeedsHumanEntryStillCommentsInQuietMode: folding the merge note must
// not fold the notice a person acts on. A PR the hive cannot merge without
// a person (085 needs-decision) still gets its own notifying comment in
// quiet mode.
func TestNeedsHumanEntryStillCommentsInQuietMode(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	e.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, QuietNotifications: true} }
	api.reset()
	pr := stagePR(13, "role/contributor", "author/clanker", "stage/050-review-pending")
	pr.Body = strPtr("Fixes the widget.\n\n-- hive: backend=pi model=gpt-6\n")
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonNoVerdict, false, nil)
	e.noteStageVerdict("acme", "widget", 13, *verdict)
	e.noteStageRole("acme", "widget", 13, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stageOf(t, store, 13); got != stagelabels.NeedsDecision {
		t.Fatalf("stage=%v want 085", got)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.DecisionCommentMarker) || prstatus.IsStatusComment(api.comments[0]) {
		t.Fatalf("needs-human entry must stay its own comment in quiet mode: %q", api.comments)
	}
}

func strPtr(s string) *string { return &s }

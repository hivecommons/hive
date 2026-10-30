package automerge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func quietStageEngine(t *testing.T, api *stageAPI, clock *contributorLaneClock) (*Engine, *stagelabels.Store) {
	t.Helper()
	api.statefulComments = true
	srv := newStageAPI(t, api)
	t.Cleanup(srv.Close)
	e, _, store := newStageEngine(t, srv.URL, clock)
	e.stageLabelPolicy = func() StageLabelPolicy {
		return StageLabelPolicy{Enabled: true, StalledRequireQuiet: true, QuietNotifications: true}
	}
	return e, store
}

// Quiet mode: the 071 note, the stall note and a real regression all land
// in ONE hive status comment that is created once and then edited in place.
// Every later change is a PATCH (no email), never a second comment.
func TestQuietNotificationsStageNotesFoldIntoOneEditedComment(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	clock := &contributorLaneClock{at: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	e, _ := quietStageEngine(t, api, clock)
	ctx := context.Background()
	approve := ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	observe := func(labels []string, reason string, err error) {
		t.Helper()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(11, labels...), reason, false, err)
		e.noteStageVerdict("acme", "widget", 11, approve)
		e.noteStageRole("acme", "widget", 11, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	observe(nil, "review-state-check", errors.New("boom")) // → 080
	clock.advance(30 * time.Minute)
	observe([]string{"stage/080-awaiting-merge-admission", "role/trusted", "author/human"}, "review-state-check", errors.New("boom")) // stalled
	if len(api.comments) != 1 || !prstatus.IsStatusComment(api.comments[0]) {
		t.Fatalf("stall must create the single status comment, got %q", api.comments)
	}
	if got, ok := prstatus.Get(api.comments[0], prstatus.KeyStageStalled); !ok || !strings.Contains(got, "no activity for 30m0s") {
		t.Fatalf("stall section missing: %s", api.comments[0])
	}
	// Regression to 070 on a real failure: same comment, edited, both
	// sections present.
	observe([]string{"stage/080-awaiting-merge-admission", "stage/stalled", "role/trusted", "author/human"}, "check-failing:unit", nil)
	if len(api.comments) != 1 {
		t.Fatalf("regression posted a second comment: %q", api.comments)
	}
	if len(api.commentEdits) == 0 {
		t.Fatal("regression did not edit the status comment")
	}
	last := api.commentEdits[len(api.commentEdits)-1]
	if got, ok := prstatus.Get(last, prstatus.KeyStageRegressed); !ok || !strings.Contains(got, "080-awaiting-merge-admission → 071-needs-author: check-failing:unit") {
		t.Fatalf("edited comment lacks the regression: %s", last)
	}
	if _, ok := prstatus.Get(last, prstatus.KeyStageStalled); !ok {
		t.Fatalf("stall section lost on edit: %s", last)
	}
}

// Quiet mode: a check going back to pending after a push is not a
// regression and produces neither a comment nor an edit.
func TestQuietNotificationsPendingIsNotARegression(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	clock := &contributorLaneClock{at: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	e, _ := quietStageEngine(t, api, clock)
	ctx := context.Background()
	observe := func(labels []string, reason string) {
		t.Helper()
		e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", stagePR(10, labels...), reason, false, nil)
		e.noteStageRole("acme", "widget", 10, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	observe(nil, "head-changed-since-eval")
	clock.advance(time.Minute)
	observe([]string{"stage/100-merge-queue", "role/trusted", "author/human"}, "required-check-pending:unit")
	if len(api.comments) != 0 || len(api.commentEdits) != 0 {
		t.Fatalf("pending check posted a regression: comments=%q edits=%q", api.comments, api.commentEdits)
	}
}

func TestRegressionNotable(t *testing.T) {
	for reason, want := range map[string]bool{
		"check-failing:unit":                            true,
		"required-check-failing:unit":                   true,
		"conflicting":                                   true,
		"required-check-pending:unit":                   false,
		"check-pending":                                 false,
		"merge-train:queued-behind#15817":               false,
		"merge-train:rebasing":                          false,
		"base-refresh:pending":                          false,
		"pending: head pushed 54s ago (< min_head_age)": false,
	} {
		if got := regressionNotable(reason); got != want {
			t.Errorf("regressionNotable(%q) = %v, want %v", reason, got, want)
		}
	}
}

// Being queued behind another PR in the merge train, or being rebased by the
// hive, is the hive's own wait: it must never read as stalled.
func TestStageReasonWaitsOnHiveMergeTrain(t *testing.T) {
	for _, reason := range []string{"merge-train:queued-behind#15817", "merge-train:rebasing", "base-refresh:pending"} {
		if !stageReasonWaitsOnHive(reason) {
			t.Errorf("stageReasonWaitsOnHive(%q) = false", reason)
		}
	}
	if stageReasonWaitsOnHive("check-failing:unit") {
		t.Error("a failing check is not a hive wait")
	}
}

// Quiet mode: the trusted-human 071 note @-mentions the author once per
// distinct cause, not once per head; a new head with the same cause edits
// the status comment without a mention, and a new cause mentions again.
func TestQuietNotificationsHuman071MentionOncePerCause(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	clock := &contributorLaneClock{at: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	e, _ := quietStageEngine(t, api, clock)
	sweep := func(head, reason string) {
		t.Helper()
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", trustedHuman071PR(41, head), reason, false, nil)
		e.noteStageRole("acme", "widget", 41, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	section := func() string {
		t.Helper()
		body := ""
		for _, c := range api.posted[41] {
			if b, _ := c["body"].(string); prstatus.IsStatusComment(b) {
				body = b
			}
		}
		if body == "" {
			t.Fatalf("no status comment: posted=%v", api.posted)
		}
		text, _ := prstatus.Get(body, prstatus.KeyHuman071Note)
		return text
	}
	sweep("aaa", "conflicting")
	if len(api.comments) != 1 {
		t.Fatalf("want one status comment, got %q", api.comments)
	}
	if s := section(); !strings.Contains(s, "@clubanderson") || !strings.Contains(s, "head=aaa") {
		t.Fatalf("first note must mention the author: %s", s)
	}
	clock.at = clock.at.Add(10 * time.Minute)
	sweep("bbb", "conflicting")
	if len(api.comments) != 1 {
		t.Fatalf("new head created a second comment: %q", api.comments)
	}
	if s := section(); strings.Contains(s, "@clubanderson") || !strings.Contains(s, "head=bbb") {
		t.Fatalf("same cause on a new head must update without a mention: %s", s)
	}
	sweep("ccc", "check-failing:unit")
	if s := section(); !strings.Contains(s, "@clubanderson") || !strings.Contains(s, "head=ccc") {
		t.Fatalf("new cause must mention again: %s", s)
	}
	if len(api.comments) != 1 {
		t.Fatalf("still one comment expected, got %q", api.comments)
	}
}

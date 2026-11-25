package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/stalehold"
	"github.com/hivecommons/hive/pkg/timeline"
)

const (
	staleHoldTestHead   = "0123456789abcdef0123456789abcdef01234567"
	staleHoldTestNumber = 19
)

var staleHoldTestStart = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

// fakeHoldWriter records every forge write the stale-hold sweep performs.
type fakeHoldWriter struct {
	comments  []string
	added     []string
	removed   []string
	failLabel string
}

func (f *fakeHoldWriter) CreateIssueComment(_ context.Context, repo string, number int, body string) error {
	f.comments = append(f.comments, stalehold.Key(repo, number)+": "+body)
	return nil
}

func (f *fakeHoldWriter) AddLabels(_ context.Context, _ string, _ int, labels []string) error {
	f.added = append(f.added, labels...)
	return nil
}

func (f *fakeHoldWriter) RemoveLabel(_ context.Context, _ string, _ int, label string) error {
	if label == f.failLabel {
		return errors.New("boom")
	}
	f.removed = append(f.removed, label)
	return nil
}

// commentOnlyWriter cannot remove labels.
type commentOnlyWriter struct{ comments int }

func (w *commentOnlyWriter) CreateIssueComment(context.Context, string, int, string) error {
	w.comments++
	return nil
}
func (w *commentOnlyWriter) AddLabels(context.Context, string, int, []string) error { return nil }

func noStaleHoldEnv(string) string { return "" }

func greenHeldPR(labels ...string) github.PullRequest {
	return github.PullRequest{Repo: "widgets", Number: staleHoldTestNumber, HeadSHA: staleHoldTestHead, Labels: labels, CIStatus: "success"}
}

func heldActionable(prs ...github.PullRequest) *github.ActionableResult {
	a := &github.ActionableResult{}
	a.PRs.Held = prs
	return a
}

type sentNotice struct{ title, message string }

func staleHoldTestDeps(w *fakeHoldWriter, store *stalehold.Store, rec lifecycleRecorder, sent *[]sentNotice, at time.Time) staleHoldSweepDeps {
	return staleHoldSweepDeps{
		writer: w, rec: rec, store: store, now: at, getenv: noStaleHoldEnv,
		send: func(title, message string) { *sent = append(*sent, sentNotice{title, message}) },
	}
}

func TestStaleHoldCI(t *testing.T) {
	completed := func(conclusion string) *github.PRCIActivity {
		return &github.PRCIActivity{Checks: []github.PRActivityCheck{
			{Name: "build", State: "completed", Conclusion: "success"},
			{Name: "tide", State: "pending"},
			{Name: "other", State: "completed", Conclusion: conclusion},
		}}
	}
	for _, tc := range []struct {
		name string
		pr   github.PullRequest
		want string
	}{
		{"success", github.PullRequest{CIStatus: "success"}, stalehold.CIGreen},
		{"failure", github.PullRequest{CIStatus: "failure"}, stalehold.CIRed},
		{"pending", github.PullRequest{CIStatus: "pending"}, stalehold.CIUnknown},
		{"not enriched", github.PullRequest{}, stalehold.CIUnknown},
		{"required red", github.PullRequest{CIStatus: "success", Protection: &github.ProtectionFacts{FailingRequiredChecks: []string{"DCO"}}}, stalehold.CIRed},
		{"required missing", github.PullRequest{CIStatus: "success", Protection: &github.ProtectionFacts{MissingRequiredChecks: []string{"DCO"}}}, stalehold.CIUnknown},
		{"required fine", github.PullRequest{CIStatus: "success", Protection: &github.ProtectionFacts{RequiredChecksKnown: true}}, stalehold.CIGreen},
		{"skipped and neutral", github.PullRequest{CIStatus: "success", CIActivity: completed("SKIPPED")}, stalehold.CIGreen},
		{"neutral", github.PullRequest{CIStatus: "success", CIActivity: completed("neutral")}, stalehold.CIGreen},
		{"cancelled", github.PullRequest{CIStatus: "success", CIActivity: completed("cancelled")}, stalehold.CIRed},
		{"timed out", github.PullRequest{CIStatus: "success", CIActivity: completed("timed_out")}, stalehold.CIRed},
	} {
		if got := staleHoldCI(tc.pr); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRunStaleHoldSweepRaisesOnceWithBlockedJourney(t *testing.T) {
	cfg := escalationTestConfig()
	store := stalehold.Load("")
	w := &fakeHoldWriter{}
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	var sent []sentNotice
	ctx := context.Background()
	act := heldActionable(greenHeldPR("do-not-merge/hold"))

	runStaleHoldSweep(ctx, cfg, act, staleHoldTestDeps(w, store, rec, &sent, staleHoldTestStart), discardLogger())
	if len(sent) != 0 {
		t.Fatalf("raised on first sighting: %v", sent)
	}
	at := staleHoldTestStart.Add(config.DefaultStaleHoldAfter)
	runStaleHoldSweep(ctx, cfg, act, staleHoldTestDeps(w, store, rec, &sent, at), discardLogger())
	if len(sent) != 1 {
		t.Fatalf("want one notice at the threshold, got %v", sent)
	}
	msg := sent[0].message
	for _, want := range []string{"acme/widgets#19", "do-not-merge/hold", "2h0m", "0123456", "Release the hold"} {
		if !strings.Contains(msg, want) {
			t.Errorf("notice %q is missing %q", msg, want)
		}
	}
	j, ok := rec.store.Journey("widgets#19")
	if !ok || j.Current != timeline.KindBlocked {
		t.Fatalf("journey = %+v ok=%v", j, ok)
	}
	if st := j.Stages[timeline.KindBlocked]; st == nil || st.Attrs["reason"] != staleHoldBlockedReason || st.Attrs["hold_labels"] != "do-not-merge/hold" {
		t.Fatalf("blocked stage = %+v", j.Stages[timeline.KindBlocked])
	}
	runStaleHoldSweep(ctx, cfg, act, staleHoldTestDeps(w, store, rec, &sent, at.Add(10*config.DefaultStaleHoldAfter)), discardLogger())
	if len(sent) != 1 {
		t.Fatalf("raised again: %v", sent)
	}
	// Raising never writes to the forge, and a plain hold is never removed.
	if len(w.removed) != 0 || len(w.added) != 0 || len(w.comments) != 0 {
		t.Fatalf("forge writes on a plain hold: removed=%v added=%v comments=%v", w.removed, w.added, w.comments)
	}
}

func TestRunStaleHoldSweepHonoursTheConfiguredThreshold(t *testing.T) {
	cfg := escalationTestConfig()
	cfg.Escalation.StaleHoldAfter = "off"
	store := stalehold.Load("")
	var sent []sentNotice
	act := heldActionable(greenHeldPR("hold"))
	for _, at := range []time.Time{staleHoldTestStart, staleHoldTestStart.Add(100 * config.DefaultStaleHoldAfter)} {
		runStaleHoldSweep(context.Background(), cfg, act, staleHoldTestDeps(&fakeHoldWriter{}, store, nil, &sent, at), discardLogger())
	}
	if len(sent) != 0 {
		t.Fatalf("disabled threshold raised %v", sent)
	}

	cfg.Escalation.StaleHoldAfter = "10m"
	store = stalehold.Load("")
	deps := staleHoldTestDeps(&fakeHoldWriter{}, store, nil, &sent, staleHoldTestStart)
	runStaleHoldSweep(context.Background(), cfg, act, deps, discardLogger())
	deps.now = staleHoldTestStart.Add(10 * time.Minute)
	runStaleHoldSweep(context.Background(), cfg, act, deps, discardLogger())
	if len(sent) != 1 {
		t.Fatalf("configured threshold not honoured: %v", sent)
	}
	// No notification channel: still recorded as notified, never re-raised.
	deps.send = nil
	deps.now = staleHoldTestStart.Add(time.Hour)
	runStaleHoldSweep(context.Background(), cfg, act, deps, discardLogger())
}

func TestRunStaleHoldSweepReleasesOnlyOptedInGreenHolds(t *testing.T) {
	cfg := escalationTestConfig()
	store := stalehold.Load("")
	w := &fakeHoldWriter{}
	var sent []sentNotice
	ctx := context.Background()

	// Opt-in but red: nothing is removed.
	red := greenHeldPR("do-not-merge/hold", config.DefaultReleaseWhenGreenLabel)
	red.CIStatus = "failure"
	runStaleHoldSweep(ctx, cfg, heldActionable(red), staleHoldTestDeps(w, store, nil, &sent, staleHoldTestStart), discardLogger())
	if len(w.removed) != 0 {
		t.Fatalf("released while red: %v", w.removed)
	}

	// Green: the hold labels come off first, then the opt-in label, with one
	// comment. Nothing is added.
	pr := greenHeldPR("do-not-merge/hold", "size/L", config.DefaultReleaseWhenGreenLabel, "hold")
	runStaleHoldSweep(ctx, cfg, heldActionable(pr), staleHoldTestDeps(w, store, nil, &sent, staleHoldTestStart), discardLogger())
	want := []string{"do-not-merge/hold", "hold", config.DefaultReleaseWhenGreenLabel}
	if strings.Join(w.removed, ",") != strings.Join(want, ",") {
		t.Fatalf("removed %v, want %v", w.removed, want)
	}
	if len(w.added) != 0 {
		t.Fatalf("release added labels: %v", w.added)
	}
	if len(w.comments) != 1 || !strings.Contains(w.comments[0], "No approval or lgtm label was added") ||
		!strings.Contains(w.comments[0], "`do-not-merge/hold`, `hold`") {
		t.Fatalf("comments = %v", w.comments)
	}
	if len(sent) != 0 {
		t.Fatalf("a release is not a notification: %v", sent)
	}

	// Only the opt-in label left (an earlier pass removed the hold): it is
	// removed without a second comment.
	w2 := &fakeHoldWriter{}
	runStaleHoldSweep(ctx, cfg, heldActionable(greenHeldPR(config.DefaultReleaseWhenGreenLabel)), staleHoldTestDeps(w2, store, nil, &sent, staleHoldTestStart), discardLogger())
	if strings.Join(w2.removed, ",") != config.DefaultReleaseWhenGreenLabel || len(w2.comments) != 0 {
		t.Fatalf("retry: removed=%v comments=%v", w2.removed, w2.comments)
	}

	// Releases switched off: the opt-in label is just another label.
	cfg.Escalation.ReleaseWhenGreenLabel = "off"
	w3 := &fakeHoldWriter{}
	runStaleHoldSweep(ctx, cfg, heldActionable(pr), staleHoldTestDeps(w3, store, nil, &sent, staleHoldTestStart), discardLogger())
	if len(w3.removed) != 0 {
		t.Fatalf("released with releases off: %v", w3.removed)
	}
}

func TestReleaseGreenHoldStopsOnFailure(t *testing.T) {
	cfg := escalationTestConfig()
	ctx := context.Background()
	pr := greenHeldPR("do-not-merge/hold", config.DefaultReleaseWhenGreenLabel)
	var sent []sentNotice

	// The hold label cannot be removed: no comment, and the opt-in label
	// stays so the next pass retries.
	w := &fakeHoldWriter{failLabel: "do-not-merge/hold"}
	runStaleHoldSweep(ctx, cfg, heldActionable(pr), staleHoldTestDeps(w, stalehold.Load(""), nil, &sent, staleHoldTestStart), discardLogger())
	if len(w.removed) != 0 || len(w.comments) != 0 {
		t.Fatalf("partial release: removed=%v comments=%v", w.removed, w.comments)
	}

	// The opt-in label cannot be removed: the hold is already gone and said so.
	w = &fakeHoldWriter{failLabel: config.DefaultReleaseWhenGreenLabel}
	runStaleHoldSweep(ctx, cfg, heldActionable(pr), staleHoldTestDeps(w, stalehold.Load(""), nil, &sent, staleHoldTestStart), discardLogger())
	if strings.Join(w.removed, ",") != "do-not-merge/hold" || len(w.comments) != 1 {
		t.Fatalf("opt-in failure: removed=%v comments=%v", w.removed, w.comments)
	}

	// A writer that cannot remove labels releases nothing and says nothing.
	c := &commentOnlyWriter{}
	deps := staleHoldTestDeps(nil, stalehold.Load(""), nil, &sent, staleHoldTestStart)
	deps.writer = c
	runStaleHoldSweep(ctx, cfg, heldActionable(pr), deps, discardLogger())
	if c.comments != 0 {
		t.Fatalf("comment-only writer commented %d times", c.comments)
	}
}

func TestRunStaleHoldSweepGuards(t *testing.T) {
	var sent []sentNotice
	deps := staleHoldTestDeps(&fakeHoldWriter{}, nil, nil, &sent, staleHoldTestStart)
	runStaleHoldSweep(context.Background(), escalationTestConfig(), heldActionable(greenHeldPR("hold")), deps, discardLogger())
	deps.store = stalehold.Load("")
	runStaleHoldSweep(context.Background(), escalationTestConfig(), nil, deps, discardLogger())
	// A zero clock and a nil config fall back to now and the defaults.
	deps.now = time.Time{}
	runStaleHoldSweep(context.Background(), nil, heldActionable(greenHeldPR("hold")), deps, discardLogger())
	if _, ok := deps.store.Entry("widgets", staleHoldTestNumber); !ok {
		t.Fatal("nil config: held PR not tracked")
	}
	if len(sent) != 0 {
		t.Fatalf("guards raised %v", sent)
	}
	if staleHoldObservations(nil, nil) != nil {
		t.Fatal("nil actionable must yield no observations")
	}
	// Recording tolerates a nil recorder and a recorder without a store.
	recordStaleHoldBlocked(context.Background(), nil, "acme", stalehold.Action{Repo: "acme/widgets", Number: 1}, "2h0m")
	recordStaleHoldBlocked(context.Background(), &fakeLifecycleRecorder{}, "acme", stalehold.Action{Repo: "acme/widgets", Number: 1}, "2h0m")
	rec := &fakeLifecycleRecorder{store: timeline.NewStore()}
	recordStaleHoldBlocked(context.Background(), rec, "acme", stalehold.Action{Repo: "", Number: 0}, "2h0m")
	if got := len(rec.store.Journeys(0)); got != 0 {
		t.Fatalf("empty ref recorded %d journeys", got)
	}
}

func TestStaleHoldObservationsCoverItemsAndHeld(t *testing.T) {
	a := &github.ActionableResult{}
	a.PRs.Held = []github.PullRequest{greenHeldPR("hold")}
	a.PRs.Items = []github.PullRequest{{Repo: "acme/other", Number: 3, HeadSHA: "x", CIStatus: "failure"}}
	got := staleHoldObservations(escalationTestConfig(), a)
	if len(got) != 2 || got[0].Repo != "acme/widgets" || got[0].CI != stalehold.CIGreen || got[1].Repo != "acme/other" || got[1].CI != stalehold.CIRed {
		t.Fatalf("observations = %+v", got)
	}
}

func TestStaleHoldFormatting(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:                  "30s",
		time.Minute:                       "1m",
		2 * time.Hour:                     "2h0m",
		11*time.Hour + 20*time.Minute + 5: "11h20m",
	} {
		if got := staleHoldDuration(d); got != want {
			t.Errorf("staleHoldDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if got := shortStaleHoldSHA("abc"); got != "abc" {
		t.Errorf("short sha = %q", got)
	}
	if got := shortStaleHoldSHA(staleHoldTestHead); got != "0123456" {
		t.Errorf("short sha = %q", got)
	}
}

func TestStaleHoldNotifierSend(t *testing.T) {
	if staleHoldNotifierSend(nil) != nil {
		t.Fatal("nil notifier must give a nil sender")
	}
	send := staleHoldNotifierSend(notify.New(config.NotificationsConfig{}, discardLogger()))
	if send == nil {
		t.Fatal("notifier sender is nil")
	}
	send("title", "message") // no channels configured: a no-op
}

func TestGetStaleHoldStore(t *testing.T) {
	if getStaleHoldStore() == nil || getStaleHoldStore() != getStaleHoldStore() {
		t.Fatal("stale-hold store must be a non-nil singleton")
	}
}

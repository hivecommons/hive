package prfollowup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/turn"
)

type fakeBranchUpdater struct {
	updateErr error
	updates   []string
	comments  []string
}

func (f *fakeBranchUpdater) UpdateBranch(ctx context.Context, repo string, number int) error {
	f.updates = append(f.updates, repo)
	return f.updateErr
}

func (f *fakeBranchUpdater) UpdateBranchExpectedHead(ctx context.Context, repo string, number int, expectedHeadSHA string, observedBaseSHA string) error {
	f.updates = append(f.updates, repo+"@"+expectedHeadSHA+":"+observedBaseSHA)
	return f.updateErr
}

func (f *fakeBranchUpdater) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}

func TestRouteBaseMovedPrecedesReviewAndDedupesByBase(t *testing.T) {
	for _, state := range []string{"dirty", "behind"} {
		for _, fork := range []bool{false, true} {
			t.Run(state+map[bool]string{false: "/branch", true: "/fork"}[fork], func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				now := time.Now()
				record(t, dir, "s1", now)
				r := newFakeResumer("s1")
				pr := redPR()
				pr.FromFork = fork
				pr.BaseSHA = "base-one"
				pr.BaseRef = "v5"
				pr.MergeableState = state
				options := opts(dir)
				options.Threads = map[string][]github.ReviewThread{ThreadsKey(testRepo, testPR): {{ThreadID: "review-one", Body: "fix this"}}}
				out := Route(ctx, []github.PullRequest{pr}, r, options, now)
				if len(out) != 1 || out[0].Route != RouteResumed || len(out[0].Events) != 1 || out[0].Events[0].Kind != EventBaseMoved {
					t.Fatalf("base repair not routed first: %+v", out)
				}
				for _, want := range []string{"hive-base-moved", "base-one", "Before addressing review threads or pushing any new work"} {
					if !strings.Contains(r.sent[0], want) {
						t.Errorf("message missing %q: %s", want, r.sent[0])
					}
				}
				pr.HeadSHA = "another-stale-head"
				out = Route(ctx, []github.PullRequest{pr}, r, options, now)
				if state == "dirty" {
					if len(out) != 1 || out[0].Events[0].Kind != EventStaleHeadPush {
						t.Fatalf("dirty stale head was not routed: %+v", out)
					}
				} else if len(out) != 0 {
					t.Fatalf("head push repeated base event: %+v", out)
				}
				pr.BaseSHA = "base-two"
				if out := Route(ctx, []github.PullRequest{pr}, r, options, now); len(out) != 1 {
					t.Fatalf("new base did not wake owner: %+v", out)
				}
				if !fork {
					pr.MergeableState = "clean"
					pr.CIStatus = "success"
					out = Route(ctx, []github.PullRequest{pr}, r, options, now)
					if len(out) != 1 || out[0].Events[0].Kind != EventReviewThread {
						t.Fatalf("review lost after repair: %+v", out)
					}
				}
			})
		}
	}
}

func TestRouteBaseMovedOwnershipAndFallback(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	pr := redPR()
	pr.FromFork = true
	pr.BaseSHA = "base-one"
	pr.MergeableState = "dirty"
	r := newFakeResumer("s1")
	if out := Route(ctx, []github.PullRequest{pr}, r, opts(dir), now); len(out) != 0 || len(r.sent) != 0 {
		t.Fatal("foreign fork without pointer was routed")
	}
	record(t, dir, "s1", now)
	out := Route(ctx, []github.PullRequest{pr}, nil, opts(dir), now)
	if len(out) != 1 || out[0].Route != RouteFallback || out[0].Queued != 1 {
		t.Fatalf("base repair lost when authoring session gone: %+v", out)
	}
	if got := HandoffSection(ctx, dir, testAgent); !strings.Contains(got, "hive-base-moved") || !strings.Contains(got, "base-one") {
		t.Fatalf("fresh kick missing base repair: %s", got)
	}
}

func TestBaseRepairDoesNotSupersedeBusyReview(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	record(t, dir, "s1", now)
	r := newFakeResumer("s1")
	r.sendErr = ErrBusy
	pr := redPR()
	pr.CIStatus = "success"
	options := opts(dir)
	options.Threads = map[string][]github.ReviewThread{ThreadsKey(testRepo, testPR): {{ThreadID: "review-one"}}}
	if out := Route(ctx, []github.PullRequest{pr}, r, options, now); len(out) != 1 || out[0].Route != RouteDeferred {
		t.Fatalf("review not deferred: %+v", out)
	}
	pr.BaseSHA, pr.MergeableState = "base-one", "dirty"
	r.sendErr = nil
	if out := Route(ctx, []github.PullRequest{pr}, r, options, now); len(out) != 1 || out[0].Events[0].Kind != EventBaseMoved {
		t.Fatalf("repair not prioritized: %+v", out)
	}
	pr.MergeableState = "clean"
	if out := Route(ctx, []github.PullRequest{pr}, r, options, now); len(out) != 1 || out[0].Events[0].Kind != EventReviewThread {
		t.Fatalf("queued review superseded by repair: %+v", out)
	}
}

func TestBaseMovedRequiresKnownBaseAndDoesNotTreatUnknownAsConflict(t *testing.T) {
	for _, pr := range []github.PullRequest{
		{MergeableState: "dirty"},
		{BaseSHA: "base", MergeableState: "unknown"},
		{BaseSHA: "base", MergeableState: "blocked"},
		{BaseSHA: "base", MergeableState: "clean"},
	} {
		if events := detectEvents(&turn.SessionEnvelope{Variables: map[string]string{}}, &pr, nil, nil, time.Now()); len(events) != 0 {
			t.Fatalf("unexpected repair: %+v", events)
		}
	}
}

func TestContributorPRBaseSyncGate(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	for _, tc := range []struct {
		name       string
		gate       bool
		state      string
		canModify  bool
		updateErr  error
		wantUpdate int
		wantRoute  string
		wantEvent  EventKind
	}{
		{name: "gate off", state: "behind", canModify: true, wantUpdate: 0, wantRoute: RouteResumed, wantEvent: EventBaseMoved},
		{name: "behind updates once", gate: true, state: "behind", canModify: true, wantUpdate: 1, wantRoute: RouteUpdated},
		{name: "dirty conflict falls back", gate: true, state: "dirty", canModify: true, updateErr: errors.New("422 Validation Failed"), wantUpdate: 1, wantRoute: RouteResumed, wantEvent: EventBaseMoved},
		{name: "maintainer cannot modify notes only", gate: true, state: "behind", canModify: false, wantUpdate: 0, wantRoute: RouteResumed, wantEvent: EventBaseMoved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			record(t, dir, "s1", now)
			r := newFakeResumer("s1")
			updater := &fakeBranchUpdater{updateErr: tc.updateErr}
			pr := redPR()
			pr.FromFork = true
			pr.MaintainerCanModify = tc.canModify
			pr.BaseSHA = "base-one"
			pr.BaseRef = "v5"
			pr.MergeableState = tc.state
			o := opts(dir)
			o.BaseSyncEnabled = func(repo string) bool { return tc.gate }
			o.BranchUpdater = updater
			out := Route(ctx, []github.PullRequest{pr}, r, o, now)
			if len(updater.updates) != tc.wantUpdate {
				t.Fatalf("UpdateBranch calls = %d, want %d (out=%+v)", len(updater.updates), tc.wantUpdate, out)
			}
			if tc.wantUpdate > 0 && !strings.HasSuffix(updater.updates[0], "@"+testSHA+":base-one") {
				t.Fatalf("UpdateBranch did not pin expected head SHA: %+v", updater.updates)
			}
			if len(out) != 1 || out[0].Route != tc.wantRoute {
				t.Fatalf("out = %+v, want route %s", out, tc.wantRoute)
			}
			if tc.wantEvent != "" && (len(out[0].Events) != 1 || out[0].Events[0].Kind != tc.wantEvent) {
				t.Fatalf("events = %+v, want %s", out[0].Events, tc.wantEvent)
			}
			if tc.wantRoute == RouteUpdated {
				if again := Route(ctx, []github.PullRequest{pr}, r, o, now.Add(time.Minute)); len(again) != 0 || len(updater.updates) != 1 {
					t.Fatalf("base sync was not deduped by base SHA: again=%+v updates=%d", again, len(updater.updates))
				}
				pr.BaseSHA = "base-two"
				if out := Route(ctx, []github.PullRequest{pr}, r, o, now.Add(2*time.Minute)); len(out) != 1 || len(updater.updates) != 2 {
					t.Fatalf("new base did not trigger a new update: out=%+v updates=%d", out, len(updater.updates))
				}
			}
		})
	}
}

func TestStaleHeadPushPostsSingleNoteAndRekicks(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	record(t, dir, "s1", now)
	r := newFakeResumer("s1")
	updater := &fakeBranchUpdater{}
	pr := redPR()
	pr.FromFork = true
	pr.BaseSHA = "base-one"
	pr.BaseRef = "v5"
	pr.MergeableState = "dirty"
	pr.HeadSHA = "head-one"
	o := opts(dir)
	o.BranchUpdater = updater
	if out := Route(ctx, []github.PullRequest{pr}, r, o, now); len(out) != 1 || out[0].Events[0].Kind != EventBaseMoved {
		t.Fatalf("first dirty route = %+v", out)
	}
	pr.HeadSHA = "head-two"
	out := Route(ctx, []github.PullRequest{pr}, r, o, now.Add(time.Minute))
	if len(out) != 1 || len(out[0].Events) != 1 || out[0].Events[0].Kind != EventStaleHeadPush {
		t.Fatalf("stale head route = %+v", out)
	}
	if len(updater.comments) != 1 || !strings.Contains(updater.comments[0], "hive-stale-head-push") {
		t.Fatalf("stale note comments = %+v", updater.comments)
	}
	if again := Route(ctx, []github.PullRequest{pr}, r, o, now.Add(2*time.Minute)); len(again) != 0 || len(updater.comments) != 1 {
		t.Fatalf("stale head note/kick repeated: again=%+v comments=%d", again, len(updater.comments))
	}
}

func TestUpdateBranchExpectedHeadMismatchWaitsForFreshSnapshot(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	dir := t.TempDir()
	record(t, dir, "s1", now)
	r := newFakeResumer("s1")
	updater := &fakeBranchUpdater{updateErr: errors.New("422 Validation Failed: expected_head_sha does not match")}
	pr := redPR()
	pr.FromFork = true
	pr.MaintainerCanModify = true
	pr.BaseSHA = "base-one"
	pr.BaseRef = "v5"
	pr.MergeableState = "behind"
	o := opts(dir)
	o.BaseSyncEnabled = func(repo string) bool { return true }
	o.BranchUpdater = updater
	if out := Route(ctx, []github.PullRequest{pr}, r, o, now); len(out) != 0 {
		t.Fatalf("stale expected_head_sha should wait for next snapshot, got %+v", out)
	}
	if len(updater.updates) != 1 {
		t.Fatalf("UpdateBranch calls = %d, want 1", len(updater.updates))
	}
	if again := Route(ctx, []github.PullRequest{pr}, r, o, now.Add(time.Minute)); len(again) != 0 || len(updater.updates) != 1 {
		t.Fatalf("stale snapshot was retried/routed: again=%+v updates=%d", again, len(updater.updates))
	}
	pr.HeadSHA = "fresh-head"
	updater.updateErr = nil
	if out := Route(ctx, []github.PullRequest{pr}, r, o, now.Add(2*time.Minute)); len(out) != 1 || out[0].Route != RouteUpdated || len(updater.updates) != 2 {
		t.Fatalf("fresh head did not retry update: out=%+v updates=%+v", out, updater.updates)
	}
}

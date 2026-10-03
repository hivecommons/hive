package prfollowup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

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
				if out := Route(ctx, []github.PullRequest{pr}, r, options, now); len(out) != 0 {
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
		if events := detectEvents(&pr, nil, nil, time.Now()); len(events) != 0 {
			t.Fatalf("unexpected repair: %+v", events)
		}
	}
}

package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

func TestPRWarnOnce(t *testing.T) {
	var w prWarnOnce
	k := prWarnKey("o/r", 7)
	if w.level(k) != slog.LevelWarn {
		t.Fatal("first sighting should warn")
	}
	if w.level(k) != slog.LevelDebug {
		t.Fatal("repeat should be debug")
	}
	w.retain(map[string]struct{}{k: {}})
	if w.level(k) != slog.LevelDebug {
		t.Fatal("still-active key should stay debug")
	}
	w.retain(map[string]struct{}{})
	if w.level(k) != slog.LevelWarn {
		t.Fatal("cleared condition should re-arm")
	}
}

func promotedWarnCount(log string, pr int) (warns, debugs int) {
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "review dispatch promoted") || !strings.Contains(line, fmt.Sprintf("pr=%d ", pr)) {
			continue
		}
		switch {
		case strings.Contains(line, "level=WARN"):
			warns++
		case strings.Contains(line, "level=DEBUG"):
			debugs++
		}
	}
	return warns, debugs
}

func TestPlanReviewDispatch_PromotionWarnsOncePerPR(t *testing.T) {
	redirectReviewPaths(t)
	reviewPromotedWarn.retain(map[string]struct{}{})
	t.Cleanup(func() { reviewPromotedWarn.retain(map[string]struct{}{}) })

	const otherPR = 4322
	// fillerPR has no wait record: promotion only reorders a list of two or
	// more heads, so it keeps a pass with a single starved head promotable.
	const fillerPR = 4399
	pass := func(numbers ...int) string {
		t.Helper()
		var waits []review.ReviewWaitRecord
		actionable := &github.ActionableResult{}
		for _, n := range numbers {
			waits = append(waits, review.ReviewWaitRecord{Repo: "hivecommons/hive", Number: n, HeadSHA: reviewTestSHA, Reason: review.ReviewWaitNoFreeReviewer, Cycles: review.ReviewWaitStarvationCycles, FirstAt: time.Now().Add(-time.Hour)})
			actionable.PRs.Items = append(actionable.PRs.Items, github.PullRequest{Repo: "hivecommons/hive", Number: n, Title: "test: add coverage", Author: "hive-bot", HeadSHA: reviewTestSHA})
		}
		actionable.PRs.Items = append(actionable.PRs.Items, github.PullRequest{Repo: "hivecommons/hive", Number: fillerPR, Title: "test: add coverage", Author: "hive-bot", HeadSHA: reviewTestSHA})
		if err := review.WriteDispatchState("", review.DispatchState{Waits: waits}); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		plan := planReviewDispatchWith(context.Background(), reviewSwarmConfig(), actionable, nil, nil, nil, logger)
		if len(plan.Promoted) != len(numbers) {
			t.Fatalf("promoted = %+v, want %d heads", plan.Promoted, len(numbers))
		}
		return buf.String()
	}
	expect := func(log string, pr, warns, debugs int) {
		t.Helper()
		if w, d := promotedWarnCount(log, pr); w != warns || d != debugs {
			t.Fatalf("PR %d promotion logs: warn=%d debug=%d, want warn=%d debug=%d\n%s", pr, w, d, warns, debugs, log)
		}
	}

	first := pass(4321, otherPR)
	expect(first, 4321, 1, 0)
	expect(first, otherPR, 1, 0)

	second := pass(4321, otherPR)
	expect(second, 4321, 0, 1)
	expect(second, otherPR, 0, 1)

	third := pass(4321)
	expect(third, 4321, 0, 1)

	fourth := pass(4321, otherPR)
	expect(fourth, 4321, 0, 1)
	expect(fourth, otherPR, 1, 0)
}

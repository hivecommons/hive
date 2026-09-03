package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

func emptyDispatchPlan(p review.DispatchPlan) bool {
	return len(p.ReviewKicks) == 0 && len(p.FixKicks) == 0 && p.State.GeneratedAt.IsZero()
}

// planReviewDispatch is the review-swarm entry gate: unless BOTH
// review.require_approval and review.fan_out are enabled, it must plan
// nothing at all — a kick escaping this gate would dispatch reviewer agents
// on a hive whose operator never opted into the review swarm.
func TestPlanReviewDispatchRequiresBothToggles(t *testing.T) {
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "hive", Number: 1, Title: "feat: thing", Author: "hive-bee", HeadSHA: "aaa"},
	}}}
	cases := []struct {
		name            string
		requireApproval bool
		fanOut          bool
	}{
		{"neither toggle", false, false},
		{"fan_out without require_approval", false, true},
		{"require_approval without fan_out", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Review: config.ReviewConfig{RequireApproval: tc.requireApproval, FanOut: tc.fanOut},
			}
			plan := planReviewDispatch(cfg, actionable, nil, restoreTestLogger())
			if !emptyDispatchPlan(plan) {
				t.Errorf("plan = %+v, want empty (gate must hold with require_approval=%v fan_out=%v)",
					plan, tc.requireApproval, tc.fanOut)
			}
		})
	}
}

// A nil config or nil enumeration must yield an empty plan, never a panic —
// the eval cycle calls this before the first successful GitHub pass.
func TestPlanReviewDispatchNilInputs(t *testing.T) {
	if plan := planReviewDispatch(nil, &github.ActionableResult{}, nil, restoreTestLogger()); !emptyDispatchPlan(plan) {
		t.Errorf("nil config produced a non-empty plan: %+v", plan)
	}
	cfg := &config.Config{Review: config.ReviewConfig{RequireApproval: true, FanOut: true}}
	if plan := planReviewDispatch(cfg, nil, nil, restoreTestLogger()); !emptyDispatchPlan(plan) {
		t.Errorf("nil actionable produced a non-empty plan: %+v", plan)
	}
}

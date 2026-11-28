package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

// auto_merge.promotions reaches the merge lanes and the sentinel only where
// the promotion lane can merge (the self-authored sweep runs), and only for
// one of the hive's own repositories. Elsewhere nothing is exempted, so a
// person who merges by hand sees every sentinel finding.
func TestPromotionRulesFuncGatesOnTheSelfAuthoredSweep(t *testing.T) {
	level := func(n int) *int { return &n }
	rule := config.PromotionRule{Source: "main", Target: "stable", Authors: []string{"github-actions[bot]"}, HeadRefPrefix: "auto/promote-"}
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"widget"}
	cfg.AutoMerge.Promotions = map[string][]config.PromotionRule{"acme/widget": {rule}, "acme/removed": {rule}}

	rules := promotionRulesFunc(cfg)
	cfg.ACMMLevel = level(config.SelfMergeMinACMMLevel - 1)
	if got := rules("acme/widget"); got != nil {
		t.Fatalf("below the self-merge level: %+v, want none", got)
	}
	cfg.ACMMLevel = level(config.SelfMergeMinACMMLevel)
	if got := rules("acme/widget"); len(got) != 1 {
		t.Fatalf("at the self-merge level: %+v, want the rule", got)
	}
	if got := rules("acme/removed"); got != nil {
		t.Fatalf("a repository outside project.repos: %+v, want none", got)
	}
	off := false
	cfg.AutoMerge.SelfAuthored = &off
	if got := rules("acme/widget"); got != nil {
		t.Fatalf("self_authored off: %+v, want none", got)
	}
}

// A relay refusal that names the promotion lane is not a hand merge: the
// hive merges that pull request itself.
func TestRelayRefusalForPromotionLaneIsNotAHandMerge(t *testing.T) {
	refusal := automerge.RelayMergeRefusalPrefix + " for the PR: promotion-lane (a branch-promotion pull request whose head tree is the protected source branch's; the self-authored sweep's promotion lane merges it itself)"
	if v := relayRefusalVerdict(refusal, false); v.HandMerge {
		t.Fatalf("verdict = %+v, want no hand merge", v)
	}
	if relayRefusalNeedsMaintainer(refusal) {
		t.Fatal("a promotion-lane refusal reads as needing a maintainer")
	}
}

// The hive never writes to a branch a promotion rule names, matched or
// not: the hold guard's keep-mergeable update and the stale-red base
// refresh skip it.
func TestPromotionBranchPRIsNeverUpdated(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"widget"}
	cfg.AutoMerge.Promotions = map[string][]config.PromotionRule{"acme/widget": {{Source: "main", Target: "stable", Authors: []string{"github-actions[bot]"}, HeadRefPrefix: "auto/promote-"}}}
	promote := github.PullRequest{Repo: "widget", Number: 1, Author: "github-actions[bot]", HeadRef: "auto/promote-main-to-stable", BaseRef: "stable"}
	other := github.PullRequest{Repo: "widget", Number: 2, Author: "github-actions[bot]", HeadRef: "fix/thing", BaseRef: "stable"}
	if !promotionBranchPR(cfg, promote) || promotionBranchPR(cfg, other) {
		t.Fatalf("promotionBranchPR: promote=%v other=%v, want true and false", promotionBranchPR(cfg, promote), promotionBranchPR(cfg, other))
	}
	if got := withoutPromotionBranches(cfg, []github.PullRequest{promote, other}); len(got) != 1 || got[0].Number != 2 {
		t.Fatalf("withoutPromotionBranches = %+v, want only #2", got)
	}
}

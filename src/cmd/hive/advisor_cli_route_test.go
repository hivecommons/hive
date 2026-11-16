package main

import (
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/advisor"
	"github.com/hivecommons/hive/pkg/config"
)

// The Claude CLI gets --effort only for a value it accepts; anything else,
// or no effort, keeps the CLI's default.
func TestReviewerCLIArgsEffort(t *testing.T) {
	if args := reviewerCLIArgs("", "high", "/tmp/s"); !containsArgSeq(args, []string{"--effort", "high"}) {
		t.Fatalf("high: %v", args)
	}
	for _, effort := range []string{"", "extreme"} {
		if args := reviewerCLIArgs("", effort, "/tmp/s"); slices.Contains(args, "--effort") {
			t.Fatalf("effort %q must not be passed: %v", effort, args)
		}
	}
}

// The advisor runtime takes the route its backend resolved to: the endpoint
// and key on http, neither on claude-cli, and a skip with the route's reason
// when no path is ready.
func TestAdvisorRuntimeFor(t *testing.T) {
	rt := config.AdvisorRuntime{Model: "claude-haiku-5-5", ReasoningEffort: "high", TimeoutS: 30, Instructions: "be brief"}

	got := advisorRuntimeFor(rt, config.ReviewerTurnRoute{Path: config.ReviewerPathHTTP, Ready: true, Endpoint: "https://llm", APIKey: "k", Model: "claude-haiku-5-5"})
	if got.Route != advisor.RouteHTTP || got.Endpoint != "https://llm" || got.APIKey != "k" || got.ReasoningEffort != "high" || got.Instructions != "be brief" {
		t.Fatalf("http = %+v", got)
	}

	got = advisorRuntimeFor(rt, config.ReviewerTurnRoute{Path: config.ReviewerPathClaudeCLI, Ready: true, Model: "claude-haiku-5-5"})
	if got.Route != advisor.RouteClaudeCLI || got.Endpoint != "" || got.APIKey != "" || got.Model != "claude-haiku-5-5" {
		t.Fatalf("claude-cli = %+v", got)
	}

	got = advisorRuntimeFor(rt, config.ReviewerTurnRoute{Path: config.ReviewerPathClaudeCLI, Reason: "Claude is not signed in"})
	if got.Route != advisor.RouteUnready || got.UnreadyReason != "Claude is not signed in" {
		t.Fatalf("not signed in = %+v", got)
	}
}

// Advisor reviews take their own slots, so they cannot hold the slots Let
// hive decide and grounded chat use.
func TestAdvisorCLIRunnerHasItsOwnSlots(t *testing.T) {
	if r := newAdvisorCLIRunner(); r.slots == reviewerCLISlots || cap(r.slots) != advisorCLIMaxConcurrent {
		t.Fatal("the advisor CLI runner shares the reviewer turns' slots")
	}
}

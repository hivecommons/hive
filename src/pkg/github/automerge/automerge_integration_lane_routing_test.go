package automerge

import (
	"context"
	"errors"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func integrationLaneFixture(t *testing.T, lane, strategy string, redCI bool) (*Engine, *laneGate, func(context.Context) (AutoMergeSweepEvent, string, error), func() int) {
	t.Helper()
	gate := &laneGate{answer: func(ctx context.Context, req hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
		if req.Authorize == nil {
			t.Fatal("serialized execution lost its lane authorization")
		}
		if err := req.Authorize(ctx, "sha7"); err != nil {
			t.Fatalf("eligible exact-head authorization: %v", err)
		}
		if err := req.Authorize(ctx, "changed-head"); err == nil {
			t.Fatal("serialized authorization accepted a different head")
		}
		return laneAnswer(hgithub.LaneOutcomeMerged, "merged")(ctx, req)
	}}
	opts := Options{
		BasePreparation:        NewBasePreparationStore(""),
		HeadFirstSeen:          NewHeadFirstSeenTracker(),
		StageStore:             stagelabels.NewStore("", nil),
		IntegrationSelfAllowed: func() bool { return true },
	}
	if lane == stageLaneContributor {
		fixture := validContributorFixture()
		fixture.baseRef = "main"
		fixture.nodeID = "PR_7"
		if redCI {
			fixture.headStatus = "failure"
		}
		api := newContributorLaneAPI(t, &fixture)
		t.Cleanup(api.Close)
		clock := &contributorLaneClock{at: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
		engine := newContributorLaneSweepEngine(api.URL, strategy, gate.gate, clock, opts)
		run := func(ctx context.Context) (AutoMergeSweepEvent, string, error) {
			return engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, defaultContributorLanePolicy())
		}
		return engine, gate, run, func() int { return fixture.merges }
	}
	pr := greenLanePR(7)
	pr.nodeID = "PR_7"
	label := hgithub.AutoMergeQueuedLabel
	if lane == stageLaneSelf {
		pr.author = testHiveAppBotLogin
		label = ""
	}
	if redCI {
		pr.statusState, pr.checkConclusion = "failure", "failure"
	}
	var merged []int
	api := newAutoMergeSweepAPI(t, label, []sweepPR{pr}, &merged)
	t.Cleanup(api.Close)
	engine := newLaneSweepEngine(api.URL, strategy, gate.gate, opts)
	run := func(ctx context.Context) (AutoMergeSweepEvent, string, error) {
		if lane == stageLaneSelf {
			return engine.trySweepSelfAuthoredPR(ctx, "widget", "acme", "widget", 7, false)
		}
		return engine.trySweepQueuedPR(ctx, "widget", "acme", "widget", 7, label)
	}
	return engine, gate, run, func() int { return len(merged) }
}

func TestIntegrationExecutionRoutesBeforeEveryMergeStrategy(t *testing.T) {
	scopes := []struct {
		name, authority, reason string
		fail                    bool
		ordinary                bool
	}{
		{name: "no-execution-scope", ordinary: true},
		{name: "observer-advisory", reason: "advisory only", ordinary: true},
		{name: "enforced-hold", authority: "hold", reason: "integration holds destination"},
		{name: "hive-authority", authority: "hive", reason: "integration owns destination"},
		{name: "unknown-authority", authority: "unknown"},
		{name: "native-unavailable", authority: "native"},
		{name: "route-error", fail: true},
	}
	for _, lane := range []string{stageLaneSelf, nativeQueueLaneQueued, stageLaneContributor} {
		for _, strategy := range []string{config.MergeStrategyHiveSerialized, config.MergeStrategyDirect} {
			for _, scope := range scopes {
				t.Run(lane+"/"+strategy+"/"+scope.name, func(t *testing.T) {
					engine, gate, run, merges := integrationLaneFixture(t, lane, strategy, false)
					calls := 0
					if scope.name != "no-execution-scope" {
						engine.integrationExecution = func(ctx context.Context, repo string, pr *gh.PullRequest, eligibility IntegrationEligibilityGate) (IntegrationExecutionDecision, error) {
							calls++
							if repo != "acme/widget" || pr.GetNumber() != 7 || pr.GetHead().GetSHA() != "sha7" || eligibility == nil {
								t.Fatal("routing lost exact member or applicable eligibility evaluator")
							}
							if scope.fail {
								return IntegrationExecutionDecision{}, errors.New("routing unavailable")
							}
							if scope.name == "observer-advisory" {
								member, reason, err := eligibility(ctx, repo, pr.GetBase().GetRef(), pr.GetNumber(), pr.GetHead().GetSHA())
								if err != nil || reason != "" || member.GetNumber() != pr.GetNumber() || member.GetHead().GetSHA() != pr.GetHead().GetSHA() {
									t.Fatalf("observer could not capture exact eligible member: member=%+v reason=%q err=%v", member, reason, err)
								}
								if len(gate.reqs) != 0 || merges() != 0 {
									t.Fatal("observer eligibility capture executed a merge")
								}
							}
							return IntegrationExecutionDecision{Authority: scope.authority, Reason: scope.reason}, nil
						}
					}
					event, reason, err := run(context.Background())
					wantCalls := 1
					if scope.name == "no-execution-scope" {
						wantCalls = 0
					}
					if calls != wantCalls {
						t.Fatalf("routing calls=%d, want %d (no duplicated routing)", calls, wantCalls)
					}
					if !scope.ordinary {
						if event.Number != 0 || (reason == "" && err == nil) || len(gate.reqs) != 0 || merges() != 0 {
							t.Fatalf("authority/error fell back to merge: event=%+v reason=%q err=%v lane=%+v REST=%d", event, reason, err, gate.reqs, merges())
						}
						return
					}
					if err != nil || reason != "" || event.Number != 7 || event.HeadSHA != "sha7" {
						t.Fatalf("ordinary behavior changed: event=%+v reason=%q err=%v", event, reason, err)
					}
					if strategy == config.MergeStrategyHiveSerialized {
						if len(gate.reqs) != 1 || merges() != 0 {
							t.Fatalf("ordinary serialized lane bypassed: requests=%+v REST=%d", gate.reqs, merges())
						}
					} else if len(gate.reqs) != 0 || merges() != 1 {
						t.Fatalf("ordinary direct behavior changed: requests=%+v REST=%d", gate.reqs, merges())
					}
				})
			}
		}
	}
}

func TestIntegrationNativeAuthorityCannotBypassSerializedAuthorization(t *testing.T) {
	for _, lane := range []string{stageLaneSelf, nativeQueueLaneQueued, stageLaneContributor} {
		t.Run(lane, func(t *testing.T) {
			engine, gate, run, merges := integrationLaneFixture(t, lane, config.MergeStrategyHiveSerialized, false)
			native := &nativeQueueFake{Transport: engine.transport, state: hgithub.NativeMergeQueueState{
				ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true,
			}}
			engine.transport = &laneNativeQueueTransport{nativeQueueFake: native, gate: gate.gate}
			enableNativeQueue(engine)
			engine.integrationExecution = func(context.Context, string, *gh.PullRequest, IntegrationEligibilityGate) (IntegrationExecutionDecision, error) {
				return IntegrationExecutionDecision{Authority: "native"}, nil
			}
			event, reason, err := run(context.Background())
			if err != nil || reason != "integration native authority cannot use serialized REST lane; no direct fallback" || event.Number != 0 {
				t.Fatalf("incompatible execution authority did not fail closed: event=%+v reason=%q err=%v", event, reason, err)
			}
			if len(gate.reqs) != 0 || merges() != 0 || native.enqueues != 0 {
				t.Fatalf("execution bypassed serialized authorization: lane=%+v REST=%d native=%d", gate.reqs, merges(), native.enqueues)
			}
		})
	}
}

func TestIntegrationEligibilityCapturesEveryLaneBeforeExecution(t *testing.T) {
	for _, lane := range []string{stageLaneSelf, nativeQueueLaneQueued, stageLaneContributor} {
		for _, strategy := range []string{config.MergeStrategyHiveSerialized, config.MergeStrategyDirect} {
			for _, redCI := range []bool{false, true} {
				name := lane + "/" + strategy + "/green"
				if redCI {
					name = lane + "/" + strategy + "/red"
				}
				t.Run(name, func(t *testing.T) {
					engine, gate, _, merges := integrationLaneFixture(t, lane, strategy, redCI)
					pr, reason, err := engine.IntegrationEligibility(context.Background(), "acme/widget", "main", 7, "sha7")
					if redCI {
						if pr != nil || (reason == "" && err == nil) {
							t.Fatal("eligibility capture bypassed the lane's CI gates")
						}
					} else if err != nil || reason != "" || pr.GetNumber() != 7 || pr.GetHead().GetSHA() != "sha7" || pr.GetBase().GetRef() != "main" {
						t.Fatalf("exact member not captured: pr=%+v reason=%q err=%v", pr, reason, err)
					}
					if len(gate.reqs) != 0 || merges() != 0 {
						t.Fatalf("read-only capture executed a merge: lane=%+v REST=%d", gate.reqs, merges())
					}
					if moved, _, _ := engine.IntegrationEligibility(context.Background(), "acme/widget", "main", 7, "changed-head"); moved != nil {
						t.Fatal("eligibility accepted a head outside the requested member")
					}
				})
			}
		}
	}
}

func TestIntegrationCaptureRejectsChangedExactMember(t *testing.T) {
	for _, field := range []string{"number", "head", "base", "repo", "nil-capture"} {
		t.Run(field, func(t *testing.T) {
			pr := &gh.PullRequest{Number: gh.Ptr(7), Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha7")},
				Base: &gh.PullRequestBranch{Ref: gh.Ptr("main"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}}}
			capture := &integrationEligibilityCapture{number: 7, head: "sha7", base: "main", repo: "acme/widget"}
			switch field {
			case "number":
				capture.number = 8
			case "head":
				capture.head = "changed-head"
			case "base":
				capture.base = "v6"
			case "repo":
				capture.repo = "other/widget"
			case "nil-capture":
				capture = nil
			}
			engine := &Engine{integrationExecution: func(context.Context, string, *gh.PullRequest, IntegrationEligibilityGate) (IntegrationExecutionDecision, error) {
				t.Fatal("read-only capture consulted execution routing")
				return IntegrationExecutionDecision{}, nil
			}}
			ctx := context.WithValue(context.Background(), integrationEligibilityContextKey{}, capture)
			native, reason, err := engine.routeIntegrationMerge(ctx, pr, effects.Claim{Repo: "acme/widget"})
			if native || err != nil || reason == "" || capture != nil && capture.pr != nil {
				t.Fatalf("changed member captured or routed: native=%v reason=%q err=%v capture=%+v", native, reason, err, capture)
			}
		})
	}
}

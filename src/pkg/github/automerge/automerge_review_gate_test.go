package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/evidence"
)

type reviewGateRecords struct {
	shadow []evidence.ShadowGate
	evals  []ReviewGateEvaluation
}

func newReviewGateSweepClient(apiURL string, gate ReviewGate, rec *reviewGateRecords) *Engine {
	c := newAutoMergeSweepClient(apiURL)
	gate.RecordShadow = func(fullRepo string, number int, head string, g evidence.ShadowGate) {
		rec.shadow = append(rec.shadow, g)
	}
	gate.RecordEvaluation = func(e ReviewGateEvaluation) { rec.evals = append(rec.evals, e) }
	c.SetReviewGate(&gate)
	return c
}

var (
	reviewGateNoBundle  = ReviewGateVerdict{Loaded: true}
	reviewGateAwaiting  = ReviewGateVerdict{Loaded: true, Found: true}
	reviewGateChanges   = ReviewGateVerdict{Loaded: true, Found: true, ChangesRequested: []string{"security", "correctness"}}
	reviewGateApproved  = ReviewGateVerdict{Loaded: true, Found: true, Approved: true}
	reviewGateArtifactX = ReviewGateVerdict{}
)

// The four outcomes x {enforce, shadow, off} (#12592): enforce holds every
// head that is not approved; shadow and off always merge, shadow records the
// would-have-held observation and off records nothing.
func TestSelfAuthoredSweepReviewGateOutcomesByMode(t *testing.T) {
	outcomes := []struct {
		name        string
		verdict     ReviewGateVerdict
		wantOutcome string
	}{
		{name: "no bundle", verdict: reviewGateNoBundle, wantOutcome: ReviewGateOutcomeNoBundle},
		{name: "artifact unreadable", verdict: reviewGateArtifactX, wantOutcome: ReviewGateOutcomeNoBundle},
		{name: "awaiting approval", verdict: reviewGateAwaiting, wantOutcome: ReviewGateOutcomeAwaiting},
		{name: "changes requested", verdict: reviewGateChanges, wantOutcome: "changes-requested:correctness,security"},
		{name: "approved", verdict: reviewGateApproved, wantOutcome: ReviewGateOutcomeApproved},
	}
	modes := []struct {
		mode        string
		wantEnforce bool
		wantRecord  bool
	}{
		{mode: ReviewGateModeEnforce, wantEnforce: true, wantRecord: true},
		{mode: ReviewGateModeShadow, wantRecord: true},
		{mode: "", wantRecord: true},
		{mode: ReviewGateModeOff},
	}
	for _, m := range modes {
		for _, o := range outcomes {
			t.Run(m.mode+"/"+o.name, func(t *testing.T) {
				merges := 0
				fx := selfSweepFixture{mergeApplied: true, mergeCalls: &merges}
				api := newSelfSweepGuardAPI(t, fx)
				defer api.Close()
				var rec reviewGateRecords
				mode := m.mode
				verdict := o.verdict
				var asked []string
				c := newReviewGateSweepClient(api.URL, ReviewGate{
					Mode: func() string { return mode },
					Verdict: func(repo string, number int, head string) ReviewGateVerdict {
						asked = append(asked, repo+"@"+head)
						return verdict
					},
				}, &rec)
				_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
				if err != nil {
					t.Fatalf("trySweepSelfAuthoredPR error: %v", err)
				}
				held := m.wantEnforce && o.wantOutcome != ReviewGateOutcomeApproved
				if held {
					if reason != autoMergeReasonReviewGatePrefix+o.wantOutcome || merges != 0 {
						t.Fatalf("reason = %q, merges = %d; want held with %q", reason, merges, autoMergeReasonReviewGatePrefix+o.wantOutcome)
					}
				} else if reason != "" || merges != 1 {
					t.Fatalf("reason = %q, merges = %d; want merge", reason, merges)
				}
				if !m.wantRecord {
					if len(asked) != 0 || len(rec.evals) != 0 || len(rec.shadow) != 0 {
						t.Fatalf("off mode evaluated: asked=%v evals=%+v shadow=%+v", asked, rec.evals, rec.shadow)
					}
					return
				}
				if len(asked) != 1 || asked[0] != "acme/widget@sha7" {
					t.Fatalf("verdict asked for %v, want the re-verified head acme/widget@sha7", asked)
				}
				if len(rec.evals) != 1 {
					t.Fatalf("evaluations = %+v, want one", rec.evals)
				}
				e := rec.evals[0]
				wouldHold := o.wantOutcome != ReviewGateOutcomeApproved
				if e.Lane != "self-merge" || e.Outcome != o.wantOutcome || e.WouldHold != wouldHold || e.Enforced != m.wantEnforce || e.HeadSHA != "sha7" || e.Number != 7 {
					t.Fatalf("evaluation = %+v", e)
				}
				if m.wantEnforce {
					if len(rec.shadow) != 0 {
						t.Fatalf("enforce recorded a shadow observation: %+v", rec.shadow)
					}
					return
				}
				if len(rec.shadow) != 1 {
					t.Fatalf("shadow = %+v, want one observation", rec.shadow)
				}
				g := rec.shadow[0]
				if g.WouldHaveHeld == nil || *g.WouldHaveHeld != wouldHold || g.Reason != o.wantOutcome || g.Lane != "self-merge" || g.WouldHaveBlocked != nil || g.Confidence != 1 || g.EvaluatedAt.IsZero() {
					t.Fatalf("shadow observation = %+v", g)
				}
			})
		}
	}
}

// review.require_approval and review.shadow are followed on every evaluation
// and are never weakened by governor.review_gate.mode.
func TestReviewGateEffectiveMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		require bool
		shadow  bool
		want    string
	}{
		{name: "default is shadow", want: ReviewGateModeShadow},
		{name: "unknown mode is shadow", mode: "bogus", want: ReviewGateModeShadow},
		{name: "off", mode: ReviewGateModeOff, want: ReviewGateModeOff},
		{name: "off + review.shadow records", mode: ReviewGateModeOff, shadow: true, want: ReviewGateModeShadow},
		{name: "off + require_approval enforces", mode: ReviewGateModeOff, require: true, want: ReviewGateModeEnforce},
		{name: "shadow + require_approval enforces", mode: ReviewGateModeShadow, require: true, want: ReviewGateModeEnforce},
		{name: "require_approval + review.shadow still enforces", mode: ReviewGateModeShadow, require: true, shadow: true, want: ReviewGateModeEnforce},
		{name: "enforce + review.shadow still enforces", mode: ReviewGateModeEnforce, shadow: true, want: ReviewGateModeEnforce},
		{name: "upper-case enforce", mode: " ENFORCE ", want: ReviewGateModeEnforce},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &ReviewGate{
				Mode:            func() string { return tt.mode },
				RequireApproval: func() bool { return tt.require },
				Shadow:          func() bool { return tt.shadow },
			}
			if got := g.effectiveMode(); got != tt.want {
				t.Fatalf("effectiveMode = %q, want %q", got, tt.want)
			}
		})
	}
	var nilGate *ReviewGate
	if got := nilGate.effectiveMode(); got != ReviewGateModeOff {
		t.Fatalf("nil gate mode = %q, want off", got)
	}
	if got := (&ReviewGate{}).effectiveMode(); got != ReviewGateModeShadow {
		t.Fatalf("zero gate mode = %q, want shadow", got)
	}
}

// A config reload of review.require_approval applies on the next evaluation
// without reinstalling the gate.
func TestSelfAuthoredSweepReviewGateReadsKnobsEveryEvaluation(t *testing.T) {
	merges := 0
	fx := selfSweepFixture{mergeApplied: true, mergeCalls: &merges}
	api := newSelfSweepGuardAPI(t, fx)
	defer api.Close()
	require := true
	var rec reviewGateRecords
	c := newReviewGateSweepClient(api.URL, ReviewGate{
		Mode:            func() string { return ReviewGateModeOff },
		RequireApproval: func() bool { return require },
		Verdict:         func(string, int, string) ReviewGateVerdict { return reviewGateAwaiting },
	}, &rec)
	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != autoMergeReasonReviewGatePrefix+ReviewGateOutcomeAwaiting || merges != 0 {
		t.Fatalf("require_approval: reason %q err %v merges %d; want held", reason, err, merges)
	}
	require = false
	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != "" || merges != 1 {
		t.Fatalf("off after reload: reason %q err %v merges %d; want merge", reason, err, merges)
	}
}

func TestSelfMergeReviewGateTrustedHumanApproval(t *testing.T) {
	tests := []struct {
		name         string
		verdict      ReviewGateVerdict
		reviews      []map[string]any
		wantReason   string
		wantOutcome  string
		wantApprover string
		wantCalls    int
	}{
		{
			name:    "human approval clears requires human",
			verdict: reviewGateAwaiting,
			reviews: []map[string]any{{
				"state": "APPROVED", "commit_id": "sha7", "user": map[string]string{"login": "bob"},
			}},
			wantOutcome:  ReviewGateOutcomeHumanApproved,
			wantApprover: "bob",
			wantCalls:    1,
		},
		{
			name:        "stale approval does not clear",
			verdict:     reviewGateAwaiting,
			reviews:     []map[string]any{{"state": "APPROVED", "commit_id": "oldsha", "user": map[string]string{"login": "bob"}}},
			wantReason:  autoMergeReasonReviewGatePrefix + ReviewGateOutcomeAwaiting,
			wantOutcome: ReviewGateOutcomeAwaiting,
			wantCalls:   1,
		},
		{
			name:        "untrusted reviewer does not clear",
			verdict:     reviewGateAwaiting,
			reviews:     []map[string]any{{"state": "APPROVED", "commit_id": "sha7", "user": map[string]string{"login": "mallory"}}},
			wantReason:  autoMergeReasonReviewGatePrefix + ReviewGateOutcomeAwaiting,
			wantOutcome: ReviewGateOutcomeAwaiting,
			wantCalls:   1,
		},
		{
			name:        "changes requested is not overridden",
			verdict:     reviewGateChanges,
			wantReason:  autoMergeReasonReviewGatePrefix + "changes-requested:correctness,security",
			wantOutcome: "changes-requested:correctness,security",
		},
		{
			name:        "swarm approval makes no review API call",
			verdict:     reviewGateApproved,
			wantOutcome: ReviewGateOutcomeApproved,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reviewCalls := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/7/reviews" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
				}
				reviewCalls++
				json.NewEncoder(w).Encode(tt.reviews)
			}))
			defer api.Close()

			var rec reviewGateRecords
			c := newReviewGateSweepClient(api.URL, ReviewGate{
				Mode:    func() string { return ReviewGateModeEnforce },
				Verdict: func(string, int, string) ReviewGateVerdict { return tt.verdict },
			}, &rec)
			reason := c.selfMergeReviewGate(context.Background(), "acme/widget", "acme", "widget", stageLaneSelf, 7, nil, "sha7")
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			if reviewCalls != tt.wantCalls {
				t.Fatalf("review API calls = %d, want %d", reviewCalls, tt.wantCalls)
			}
			if len(rec.evals) != 1 {
				t.Fatalf("evaluations = %+v, want one", rec.evals)
			}
			e := rec.evals[0]
			if e.Outcome != tt.wantOutcome || e.HumanApprover != tt.wantApprover || e.WouldHold != (tt.wantReason != "") {
				t.Fatalf("evaluation = %+v, want outcome %q approver %q", e, tt.wantOutcome, tt.wantApprover)
			}
		})
	}
}

// Only the App's own PRs are gated; a nil gate or a trusted-bot lane PR is
// never evaluated.
func TestSelfMergeReviewGateScope(t *testing.T) {
	tests := []struct {
		name string
		gate *ReviewGate
		lane string
	}{
		{name: "nil gate", lane: "self-authored"},
		{name: "trusted-bot lane", gate: &ReviewGate{Mode: func() string { return ReviewGateModeEnforce }}, lane: "trusted-bot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newAutoMergeSweepClient("http://127.0.0.1:0")
			asked := 0
			if tt.gate != nil {
				tt.gate.Verdict = func(string, int, string) ReviewGateVerdict { asked++; return reviewGateNoBundle }
				c.SetReviewGate(tt.gate)
			}
			if reason := c.selfMergeReviewGate(context.Background(), "acme/widget", "acme", "widget", tt.lane, 7, nil, "sha7"); reason != "" || asked != 0 {
				t.Fatalf("reason = %q, asked = %d; want no evaluation", reason, asked)
			}
		})
	}
	var nilEngine *Engine
	nilEngine.SetReviewGate(&ReviewGate{})
	if nilEngine.currentReviewGate() != nil {
		t.Fatal("nil engine must have no review gate")
	}
}

package automerge

// Tests for the self-authored sweep's intent tier gate (#6258). The
// invariant under test is that the MERGE DOES NOT HAPPEN — every refusal
// case asserts zero PUT .../merge calls against the fixture, not merely a
// skip reason — because before this gate the App squashed every green PR it
// authored regardless of tier.

import (
	"context"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/intent"
)

const (
	testIntentSourceFile    = "src/pkg/thing/thing.go"
	testIntentDocsFile      = "docs/guide.md"
	testIntentGuardrailFile = ".github/workflows/ci.yml"
	testIntentLinkedBody    = "Fixes #12"
)

func newIntentGateSweepClient(apiURL string, enforce bool) *Engine {
	c := newAutoMergeSweepClient(apiURL)
	c.SetIntentGate(&IntentGate{Enforce: func() bool { return enforce }})
	return c
}

func TestSelfAuthoredSweepIntentGateRefusesUnauthorizedTier(t *testing.T) {
	tests := []struct {
		name string
		fx   selfSweepFixture
	}{
		// Tier1 (bugfix/chore default) requires a linked issue. No body, no
		// issue, no merge — the exact PR the human lane would refuse.
		{name: "tier1 without linked issue", fx: selfSweepFixture{files: []string{testIntentSourceFile}}},
		// A body that mentions an issue without a closing keyword is not
		// linked-issue evidence.
		{name: "tier1 with unlinked issue mention", fx: selfSweepFixture{body: "see issue 12", files: []string{testIntentSourceFile}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merges := 0
			tt.fx.mergeCalls = &merges
			api := newSelfSweepGuardAPI(t, tt.fx)
			defer api.Close()
			c := newIntentGateSweepClient(api.URL, true)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if reason != autoMergeReasonIntentUnauthorized {
				t.Fatalf("reason = %q, want %q", reason, autoMergeReasonIntentUnauthorized)
			}
			if event.MergeSHA != "" {
				t.Fatalf("event = %+v, want no merge recorded", event)
			}
			if merges != 0 {
				t.Fatalf("merge endpoint called %d times, want 0: the intent tier gate must withhold the merge, not just log it", merges)
			}
		})
	}
}

func TestSelfAuthoredSweepIntentGateFailsClosedWithoutEvidence(t *testing.T) {
	tests := []struct {
		name string
		fx   selfSweepFixture
	}{
		{name: "files API error", fx: selfSweepFixture{body: testIntentLinkedBody, files: []string{testIntentSourceFile}, filesHTTPCode: http.StatusInternalServerError}},
		// GitHub says 3 files changed but the API returned 1: a partial list
		// could hide a guardrail path, so the tier is unknown.
		{name: "incomplete file list", fx: selfSweepFixture{body: testIntentLinkedBody, files: []string{testIntentSourceFile}, changedFiles: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merges := 0
			tt.fx.mergeCalls = &merges
			api := newSelfSweepGuardAPI(t, tt.fx)
			defer api.Close()
			c := newIntentGateSweepClient(api.URL, true)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
			if err == nil {
				t.Fatalf("err = nil, want evidence error")
			}
			if reason != autoMergeReasonIntentEvidence {
				t.Fatalf("reason = %q, want %q", reason, autoMergeReasonIntentEvidence)
			}
			if event.MergeSHA != "" || merges != 0 {
				t.Fatalf("event = %+v, merges = %d; want no merge when the tier cannot be established", event, merges)
			}
		})
	}
}

func TestSelfAuthoredSweepIntentGateAuthorizedTiersMerge(t *testing.T) {
	tests := []struct {
		name string
		fx   selfSweepFixture
	}{
		// Tier0: additive docs-only change needs no evidence at all.
		{name: "tier0 additive docs", fx: selfSweepFixture{files: []string{testIntentDocsFile}}},
		// Tier1 with a linked issue is authorized.
		{name: "tier1 with linked issue", fx: selfSweepFixture{body: testIntentLinkedBody, files: []string{testIntentSourceFile}}},
		// Tier3 guardrail path: EvaluateForAppSelfMerge drops ONLY the
		// human-approval requirement (Prow forbids the App approving its
		// own PR), so this is authorized by that function's documented
		// contract — pinned here so a future tightening is deliberate.
		{name: "tier3 guardrail under app self-merge contract", fx: selfSweepFixture{files: []string{testIntentGuardrailFile}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merges := 0
			tt.fx.mergeCalls = &merges
			tt.fx.mergeApplied = true
			api := newSelfSweepGuardAPI(t, tt.fx)
			defer api.Close()
			c := newIntentGateSweepClient(api.URL, true)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
			if err != nil || reason != "" {
				t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want clean merge", reason, err)
			}
			if event.MergeSHA != "merge7" || merges != 1 {
				t.Fatalf("event = %+v, merges = %d; want exactly one merge at merge7", event, merges)
			}
		})
	}
}

// With intent.enforce off the gate is advisory, exactly like
// writeMergeEligible's `if enforceIntent`: parity with the human lane means
// the self-authored sweep must not be STRICTER than it either.
func TestSelfAuthoredSweepIntentGateAdvisoryWhenNotEnforced(t *testing.T) {
	merges := 0
	fx := selfSweepFixture{files: []string{testIntentSourceFile}, mergeApplied: true, mergeCalls: &merges}
	api := newSelfSweepGuardAPI(t, fx)
	defer api.Close()
	c := newIntentGateSweepClient(api.URL, false)
	event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7)
	if err != nil || reason != "" {
		t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want advisory merge", reason, err)
	}
	if event.MergeSHA != "merge7" || merges != 1 {
		t.Fatalf("event = %+v, merges = %d; want one merge under advisory mode", event, merges)
	}
}

// The gate uses the SAME refusal predicate as the human merge lane
// (intent.Verdict.BlocksMerge) so the two lanes cannot drift apart.
func TestSelfAuthoredSweepIntentGateMatchesHumanLanePredicate(t *testing.T) {
	class := intent.Classify(intent.PR{Title: "fix widget", Files: []intent.ChangedFile{{Filename: testIntentSourceFile, Status: "modified", Additions: 1}}, AgentAuthor: true}, intent.Config{})
	if class.Tier != intent.Tier1 {
		t.Fatalf("classification = %+v, want Tier1", class)
	}
	verdict := intent.EvaluateForAppSelfMerge(class, intent.Evidence{}, true)
	if !verdict.BlocksMerge(true) {
		t.Fatalf("verdict = %+v, want BlocksMerge(true) for tier1 without a linked issue", verdict)
	}
	if verdict.BlocksMerge(false) {
		t.Fatalf("verdict = %+v, want advisory (no block) when enforcement is off", verdict)
	}
}

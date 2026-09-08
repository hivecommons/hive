package dashboard

// The review_rejected emitter (#6259): an owner denying a queued approval is
// the one production path that fires the transition, so these tests pin that
// a denial emits exactly once with the payload the hook contract documents,
// and that nothing else on the desk does.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/toolapprove"
)

const (
	reviewRejectedTestOperator  = "fleet-owner"
	reviewRejectedTestRationale = "hallucinated the API surface"
	reviewRejectedTestOrigin    = "https://hive.example.com"
	reviewRejectedTestACMMLevel = 4
	reviewRejectedTestPin       = "sonnet-20240229"
)

// hookCapture records every payload the dashboard fires through the HookFire
// seam, safe under -race.
type hookCapture struct {
	mu       sync.Mutex
	payloads []hooks.Payload
}

func (c *hookCapture) fire(_ context.Context, p hooks.Payload) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, p)
}

func (c *hookCapture) all() []hooks.Payload {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]hooks.Payload(nil), c.payloads...)
}

// reviewRejectedServer wires an approval desk whose HookFire is captured, a
// public dashboard origin, an ACMM level, and one queued item per agent name
// given, each targeting a distinct PR number.
func reviewRejectedServer(t *testing.T, agents ...string) (*Server, *toolapprove.Inbox, *hookCapture) {
	t.Helper()
	s, inbox := approvalServer(t, 0)
	capture := &hookCapture{}
	s.deps.HookFire = capture.fire
	s.deps.Config.Dashboard.PublicURL = reviewRejectedTestOrigin
	level := reviewRejectedTestACMMLevel
	s.deps.Config.ACMMLevel = &level

	for i, name := range agents {
		req := toolapprove.Request{
			Kind:   toolapprove.KindSelfMerge,
			Repo:   "hivecommons/hive",
			Number: 100 + i,
			Author: "hive-app[bot]",
			Agent:  toolapprove.AgentIdentity{Name: name},
			Tool:   toolapprove.ToolRequest{Tool: "hive-merge"},
		}
		v := toolapprove.Verdict{
			Decision:  toolapprove.DecisionOperatorApprove,
			ACMMLevel: reviewRejectedTestACMMLevel,
			Tool:      "hive-merge",
			Rule:      "needs-human",
		}
		if _, err := inbox.Enqueue(req, v); err != nil {
			t.Fatalf("seed enqueue: %v", err)
		}
	}
	return s, inbox, capture
}

// doOwnerPostAs posts as a verified owner with an explicit operator identity,
// so the emitted actor can be asserted rather than defaulting to "local".
func doOwnerPostBodyAs(s *Server, operator, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", operator)
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// TestApprovalDenyEmitsReviewRejectedOnce is the end-to-end contract: one
// denial, one review_rejected, carrying the agent, target, actor, reason, the
// producing model and pin, the ACMM level, and the model-knob deep link. A
// replayed denial is a 409 and emits nothing more.
func TestApprovalDenyEmitsReviewRejectedOnce(t *testing.T) {
	s, inbox, capture := reviewRejectedServer(t, "scanner")
	if err := s.deps.AgentMgr.PinModel("scanner", reviewRejectedTestPin); err != nil {
		t.Fatalf("PinModel: %v", err)
	}
	id := inbox.List()[0].ID

	rec := doOwnerPostBodyAs(s, reviewRejectedTestOperator, "/api/approvals/resolve",
		map[string]any{"id": id, "approved": false, "rationale": reviewRejectedTestRationale})
	if rec.Code != http.StatusOK {
		t.Fatalf("deny = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	got := capture.all()
	if len(got) != 1 {
		t.Fatalf("a denial must emit exactly one review_rejected, got %d: %+v", len(got), got)
	}
	p := got[0]
	if p.Transition != hooks.TransitionReviewRejected {
		t.Errorf("transition = %q, want %q", p.Transition, hooks.TransitionReviewRejected)
	}
	if p.Agent != "scanner" {
		t.Errorf("agent = %q, want the requesting agent", p.Agent)
	}
	if p.Repo != "hivecommons/hive" {
		t.Errorf("repo = %q", p.Repo)
	}
	if p.Actor != reviewRejectedTestOperator {
		t.Errorf("actor = %q, want the resolving operator %q", p.Actor, reviewRejectedTestOperator)
	}
	if p.Reason != reviewRejectedTestRationale {
		t.Errorf("reason = %q, want the operator's rationale", p.Reason)
	}
	if p.Backend != "claude" {
		t.Errorf("backend = %q, want the agent's configured backend", p.Backend)
	}
	if p.Model != reviewRejectedTestPin || p.Pin != reviewRejectedTestPin {
		t.Errorf("model/pin = %q/%q, want the pinned model %q on both", p.Model, p.Pin, reviewRejectedTestPin)
	}
	if p.ACMMLevel != reviewRejectedTestACMMLevel {
		t.Errorf("acmm_level = %d, want %d", p.ACMMLevel, reviewRejectedTestACMMLevel)
	}
	if p.Attrs[hooks.AttrPR] != "100" {
		t.Errorf("attrs.pr = %q, want the target PR", p.Attrs[hooks.AttrPR])
	}
	wantKnob := hooks.ModelKnobURL(reviewRejectedTestOrigin, "scanner")
	if p.Attrs[hooks.AttrModelKnobURL] != wantKnob {
		t.Errorf("attrs.model_knob_url = %q, want %q", p.Attrs[hooks.AttrModelKnobURL], wantKnob)
	}
	if p.Causation.Depth != 0 {
		t.Errorf("a human denial is world-originated; causation depth = %d, want 0", p.Causation.Depth)
	}

	// Replay: the journal answers 409 and the emitter must stay silent.
	rec = doOwnerPostBodyAs(s, reviewRejectedTestOperator, "/api/approvals/resolve",
		map[string]any{"id": id, "approved": false, "rationale": reviewRejectedTestRationale})
	if rec.Code != http.StatusConflict {
		t.Fatalf("replayed deny = %d, want 409", rec.Code)
	}
	if n := len(capture.all()); n != 1 {
		t.Errorf("a replayed denial re-emitted: %d payloads, want 1", n)
	}
}

// TestApprovalApproveDoesNotEmitReviewRejected is the negative control: a
// grant is not a rejection.
func TestApprovalApproveDoesNotEmitReviewRejected(t *testing.T) {
	s, inbox, capture := reviewRejectedServer(t, "scanner")
	id := inbox.List()[0].ID

	rec := doOwnerPostBodyAs(s, reviewRejectedTestOperator, "/api/approvals/resolve",
		map[string]any{"id": id, "approved": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if n := len(capture.all()); n != 0 {
		t.Errorf("an approval emitted review_rejected %d times, want 0", n)
	}
}

// TestApprovalBulkDenyEmitsOncePerDeniedItem pins that a bulk deny is N
// rejections, each naming its own agent and target, and that an ID that did
// not resolve (unknown here) emits nothing.
func TestApprovalBulkDenyEmitsOncePerDeniedItem(t *testing.T) {
	s, inbox, capture := reviewRejectedServer(t, "scanner", "reviewer", "scanner")

	ids := []string{"not-a-pending-id"}
	for _, p := range inbox.List() {
		ids = append(ids, p.ID)
	}
	rec := doOwnerPostBodyAs(s, reviewRejectedTestOperator, "/api/approvals/bulk",
		map[string]any{"ids": ids, "approved": false, "rationale": reviewRejectedTestRationale})
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk deny = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	got := capture.all()
	if len(got) != 3 {
		t.Fatalf("bulk deny of 3 real ids emitted %d review_rejected, want 3: %+v", len(got), got)
	}
	seenPR := map[string]bool{}
	for _, p := range got {
		if p.Transition != hooks.TransitionReviewRejected {
			t.Errorf("transition = %q", p.Transition)
		}
		if p.Actor != reviewRejectedTestOperator || p.Reason != reviewRejectedTestRationale {
			t.Errorf("payload lost operator/rationale: actor=%q reason=%q", p.Actor, p.Reason)
		}
		pr := p.Attrs[hooks.AttrPR]
		if seenPR[pr] {
			t.Errorf("PR %s was emitted twice", pr)
		}
		seenPR[pr] = true
	}
	if !seenPR["100"] || !seenPR["101"] || !seenPR["102"] {
		t.Errorf("bulk deny did not name every denied PR: %v", seenPR)
	}
}

// TestApprovalDenyUnknownAgentFallsBackToConfig pins the degraded path: an
// agent the manager does not know still gets its backend/model from static
// config, with no pin, so the notification names what it can.
func TestApprovalDenyUnknownAgentFallsBackToConfig(t *testing.T) {
	s, inbox, capture := reviewRejectedServer(t, "reviewer")
	s.deps.Config.Agents["reviewer"] = config.AgentConfig{Backend: "copilot", Model: "claude-opus-4-6"}
	id := inbox.List()[0].ID

	rec := doOwnerPostBodyAs(s, reviewRejectedTestOperator, "/api/approvals/resolve",
		map[string]any{"id": id, "approved": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("deny = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	got := capture.all()
	if len(got) != 1 {
		t.Fatalf("got %d payloads, want 1", len(got))
	}
	if got[0].Backend != "copilot" || got[0].Model != "claude-opus-4-6" || got[0].Pin != "" {
		t.Errorf("config fallback: backend=%q model=%q pin=%q", got[0].Backend, got[0].Model, got[0].Pin)
	}
}

// TestApprovalDenyWithoutHookSeamIsSilent pins nil-safety: a hive with no
// HookFire wired records the denial and does not panic.
func TestApprovalDenyWithoutHookSeamIsSilent(t *testing.T) {
	s, inbox, _ := reviewRejectedServer(t, "scanner")
	s.deps.HookFire = nil
	id := inbox.List()[0].ID

	rec := doOwnerPostBodyAs(s, reviewRejectedTestOperator, "/api/approvals/resolve",
		map[string]any{"id": id, "approved": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("deny = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if inbox.Count() != 0 {
		t.Errorf("denial was not journaled: %d still pending", inbox.Count())
	}
}

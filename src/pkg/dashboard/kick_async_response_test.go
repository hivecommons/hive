package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// These tests pin the response contract that makes kubestellar/hive#5325
// unreachable: POST /api/kick answers as soon as the kick is QUEUED, so the
// handler can never outlive an ingress idle timeout and produce a 504 for a
// kick that actually succeeded.
//
// The contract has three load-bearing halves, one per test below:
//  1. a genuine precondition failure is STILL a synchronous 400,
//  2. an accepted kick is 202 with a status that does not claim delivery, and
//  3. delivery outcome is readable from a separate, always-fast endpoint whose
//     pending phase is explicitly indeterminate rather than failed.

func decodeKickJSON(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("response is not JSON (%v): %s", err, body)
	}
	return m
}

// TestKickPreconditionFailureStaysSynchronous400 is the constraint that keeps
// the async contract honest. Moving delivery off the request path must not turn
// an un-kickable agent into a cheerful 202: an agent that is not running has
// failed deterministically and instantly, and the operator must be told so on
// the POST itself. The test server's "scanner" has no tmux session.
func TestKickPreconditionFailureStaysSynchronous400(t *testing.T) {
	s, _ := apiServer(t)

	rec := doPost(s, "/api/kick/scanner", map[string]string{"message": "hi"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("kick of a non-running agent = %d, want 400 (genuine failures must not become 202)", rec.Code)
	}
	body := decodeKickJSON(t, rec.Body.String())
	if ok, _ := body["ok"].(bool); ok {
		t.Errorf("precondition failure reported ok=true: %v", body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Errorf("precondition failure carried no error message: %v", body)
	}
}

// TestKickStatusEndpointReportsUnknownNotFailure asserts that an agent with no
// dispatch on record is reported as "unknown" and NOT pending — and above all
// not as a failure. A poll that invented a failure for a kick nobody sent would
// recreate the false-failure→retry→double-delivery loop from the other end.
func TestKickStatusEndpointReportsUnknownNotFailure(t *testing.T) {
	s, _ := apiServer(t)

	rec := doGet(s, "/api/kick/scanner/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("kick status = %d, want 200", rec.Code)
	}
	body := decodeKickJSON(t, rec.Body.String())
	if got, _ := body["status"].(string); got != kickStatusUnknown {
		t.Errorf("status = %q, want %q", got, kickStatusUnknown)
	}
	if pending, _ := body["pending"].(bool); pending {
		t.Errorf("an agent with no dispatch must not report pending: %v", body)
	}
	if _, has := body["error"]; has {
		t.Errorf("absence of a dispatch must not be reported as an error: %v", body)
	}
}

// TestKickAcceptedResponseCarriesJSONContentType guards the interaction between
// this fix and #5306. The dashboard's postJSON sniffs Content-Type and, when it
// is not JSON, throws an error naming the HTTP status — so a 202 written before
// its Content-Type (WriteHeader freezes the header map) would be reported to the
// operator as "server returned 202 Accepted", recreating the false failure this
// change exists to remove.
func TestKickAcceptedResponseCarriesJSONContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	jsonStatusResponse(rec, http.StatusAccepted, map[string]interface{}{"ok": true, "status": kickStatusQueued})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json — postJSON would report this 202 as a failure", ct)
	}
	body := decodeKickJSON(t, rec.Body.String())
	if got, _ := body["status"].(string); got != kickStatusQueued {
		t.Errorf("status = %q, want %q", got, kickStatusQueued)
	}
}

// TestKickPhaseStatusMapsPendingToInFlight pins the mapping the UI branches on.
// Everything that is not a settled terminal phase must surface as "in-flight",
// because the issue's core requirement is that an unsettled kick is
// INDETERMINATE. Silently mapping an unrecognised phase to "failed" is the
// mistake this guards against.
func TestKickPhaseStatusMapsPendingToInFlight(t *testing.T) {
	cases := map[string]string{
		agent.KickPhaseDelivered: kickStatusDelivered,
		agent.KickPhaseFailed:    kickStatusFailed,
		agent.KickPhasePending:   kickStatusInFlight,
		"":                       kickStatusInFlight,
		"something-new":          kickStatusInFlight,
	}
	for phase, want := range cases {
		if got := kickPhaseStatus(phase); got != want {
			t.Errorf("kickPhaseStatus(%q) = %q, want %q", phase, got, want)
		}
	}
}

// TestKickHandlerDoesNotCallSynchronousSendKick is a source-level guard. The
// bug was structural — an unbounded wait on the request path — so the durable
// protection is that handleKick routes through the async entry point. A future
// edit that reinstates the inline SendKick reinstates the 504.
func TestKickHandlerDoesNotCallSynchronousSendKick(t *testing.T) {
	raw, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func (s *Server) handleKick(")
	if start < 0 {
		t.Fatal("handleKick not found in api.go")
	}
	// Slice to the next top-level declaration so the assertions see only this
	// handler and not the whole file.
	rest := src[start:]
	if end := strings.Index(rest, "\n// Kick dispatch statuses on the wire."); end > 0 {
		rest = rest[:end]
	}
	handler := rest
	if strings.Contains(handler, "AgentMgr.SendKick(") {
		t.Error("handleKick calls the synchronous SendKick again — its 120s prompt wait outlives the proxy (#5325)")
	}
	if !strings.Contains(handler, "AgentMgr.SendKickAsync(") {
		t.Error("handleKick no longer uses SendKickAsync; the kick is back on the request path")
	}
	if !strings.Contains(handler, "http.StatusAccepted") {
		t.Error("handleKick no longer answers 202; a queued kick must not be reported as a completed one")
	}
}

// TestKickUIPollsForOutcomeAndNeverRendersPendingAsFailure pins the browser
// half. postJSON (from #5306) correctly surfaces real HTTP statuses and must
// stay; what this adds is that the kick call sites treat the 202 as "queued"
// and learn the real outcome from the status poll, with the give-up path
// worded as still-waiting rather than failed.
func TestKickUIPollsForOutcomeAndNeverRendersPendingAsFailure(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"async function pollKickOutcome(agent)",
		"fetch(`/api/kick/${agent}/status`)",
		"async function reportKickOutcome(agent, toast, queuedMsg)",
		"if (!outcome.settled) {",
		"data.status === 'in-flight'",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — the kick UI stopped reading the async outcome (#5325)", snippet)
		}
	}
	// The exhausted-poll branch must not use the error toast type: running out
	// of client-side patience is not evidence of failure.
	if strings.Contains(html, "if (!outcome.settled) {\n        dismissToast(toast, `${queuedMsg} — still waiting for ${agent}'s CLI; check the terminal`, 'error');") {
		t.Error("an unsettled kick is rendered as an error toast; pending is indeterminate, not failed")
	}
}

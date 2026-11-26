package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const chatConfigIdleAnswer = `{"agent":"scanner","section":"cadences","set":{"idle":1800}}`

// chatConfigApplyPut is the card's Apply: the owner's PUT with the proposal
// id in X-Hive-Chat-Proposal.
func chatConfigApplyPut(s *Server, path, user, id string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(body)
	req := httptest.NewRequest(http.MethodPut, path, &b)
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	if id != "" {
		req.Header.Set(chatConfigProposalHeader, id)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func chatConfigProposalID(t *testing.T, resp map[string]any) string {
	t.Helper()
	p, _ := resp["config_proposal"].(map[string]any)
	id, _ := p["id"].(string)
	if id == "" {
		t.Fatalf("no proposal: %v", resp)
	}
	return id
}

func chatConfigIdleCadence(s *Server) string {
	return string(s.deps.Config.Governor.Modes["idle"].Cadences["scanner"])
}

func chatConfigUndo(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	return doPost(s, "/api/chat/config-proposals/"+id+"/undo", nil)
}

func TestChatConfigApply_AppliesStoredBodyOnce(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	path := "/api/config/agent/scanner/cadences"

	if rec := chatConfigApplyPut(s, path, "", id, map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	if got := chatConfigIdleCadence(s); got != "30m" {
		t.Fatalf("cadence = %q, want 30m", got)
	}
	if d := agentConfigAuditDetail(t, s, "config_agent_cadences"); !strings.Contains(d, ", via=chat, proposal="+id) {
		t.Fatalf("apply detail = %q", d)
	}
	// Single use: a second click or a second tab gets 409.
	if rec := chatConfigApplyPut(s, path, "", id, map[string]any{"idle": 1800}); rec.Code != http.StatusConflict {
		t.Fatalf("second apply: %d, want 409", rec.Code)
	}
	if chatConfigAuditCount(s, "chat_config_apply_failed") != 1 {
		t.Fatal("want one chat_config_apply_failed line for the second apply")
	}
	// An applied proposal is no longer pending: Dismiss does not find it.
	if rec := doPost(s, "/api/chat/config-proposals/"+id+"/decline", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("decline after apply: %d, want 404", rec.Code)
	}
}

func TestChatConfigApply_ModelsBody(t *testing.T) {
	s, deps, _ := chatConfigServer(t, `{"agent":"scanner","section":"models","set":{"model":"claude-opus-5-5"}}`)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "switch the scanner agent model to opus", true))
	if rec := chatConfigApplyPut(s, "/api/config/agent/scanner/models", "", id, map[string]any{"model": "claude-opus-5-5"}); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Agents["scanner"].Model; got != "claude-opus-5-5" {
		t.Fatalf("model = %q", got)
	}
}

// Every binding mismatch is a 409 that changes nothing and leaves the
// proposal pending for the real Apply.
func TestChatConfigApply_BindingMismatch409(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	cases := []struct {
		name, path, user, id string
		body                 any
	}{
		{"other user", "/api/config/agent/scanner/cadences", "mallory", id, map[string]any{"idle": 1800}},
		{"other body", "/api/config/agent/scanner/cadences", "", id, map[string]any{"idle": 600}},
		{"extra field", "/api/config/agent/scanner/cadences", "", id, map[string]any{"idle": 1800, "busy": 60}},
		{"other section", "/api/config/agent/scanner/general", "", id, map[string]any{"idle": 1800}},
		{"unknown id", "/api/config/agent/scanner/cadences", "", "deadbeef", map[string]any{"idle": 1800}},
	}
	for _, c := range cases {
		if rec := chatConfigApplyPut(s, c.path, c.user, c.id, c.body); rec.Code != http.StatusConflict {
			t.Fatalf("%s: %d %s, want 409", c.name, rec.Code, rec.Body.String())
		}
		if got := chatConfigIdleCadence(s); got != "15m" {
			t.Fatalf("%s: cadence changed to %q", c.name, got)
		}
	}
	if got := chatConfigAuditCount(s, "chat_config_apply_failed"); got != len(cases) {
		t.Fatalf("chat_config_apply_failed lines = %d, want %d", got, len(cases))
	}
	if rec := chatConfigApplyPut(s, "/api/config/agent/scanner/cadences", "", id, map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("apply after mismatches: %d %s", rec.Code, rec.Body.String())
	}
}

func TestChatConfigApply_StaleProposal409(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	// A dialog save (no header) changes the section after the proposal.
	if rec := chatConfigPut(s, "/api/config/agent/scanner/cadences", "", map[string]any{"idle": 600}); rec.Code != http.StatusOK {
		t.Fatalf("dialog save: %d", rec.Code)
	}
	rec := chatConfigApplyPut(s, "/api/config/agent/scanner/cadences", "", id, map[string]any{"idle": 1800})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale apply: %d, want 409", rec.Code)
	}
	if got := chatConfigIdleCadence(s); got != "10m" {
		t.Fatalf("cadence = %q, want the dialog's 10m", got)
	}
	if d := agentConfigAuditDetail(t, s, "chat_config_apply_failed"); !strings.Contains(d, "proposal="+id) || !strings.Contains(d, "status=409") {
		t.Fatalf("apply_failed detail = %q", d)
	}
}

func TestChatConfigApply_FlagOff(t *testing.T) {
	s, deps, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	off := false
	deps.Config.Chat.ConfigAssist.Enabled = &off
	if rec := chatConfigApplyPut(s, "/api/config/agent/scanner/cadences", "", id, map[string]any{"idle": 1800}); rec.Code != http.StatusForbidden {
		t.Fatalf("apply with config assist off: %d, want 403", rec.Code)
	}
	if rec := chatConfigUndo(t, s, id); rec.Code != http.StatusForbidden {
		t.Fatalf("undo with config assist off: %d, want 403", rec.Code)
	}
	if got := chatConfigIdleCadence(s); got != "15m" {
		t.Fatalf("cadence changed to %q", got)
	}
}

func TestChatConfigProposalStore_ClaimIsSingleUse(t *testing.T) {
	var st chatConfigProposalStore
	now := time.Now()
	if err := st.add(&chatConfigProposal{ID: "p1", User: "alice", Expires: now.Add(time.Minute)}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.claim("p1", "bob", now); err != errChatConfigNoProposal {
		t.Fatalf("other user's claim: err = %v", err)
	}
	if _, err := st.claim("p1", "alice", now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.claim("p1", "alice", now); err != errChatConfigInFlight {
		t.Fatalf("second claim: err = %v, want in flight", err)
	}
	if _, ok := st.lookup("p1", "alice", now); ok {
		t.Fatal("in-flight proposal still pending")
	}
	st.remove("p1")
	st.release("p1")
	if _, err := st.claim("p1", "alice", now); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	st.markApplied("p1", "h", now)
	if _, err := st.claim("p1", "alice", now); err != errChatConfigNoProposal {
		t.Fatalf("claim of applied proposal: err = %v", err)
	}
	if p, ok := st.applied("p1", "alice", now); !ok || p.AppliedHash != "h" {
		t.Fatalf("applied = %+v, %v", p, ok)
	}
	// Applied proposals do not hold a cap slot.
	for i := 0; i < chatConfigProposalPerUser; i++ {
		if err := st.add(&chatConfigProposal{ID: "q" + string(rune('0'+i)), User: "alice", Expires: now.Add(time.Minute)}, now); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
}

func TestChatConfigUndo_RestoresBeforeValue(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	path := "/api/config/agent/scanner/cadences"
	if rec := chatConfigApplyPut(s, path, "", id, map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	rec := chatConfigUndo(t, s, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	rev, _ := resp["config_proposal"].(map[string]any)
	revID, _ := rev["id"].(string)
	if rev["reverses"] != id || rev["before"] != "30m" || rev["after"] != "15m" || revID == "" || revID == id {
		t.Fatalf("reverse proposal = %v", rev)
	}
	body, _ := rev["body"].(map[string]any)
	if body["idle"] != float64(900) {
		t.Fatalf("reverse body = %v, want idle=900", rev["body"])
	}
	// Creating the reverse proposal writes nothing; its Apply does.
	if got := chatConfigIdleCadence(s); got != "30m" {
		t.Fatalf("cadence = %q before the undo apply", got)
	}
	if rec := chatConfigApplyPut(s, path, "", revID, body); rec.Code != http.StatusOK {
		t.Fatalf("undo apply: %d %s", rec.Code, rec.Body.String())
	}
	if got := chatConfigIdleCadence(s); got != "15m" {
		t.Fatalf("cadence = %q after undo, want 15m", got)
	}
	if d := agentConfigAuditDetail(t, s, "chat_config_undo"); !strings.Contains(d, "proposal="+revID) || !strings.Contains(d, "reverses="+id) {
		t.Fatalf("undo detail = %q", d)
	}
	// Once undone, it cannot be undone again, from the store or the log.
	if rec := chatConfigUndo(t, s, id); rec.Code != http.StatusNotFound {
		t.Fatalf("second undo: %d, want 404", rec.Code)
	}
}

func TestChatConfigUndo_SectionChanged409(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	path := "/api/config/agent/scanner/cadences"
	if rec := chatConfigApplyPut(s, path, "", id, map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	if rec := chatConfigPut(s, path, "", map[string]any{"idle": 600}); rec.Code != http.StatusOK {
		t.Fatalf("dialog save: %d", rec.Code)
	}
	if rec := chatConfigUndo(t, s, id); rec.Code != http.StatusConflict {
		t.Fatalf("undo after change: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if got := chatConfigIdleCadence(s); got != "10m" {
		t.Fatalf("cadence = %q, want the dialog's 10m", got)
	}
}

// A reverse proposal is bound to the section when it was made: a change
// between Undo and its Apply is a 409 too.
func TestChatConfigUndo_ApplyAfterChange409(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	path := "/api/config/agent/scanner/cadences"
	if rec := chatConfigApplyPut(s, path, "", id, map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d", rec.Code)
	}
	revID := chatConfigProposalID(t, decodeJSON(t, chatConfigUndo(t, s, id)))
	if rec := chatConfigPut(s, path, "", map[string]any{"idle": 600}); rec.Code != http.StatusOK {
		t.Fatalf("dialog save: %d", rec.Code)
	}
	if rec := chatConfigApplyPut(s, path, "", revID, map[string]any{"idle": 900}); rec.Code != http.StatusConflict {
		t.Fatalf("stale undo apply: %d, want 409", rec.Code)
	}
	if chatConfigAuditCount(s, "chat_config_undo") != 0 {
		t.Fatal("chat_config_undo written for a refused undo")
	}
}

// After a restart drops the store, Undo rebuilds the applied proposal from
// the audit log.
func TestChatConfigUndo_FromAuditAfterRestart(t *testing.T) {
	s, _, _ := chatConfigServer(t, chatConfigIdleAnswer)
	id := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	path := "/api/config/agent/scanner/cadences"
	if rec := chatConfigApplyPut(s, path, "", id, map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d", rec.Code)
	}
	s.chatConfigProposals.mu.Lock()
	s.chatConfigProposals.byID = nil
	s.chatConfigProposals.mu.Unlock()

	rec := chatConfigUndo(t, s, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo from audit: %d %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	revID := chatConfigProposalID(t, resp)
	body, _ := resp["config_proposal"].(map[string]any)["body"].(map[string]any)
	if rec := chatConfigApplyPut(s, path, "", revID, body); rec.Code != http.StatusOK {
		t.Fatalf("undo apply: %d %s", rec.Code, rec.Body.String())
	}
	if got := chatConfigIdleCadence(s); got != "15m" {
		t.Fatalf("cadence = %q after undo, want 15m", got)
	}
	// A proposal that was never applied has nothing to undo.
	other := chatConfigProposalID(t, chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true))
	if rec := chatConfigUndo(t, s, other); rec.Code != http.StatusNotFound {
		t.Fatalf("undo of a pending proposal: %d, want 404", rec.Code)
	}
}

func TestChatConfigApply_AuditActionsSensitive(t *testing.T) {
	for _, a := range []string{"chat_config_apply_failed", "chat_config_undo"} {
		if !auditSensitiveActions[a] {
			t.Errorf("%s must be a sensitive audit action", a)
		}
	}
}

// The UI half: the card offers Apply, which sends the stored body with the
// proposal header, and an applied card offers Undo through the undo route.
func TestChatRendersConfigProposalApplyUndo(t *testing.T) {
	html := indexHTML(t)
	card := jsFunctionBody(t, html, "function chatConfigProposalCardHtml(p)")
	if !strings.Contains(card, "chat-config-proposal-apply") {
		t.Error("chatConfigProposalCardHtml has no Apply button")
	}
	apply := jsFunctionBody(t, html, "async function chatApplyConfigProposal(card, p)")
	for _, want := range []string{"'X-Hive-Chat-Proposal': String(p.id || '')", "method: 'PUT'", "JSON.stringify((p.body", "chat-config-proposal-undo", "res.status === 409"} {
		if !strings.Contains(apply, want) {
			t.Errorf("chatApplyConfigProposal does not contain %q", want)
		}
	}
	undo := jsFunctionBody(t, html, "async function chatUndoConfigProposal(card, p)")
	for _, want := range []string{"'/undo'", "chatApplyConfigProposal(card, rev)"} {
		if !strings.Contains(undo, want) {
			t.Errorf("chatUndoConfigProposal does not contain %q", want)
		}
	}
}

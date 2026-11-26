package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// chatConfigServer is a server with config assist on and a fake runner that
// replies with answer.
func chatConfigServer(t *testing.T, answer string) (*Server, *Dependencies, *fakeChatGrounder) {
	t.Helper()
	g := &fakeChatGrounder{answer: answer}
	s, deps := chatEvalServer(t, g)
	on := true
	deps.Config.Chat.ConfigAssist.Enabled = &on
	writableConfig(t, deps.Config)
	return s, deps, g
}

func chatConfigAsk(t *testing.T, s *Server, query string, owner bool) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"query":`+strconvQuote(query)+`}`))
	req.Header.Set("Content-Type", "application/json")
	if owner {
		markOwnerRequest(req)
	} else {
		req.Header.Set("X-Hive-Role", config.RoleMerger)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%q: status %d: %s", query, rec.Code, rec.Body.String())
	}
	return decodeJSON(t, rec)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func chatConfigAuditCount(s *Server, action string) int {
	n := 0
	for _, e := range s.audit.Recent(200) {
		if e.Action == action {
			n++
		}
	}
	return n
}

func TestChatConfigProposal_CardFromStoredProposal(t *testing.T) {
	s, deps, g := chatConfigServer(t, "```json\n{\"agent\":\"scanner\",\"section\":\"cadences\",\"set\":{\"idle\":1800},\"why\":\"Run it every 30 minutes when idle.\"}\n```")
	resp := chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true)
	p, ok := resp["config_proposal"].(map[string]any)
	if !ok {
		t.Fatalf("no config_proposal in %v", resp)
	}
	if p["agent"] != "scanner" || p["section"] != "cadences" || p["field"] != "idle" || p["before"] != "15m" || p["after"] != "30m" {
		t.Fatalf("proposal = %v", p)
	}
	if body, _ := p["body"].(map[string]any); body["idle"] != float64(1800) {
		t.Fatalf("body = %v, want idle=1800", p["body"])
	}
	if id, _ := p["id"].(string); len(id) < 16 {
		t.Fatalf("proposal id %q is too short", id)
	}
	// Nothing was written: the owner's Save in the dialog is the only write.
	if got := deps.Config.Governor.Modes["idle"].Cadences["scanner"]; got != "15m" {
		t.Fatalf("cadence changed to %q by a proposal", got)
	}
	if chatConfigAuditCount(s, "chat_config_proposed") != 1 {
		t.Fatal("want one chat_config_proposed audit line")
	}
	// The turn is isolated: config profile, no snapshot text (PR titles).
	if len(g.calls) != 1 || g.calls[0].profile != chatConfigProfile {
		t.Fatalf("calls = %+v, want one %s turn", g.calls, chatConfigProfile)
	}
	if strings.Contains(g.calls[0].user, "feat: thing") || strings.Contains(g.calls[0].user, "hivecommons/hive") {
		t.Fatal("config turn input carries hive snapshot text")
	}
}

func TestChatConfigProposal_RejectsOutsideAllowlist(t *testing.T) {
	cases := map[string]string{
		"backend":        `{"agent":"scanner","section":"models","set":{"backend":"codex"}}`,
		"enable":         `{"agent":"scanner","section":"general","set":{"enabled":true}}`,
		"other general":  `{"agent":"scanner","section":"general","set":{"onDemand":true}}`,
		"unknown agent":  `{"agent":"nobody","section":"general","set":{"enabled":false}}`,
		"two fields":     `{"agent":"scanner","section":"cadences","set":{"idle":600,"busy":600}}`,
		"unlisted model": `{"agent":"scanner","section":"models","set":{"model":"made-up-model"}}`,
		"unknown mode":   `{"agent":"scanner","section":"cadences","set":{"nightly":600}}`,
		"excluded":       `{"agent":"scanner","section":"connections","set":{"connections":[]}}`,
		"no change":      `{"agent":"scanner","section":"cadences","set":{"idle":"15m"}}`,
		"free text":      `sure, I switched it for you`,
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, _ := chatConfigServer(t, answer)
			resp := chatConfigAsk(t, s, "set the scanner agent config please", true)
			if _, ok := resp["config_proposal"]; ok {
				t.Fatalf("proposal accepted: %v", resp)
			}
			if resp["status"] != "config_proposal_rejected" {
				t.Fatalf("status = %v", resp["status"])
			}
			if chatConfigAuditCount(s, "chat_config_rejected") != 1 {
				t.Fatal("want one chat_config_rejected audit line")
			}
		})
	}
}

func TestChatConfigProposal_AllowedFields(t *testing.T) {
	cases := map[string]string{
		"model":   `{"agent":"scanner","section":"models","set":{"model":"claude-opus-5-5"}}`,
		"effort":  `{"agent":"scanner","section":"models","set":{"reasoning_effort":"high"}}`,
		"disable": `{"agent":"scanner","section":"general","set":{"enabled":false}}`,
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			s, _, _ := chatConfigServer(t, answer)
			resp := chatConfigAsk(t, s, "change the scanner agent config", true)
			if _, ok := resp["config_proposal"].(map[string]any); !ok {
				t.Fatalf("no proposal: %v", resp)
			}
		})
	}
}

func TestChatConfigProposal_OnlyVerifiedOwnerWithFlagOn(t *testing.T) {
	answer := `{"agent":"scanner","section":"cadences","set":{"idle":1800}}`
	s, deps, g := chatConfigServer(t, answer)
	if resp := chatConfigAsk(t, s, "run the scanner agent every 30 minutes", false); resp["config_proposal"] != nil {
		t.Fatal("non-owner got a proposal")
	}
	off := false
	deps.Config.Chat.ConfigAssist.Enabled = &off
	if resp := chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true); resp["config_proposal"] != nil {
		t.Fatal("proposal with config assist off")
	}
	for _, c := range g.calls {
		if c.profile == chatConfigProfile {
			t.Fatal("config turn ran for a non-owner or with the flag off")
		}
	}
}

func chatConfigPut(s *Server, path, user string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(body)
	req := httptest.NewRequest(http.MethodPut, path, &b)
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// via=chat is recorded only for a stored, unexpired proposal of the same
// user, agent and section, and only once.
func TestChatConfigProposal_ViaChatIsServerVerified(t *testing.T) {
	s, _, _ := chatConfigServer(t, `{"agent":"scanner","section":"cadences","set":{"idle":1800}}`)
	resp := chatConfigAsk(t, s, "run the scanner agent every 30 minutes", true)
	p, _ := resp["config_proposal"].(map[string]any)
	id, _ := p["id"].(string)
	if id == "" {
		t.Fatalf("no proposal: %v", resp)
	}
	path := "/api/config/agent/scanner/cadences?via=chat&proposal=" + id

	// Another user cannot claim this proposal.
	if rec := chatConfigPut(s, path, "mallory", map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if d := agentConfigAuditDetail(t, s, "config_agent_cadences"); strings.Contains(d, ", via=chat") || !strings.Contains(d, "via_unverified=chat") {
		t.Fatalf("other user's save detail = %q", d)
	}
	// A made-up id is advisory only.
	if rec := chatConfigPut(s, "/api/config/agent/scanner/cadences?via=chat&proposal=deadbeef", "", map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("save: %d", rec.Code)
	}
	if d := agentConfigAuditDetail(t, s, "config_agent_cadences"); strings.Contains(d, ", via=chat") {
		t.Fatalf("unknown id recorded as via=chat: %q", d)
	}
	// The wrong section does not match.
	if rec := chatConfigPut(s, "/api/config/agent/scanner/general?via=chat&proposal="+id, "", map[string]any{"displayName": "Renamed"}); rec.Code != http.StatusOK {
		t.Fatalf("general save: %d %s", rec.Code, rec.Body.String())
	}
	if d := agentConfigAuditDetail(t, s, "config_agent_general"); strings.Contains(d, ", via=chat") {
		t.Fatalf("wrong section recorded as via=chat: %q", d)
	}
	// The requesting owner's save is attributed, then the proposal is used up.
	if rec := chatConfigPut(s, path, "", map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("save: %d", rec.Code)
	}
	if d := agentConfigAuditDetail(t, s, "config_agent_cadences"); !strings.Contains(d, ", via=chat, proposal="+id) {
		t.Fatalf("verified save detail = %q", d)
	}
	if rec := chatConfigPut(s, path, "", map[string]any{"idle": 1800}); rec.Code != http.StatusOK {
		t.Fatalf("save: %d", rec.Code)
	}
	if d := agentConfigAuditDetail(t, s, "config_agent_cadences"); strings.Contains(d, ", via=chat") {
		t.Fatalf("used proposal attributed twice: %q", d)
	}
}

func TestChatConfigProposal_ModelsViaChatRecordsBeforeAfter(t *testing.T) {
	s, _, _ := chatConfigServer(t, `{"agent":"scanner","section":"models","set":{"model":"claude-opus-5-5"}}`)
	resp := chatConfigAsk(t, s, "switch the scanner agent model to opus", true)
	p, _ := resp["config_proposal"].(map[string]any)
	id, _ := p["id"].(string)
	if id == "" {
		t.Fatalf("no proposal: %v", resp)
	}
	if rec := chatConfigPut(s, "/api/config/agent/scanner/models?via=chat&proposal="+id, "", map[string]any{"model": "claude-opus-5-5"}); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	d := agentConfigAuditDetail(t, s, "config_agent_models")
	if !strings.Contains(d, "via=chat, proposal="+id) || !strings.Contains(d, "model=sonnet -> claude-opus-5-5") {
		t.Fatalf("models detail = %q", d)
	}
}

func TestChatConfigProposal_Decline(t *testing.T) {
	s, _, _ := chatConfigServer(t, `{"agent":"scanner","section":"general","set":{"enabled":false}}`)
	resp := chatConfigAsk(t, s, "disable the scanner agent", true)
	p, _ := resp["config_proposal"].(map[string]any)
	id, _ := p["id"].(string)
	if id == "" {
		t.Fatalf("no proposal: %v", resp)
	}
	path := "/api/chat/config-proposals/" + id + "/decline"
	unverified := httptest.NewRequest(http.MethodPost, path, nil)
	unverified.Header.Set("X-Hive-Role", "owner")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, unverified)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unverified decline: %d, want 403", rec.Code)
	}
	if rec := doPost(s, path, nil); rec.Code != http.StatusOK {
		t.Fatalf("decline: %d %s", rec.Code, rec.Body.String())
	}
	if chatConfigAuditCount(s, "chat_config_declined") != 1 {
		t.Fatal("want one chat_config_declined audit line")
	}
	if rec := doPost(s, path, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second decline: %d, want 404", rec.Code)
	}
}

func TestChatConfigProposal_NoRunnerSaysSo(t *testing.T) {
	s, deps, _ := chatConfigServer(t, "")
	deps.ChatGrounder = nil
	resp := chatConfigAsk(t, s, "switch the scanner agent model to opus", true)
	if resp["status"] != "config_assist_unavailable" || resp["config_proposal"] != nil {
		t.Fatalf("resp = %v", resp)
	}
}

func TestChatConfigProposalStore_Limits(t *testing.T) {
	var st chatConfigProposalStore
	now := time.Now()
	add := func(id, user string, at time.Time) error {
		return st.add(&chatConfigProposal{ID: id, User: user, Expires: at.Add(chatConfigProposalTTL)}, at)
	}
	for i := 0; i < chatConfigProposalPerUser; i++ {
		if err := add("a"+string(rune('0'+i)), "alice", now); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if err := add("a9", "alice", now); err != errChatConfigCap {
		t.Fatalf("per-user cap: err = %v", err)
	}
	if err := add("b0", "bob", now); err != nil {
		t.Fatalf("other user refused: %v", err)
	}
	if _, ok := st.lookup("a0", "bob", now); ok {
		t.Fatal("bob can see alice's proposal")
	}
	if _, ok := st.lookup("a0", "alice", now.Add(chatConfigProposalTTL+time.Second)); ok {
		t.Fatal("expired proposal still served")
	}
	if err := add("a9", "alice", now.Add(chatConfigProposalTTL+time.Second)); err != nil {
		t.Fatalf("cap not freed by expiry: %v", err)
	}

	var rt chatConfigProposalStore
	for i := 0; i < chatConfigProposalRate; i++ {
		if err := rt.allowTurn("alice", now); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if err := rt.allowTurn("alice", now); err != errChatConfigRate {
		t.Fatalf("rate: err = %v", err)
	}
	if err := rt.allowTurn("alice", now.Add(time.Minute+time.Second)); err != nil {
		t.Fatalf("rate not reset after a minute: %v", err)
	}

	var tot chatConfigProposalStore
	for i := 0; i < chatConfigProposalTotal; i++ {
		if err := tot.add(&chatConfigProposal{ID: "t" + string(rune('A'+i)), User: "u" + string(rune('A'+i)), Expires: now.Add(time.Minute)}, now); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if err := tot.add(&chatConfigProposal{ID: "over", User: "zed", Expires: now.Add(time.Minute)}, now); err != errChatConfigCap {
		t.Fatalf("total cap: err = %v", err)
	}
}

func TestChatConfigProposal_AuditActionsSensitive(t *testing.T) {
	for _, a := range []string{"chat_config_proposed", "chat_config_rejected", "chat_config_declined"} {
		if !auditSensitiveActions[a] {
			t.Errorf("%s must be a sensitive audit action", a)
		}
	}
}

// The UI half: the chat renderer shows the card from config_proposal, the
// dialog is pre-filled, and its Save carries the attribution.
func TestChatRendersConfigProposalCard(t *testing.T) {
	html := indexHTML(t)
	body := jsFunctionBody(t, html, "async function chatAskBackend(q, suppressUserEcho)")
	for _, want := range []string{"data.config_proposal", "chatConfigProposalCardHtml(proposal)", "chatBindConfigProposalCard(msgEl, proposal)"} {
		if !strings.Contains(body, want) {
			t.Errorf("chatAskBackend does not contain %q", want)
		}
	}
	card := jsFunctionBody(t, html, "function chatConfigProposalCardHtml(p)")
	for _, want := range []string{"escapeHtml(String(p.why))", "Open in agent config", "chat-config-proposal-dismiss"} {
		if !strings.Contains(card, want) {
			t.Errorf("chatConfigProposalCardHtml does not contain %q", want)
		}
	}
	for _, want := range []string{"function applyChatConfigProposal()", "applyChatConfigProposal();", "'?via=chat&proposal=' + encodeURIComponent(chatProposal.id)", "/decline'"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
}

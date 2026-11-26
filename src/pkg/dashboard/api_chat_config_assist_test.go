package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const chatConfigAssistPath = "/api/config/chat/config-assist"

func TestChatConfigAssistToggleRoundTrip(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	if s.deps.Config.Chat.ConfigAssistEnabled() {
		t.Fatal("must default off")
	}
	if rec := doPut(s, chatConfigAssistPath, map[string]any{"enabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	if !s.deps.Config.Chat.ConfigAssistEnabled() {
		t.Fatal("toggle did not enable")
	}
	// Absent key changes nothing.
	if rec := doPut(s, chatConfigAssistPath, map[string]any{}); rec.Code != http.StatusOK || !s.deps.Config.Chat.ConfigAssistEnabled() {
		t.Fatalf("empty put changed state: %d", rec.Code)
	}
	if rec := doPut(s, chatConfigAssistPath, map[string]any{"enabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("disable: %d", rec.Code)
	}
	if s.deps.Config.Chat.ConfigAssistEnabled() {
		t.Fatal("toggle did not disable")
	}
}

func TestChatConfigAssistToggleOwnerOnly(t *testing.T) {
	s := covApiServer(t)
	for _, role := range []string{"", config.RoleRead, config.RoleMerger, config.RoleOwner} {
		req := httptest.NewRequest(http.MethodPut, chatConfigAssistPath, bytes.NewBufferString(`{"enabled":true}`))
		req.Header.Set("Content-Type", "application/json")
		if role != "" {
			req.Header.Set("X-Hive-Role", role) // no verified marker
		}
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("role %q: status %d, want 403", role, rec.Code)
		}
		if s.deps.Config.Chat.ConfigAssistEnabled() {
			t.Fatalf("role %q enabled the flag", role)
		}
	}
}

// #20092: the GET is owner-only like the PUT, so a non-owner's panel shows
// the owner-only note instead of a switch that answers 403 on save.
func TestChatConfigAssistGetOwnerOnly(t *testing.T) {
	s := covApiServer(t)
	for _, role := range []string{"", config.RoleRead, config.RoleMerger, config.RoleOwner} {
		if rec := doGetRole(s, chatConfigAssistPath, role, false); rec.Code != http.StatusForbidden {
			t.Fatalf("role %q: status %d, want 403", role, rec.Code)
		}
	}
	rec := doGetRole(s, chatConfigAssistPath, config.RoleOwner, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("verified owner: status %d (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"enabled", "model", "fallback_model", "runner_attached"} {
		if _, ok := body[key]; !ok {
			t.Errorf("GET body is missing %q: %v", key, body)
		}
	}
}

// #20092: chat.config_assist.model is saved, audited, served with the
// grounded_answers fallback, cleared by an empty string, and validated.
func TestChatConfigAssistModelRoundTrip(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	s.deps.Config.Chat.GroundedAnswers.Model = "grounded-model"
	if rec := doPut(s, chatConfigAssistPath, map[string]any{"model": " assist-model "}); rec.Code != http.StatusOK {
		t.Fatalf("set model: %d %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Chat.ConfigAssist.Model; got != "assist-model" {
		t.Fatalf("model = %q, want assist-model", got)
	}
	if s.deps.Config.Chat.ConfigAssist.Enabled != nil {
		t.Fatal("a model-only PUT must not touch enabled")
	}
	entries := chatAssistAuditEntries(s)
	if len(entries) != 1 || !strings.Contains(entries[0].Detail, "assist-model") {
		t.Fatalf("audit entries = %+v, want one naming the new model", entries)
	}
	rec := doGetRole(s, chatConfigAssistPath, config.RoleOwner, true)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["model"] != "assist-model" || body["fallback_model"] != "grounded-model" {
		t.Fatalf("GET body = %v", body)
	}
	if rec := doPut(s, chatConfigAssistPath, map[string]any{"model": "two words"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad model: %d, want 400", rec.Code)
	}
	if rec := doPut(s, chatConfigAssistPath, map[string]any{"model": strings.Repeat("m", chatConfigAssistMaxModel+1)}); rec.Code != http.StatusBadRequest {
		t.Fatalf("long model: %d, want 400", rec.Code)
	}
	if got := s.deps.Config.Chat.ConfigAssist.Model; got != "assist-model" {
		t.Fatalf("rejected PUT changed model to %q", got)
	}
	if rec := doPut(s, chatConfigAssistPath, map[string]any{"model": ""}); rec.Code != http.StatusOK {
		t.Fatalf("clear model: %d", rec.Code)
	}
	if got := s.deps.Config.Chat.ConfigAssistModel(); got != "grounded-model" {
		t.Fatalf("cleared model resolves to %q, want grounded-model", got)
	}
}

// #20092: the Settings panel offers the model picker and saves the model
// with the toggle.
func TestChatConfigAssistPanelOffersModel(t *testing.T) {
	html := indexHTML(t)
	body := jsFunctionBody(t, html, "function renderChatConfigAssistPanel(host, data)")
	for _, want := range []string{
		"chat.config_assist.model",
		"'security-model-options'",
		"data.fallback_model",
		"model: state.model",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderChatConfigAssistPanel does not contain %q", want)
		}
	}
}

func chatAssistAuditEntries(s *Server) []AuditEntry {
	var out []AuditEntry
	if s.audit == nil {
		return out
	}
	for _, entry := range s.audit.Recent(100) {
		if entry.Action == "config_chat_assist" {
			out = append(out, entry)
		}
	}
	return out
}

func TestChatConfigAssistToggleAudited(t *testing.T) {
	if !auditSensitiveActions["config_chat_assist"] {
		t.Fatal("config_chat_assist must be a sensitive audit action")
	}
	s := covApiServer(t)
	if s.audit == nil {
		t.Fatal("test server has no audit log")
	}
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	if rec := doPut(s, chatConfigAssistPath, map[string]any{"enabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	entries := chatAssistAuditEntries(s)
	if len(entries) != 1 || !strings.Contains(entries[0].Detail, "before=false") || !strings.Contains(entries[0].Detail, "after=true") {
		t.Fatalf("audit entries = %+v, want one before=false after=true", entries)
	}
	// An absent key and a refused caller write no audit entry.
	doPut(s, chatConfigAssistPath, map[string]any{})
	req := httptest.NewRequest(http.MethodPut, chatConfigAssistPath, bytes.NewBufferString(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unverified PUT: %d, want 403", rec.Code)
	}
	if got := chatAssistAuditEntries(s); len(got) != 1 {
		t.Fatalf("audit entries after no-op and refused PUTs = %d, want 1", len(got))
	}
}

// With no config source path a save is an in-memory no-op, so the toggle
// must refuse with 409 instead of claiming a change lost on restart.
func TestChatConfigAssistToggleRefusesWithoutSourcePath(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.SourcePath = ""
	rec := doPut(s, chatConfigAssistPath, map[string]any{"enabled": true})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Chat.ConfigAssist.Enabled != nil {
		t.Fatalf("refused save left Enabled = %v, want nil", *s.deps.Config.Chat.ConfigAssist.Enabled)
	}
	if got := chatAssistAuditEntries(s); len(got) != 0 {
		t.Fatalf("refused save was audited: %+v", got)
	}
}

// A failed save restores the original (nil) pointer rather than an explicit
// false that a later unrelated save would persist.
func TestChatConfigAssistToggleSaveFailureRollsBack(t *testing.T) {
	s := covApiServer(t)
	unwritableConfig(t, s.deps.Config)
	rec := doPut(s, chatConfigAssistPath, map[string]any{"enabled": true})
	if rec.Code < 400 {
		t.Fatalf("status %d, want an error for a failed save", rec.Code)
	}
	if s.deps.Config.Chat.ConfigAssist.Enabled != nil {
		t.Fatalf("failed save left Enabled = %v, want nil", *s.deps.Config.Chat.ConfigAssist.Enabled)
	}
}

// The hint rides on /api/chat answers for a verified owner even when no
// grounded runner or responder is attached, and never for a non-owner.
func TestChatHandlerCarriesConfigAssistHint(t *testing.T) {
	s := covApiServer(t)
	s.deps.ChatResponder = nil
	ask := func(owner bool) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"query":"switch the reviewer model to opus"}`))
		req.Header.Set("Content-Type", "application/json")
		if owner {
			markOwnerRequest(req)
		} else {
			req.Header.Set("X-Hive-Role", config.RoleMerger)
		}
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %d: %v (%s)", rec.Code, err, rec.Body.String())
		}
		return body
	}
	if hint, _ := ask(true)["config_assist_hint"].(string); hint == "" {
		t.Fatal("owner answer without a runner or responder has no config_assist_hint")
	}
	if _, ok := ask(false)["config_assist_hint"]; ok {
		t.Fatal("non-owner answer carries config_assist_hint")
	}
}

func TestChatConfigAssistHintOwnerOnly(t *testing.T) {
	s := covApiServer(t)
	owner := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	markOwnerRequest(owner)
	q := "switch the reviewer model to opus"
	if s.chatConfigAssistHint(owner, q) == "" {
		t.Fatal("owner should see the hint")
	}
	if s.chatConfigAssistHint(owner, "which agent completed last?") != "" {
		t.Fatal("non-config question must not get the hint")
	}
	merger := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	merger.Header.Set("X-Hive-Role", config.RoleMerger)
	if s.chatConfigAssistHint(merger, q) != "" {
		t.Fatal("non-owner must not see the hint")
	}
	on := true
	s.deps.Config.Chat.ConfigAssist.Enabled = &on
	if s.chatConfigAssistHint(owner, q) != "" {
		t.Fatal("no hint once enabled")
	}
}

package dashboard

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const chatConfigAssistPath = "/api/config/chat/config-assist"

func TestChatConfigAssistToggleRoundTrip(t *testing.T) {
	s := covApiServer(t)
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

func TestChatConfigAssistToggleAudited(t *testing.T) {
	if !auditSensitiveActions["config_chat_assist"] {
		t.Fatal("config_chat_assist must be a sensitive audit action")
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

package dashboard

import (
	"strings"
	"testing"
)

// TestChatRendersConfigAssistHint pins the UI half of the config assist NUX
// (#19979): the chat answer renderer must read config_assist_hint, escape it,
// and let the viewer dismiss it, or the owner never sees the hint the server
// sends.
func TestChatRendersConfigAssistHint(t *testing.T) {
	html := indexHTML(t)
	body := jsFunctionBody(t, html, "async function chatAskBackend(q, suppressUserEcho)")
	for _, want := range []string{
		"data.config_assist_hint",
		"escapeHtml(String(data.config_assist_hint))",
		"chatConfigAssistHintDismissed()",
		"chat-config-assist-dismiss",
		"CHAT_CONFIG_ASSIST_HINT_DISMISSED_KEY",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("chatAskBackend does not contain %q", want)
		}
	}
	if !strings.Contains(html, "function chatConfigAssistHintDismissed()") {
		t.Error("index.html is missing chatConfigAssistHintDismissed")
	}
}

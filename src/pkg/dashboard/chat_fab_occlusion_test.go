package dashboard

import (
	"strings"
	"testing"
)

func TestChatFABIsReadableThroughAtRestAndAccessibleOnFocus(t *testing.T) {
	html := indexHTML(t)

	for _, snippet := range []string{
		"transition: transform 0.15s, background 0.15s, opacity 0.15s;",
		"opacity: 0.45;",
		".hive-chat-fab:hover, .hive-chat-fab:focus-visible,",
		".hive-chat-fab.open, .hive-chat-fab.has-unread { opacity: 1; }",
		".hive-chat-fab:focus-visible { outline: 2px solid var(--amber); outline-offset: 3px; }",
		`<button type="button" class="hive-chat-fab" id="hiveChatFab" title="Ask Hive" aria-label="Ask Hive chat assistant">`,
		"chatFab.classList.toggle('open', isOpen);",
		"chatFab.classList.remove('open');",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — the fixed chat button must stay readable-through at rest and fully visible for hover/focus/open states (#7160)", snippet)
		}
	}
}

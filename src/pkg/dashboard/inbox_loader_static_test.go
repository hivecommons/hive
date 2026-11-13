package dashboard

import (
	"os"
	"strings"
	"testing"
)

// The Inbox loader must never leave the "Loading inbox…" placeholder up: a
// non-OK /api/inbox response renders an error state, and an OK response
// with zero items renders the empty-state text (#17918).
func TestInboxLoaderHandlesNonOKAndEmpty(t *testing.T) {
	src, err := os.ReadFile("static-src/js/inbox.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	start := strings.Index(js, "async function fetchInbox()")
	if start < 0 {
		t.Fatal("fetchInbox missing")
	}
	body := js[start : strings.Index(js[start:], "dashboardScheduleWork('inbox'")+start]
	if strings.Contains(body, "if (!r.ok) return;") {
		t.Fatal("fetchInbox bails out on a non-OK response, leaving the loading placeholder in place")
	}
	for _, want := range []string{"if (!r.ok) {", "_inboxCache = { items: []", "renderInbox();"} {
		if !strings.Contains(body, want) {
			t.Fatalf("fetchInbox non-OK branch missing %q:\n%s", want, body)
		}
	}
	for _, want := range []string{"if (data.error) html +=", "Nothing yet. Connect Plaud or another voice source"} {
		if !strings.Contains(js, want) {
			t.Fatalf("renderInbox missing %q", want)
		}
	}
}

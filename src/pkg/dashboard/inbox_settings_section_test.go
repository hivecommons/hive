package dashboard

import (
	"strings"
	"testing"
)

func TestSettingsInboxSectionWiring(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{
		"'Knowledge', 'Inbox', 'Project Observability'",
		"case 'Inbox': return renderGovInbox();",
		"function renderGovInbox",
		"function loadInboxSettingsInline",
		"loadInboxSettingsInline();",
		"inbox-settings-inline",
		"yamlHint('inbox.spek.repo')",
		"yamlHint('inbox.dibs.url')",
		"yamlHint('inbox.rules')",
		"Default RFC repository",
		"data-inbox-field=\"dibs-url\"",
		"function inboxSettingsRoot",
		"data-inbox-form=",
		"key: 'inbox.spek.repo'",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Settings → Inbox wiring missing %q", want)
		}
	}
}

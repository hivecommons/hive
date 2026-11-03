package dashboard

import (
	"strings"
	"testing"
)

func TestAgentChannelsReposEscaped(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "ch.repos.map(esc).join(', ')") {
		t.Fatal("channel repos must be escaped before rendering")
	}
	if strings.Contains(html, "ch.repos.join(', ')") {
		t.Fatal("channel repos must not be joined without esc()")
	}
}

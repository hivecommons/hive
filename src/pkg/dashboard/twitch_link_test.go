package dashboard

import (
	"strings"
	"testing"
)

// The Twitch live stream is a community link and must appear wherever the
// Discord link does in the dashboard shell (sidebar Help, FAQ, Welcome).
func TestTwitchLiveStreamLinkInDashboardShell(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<a class="oc-nav-item" href="https://www.twitch.tv/hivecommons" target="_blank" rel="noopener"`,
		`<span class="oc-nav-text">Watch live on Twitch</span>`,
		`Watch the hive live on Twitch</a>.</p>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing %q", want)
		}
	}
	if strings.Count(html, `href="https://www.twitch.tv/hivecommons"`) < 3 {
		t.Fatalf("expected Twitch link in sidebar, FAQ and Welcome; got %d occurrences", strings.Count(html, `href="https://www.twitch.tv/hivecommons"`))
	}
}

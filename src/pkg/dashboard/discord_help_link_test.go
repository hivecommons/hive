package dashboard

import (
	"os"
	"strings"
	"testing"
)

func TestDashboardCommunityGroupHasDiscordLink(t *testing.T) {
	html := indexHTML(t)
	want := `<a class="oc-nav-item" href="https://hivecommons.dev/discord" target="_blank" rel="noopener"><span class="oc-nav-emoji">💬</span><span class="oc-nav-text">Discord</span></a>`
	if !strings.Contains(html, want) {
		t.Fatalf("dashboard Community group missing Discord link with safe external-link attributes")
	}
	community := strings.Index(html, `<div class="oc-nav-label">Community</div>`)
	discord := strings.Index(html, want)
	twitch := strings.Index(html, `<span class="oc-nav-text">Live on Twitch</span>`)
	if community == -1 || discord == -1 || twitch == -1 || !(community < discord && discord < twitch) {
		t.Fatalf("Discord should be the first Community link, before Live on Twitch")
	}
}

func TestDashboardFAQWelcomeAndFeedbackMentionDiscord(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="faq-panel"`,
		`Join our Discord</a>; the Hive Commons community can help`,
		`New to Hive? Start here`,
		`href="https://hivecommons.dev/discord" target="_blank" rel="noopener"`,
		`nps-card-discord`,
		`Need help? Join our Discord`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard static index missing Discord snippet %q", want)
		}
	}
}

func TestContributeLandingMentionsDiscord(t *testing.T) {
	raw, err := os.ReadFile("contribute_landing.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`Questions? <a class="discord-link" href="https://hivecommons.dev/discord"`,
		`we'll help you get to your first PR`,
		`placeholder="Chat with other contributors | https://hivecommons.dev/discord`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("contribute landing missing Discord snippet %q", want)
		}
	}
}

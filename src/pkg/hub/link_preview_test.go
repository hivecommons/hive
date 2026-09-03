package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Shared Hive links used to unfurl as GitHub ("Build software better,
// together") because unauthenticated crawlers were redirected to /login and on
// to GitHub's OAuth page, where they scraped GitHub's Open Graph tags. These
// tests pin the detection and the card so that regression can't come back.

func TestIsLinkPreviewCrawler(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		want bool
	}{
		// Real people must never be diverted from the sign-in flow.
		{"empty UA", "", false},
		{"chrome on macOS", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36", false},
		{"safari on iPhone", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1", false},
		{"firefox on linux", "Mozilla/5.0 (X11; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0", false},
		{"curl", "curl/8.4.0", false},

		// Unfurlers — these must get the Hive card.
		{"slack", "Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)", true},
		{"twitter", "Twitterbot/1.0", true},
		{"facebook", "facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)", true},
		{"linkedin", "LinkedInBot/1.0 (compatible; Mozilla/5.0; Apache-HttpClient +http://www.linkedin.com)", true},
		{"discord", "Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)", true},
		{"telegram", "TelegramBot (like TwitterBot)", true},
		{"whatsapp", "WhatsApp/2.23.20.0", true},
		{"skype", "SkypeUriPreview Preview/0.5", true},
		{"embedly", "Embedly/1.0", true},
		{"reddit", "redditbot/1.0", true},
		{"mattermost", "Mattermost-Bot/1.0", true},
		{"googlebot", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", true},
		{"bingbot", "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)", true},

		// Matching is case-insensitive: crawlers vary their casing.
		{"uppercase slackbot", "SLACKBOT-LINKEXPANDING 1.0", true},
		{"mixed case discord", "DiScOrDbOt/2.0", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/login", nil)
			if tt.ua != "" {
				r.Header.Set("User-Agent", tt.ua)
			}
			if got := isLinkPreviewCrawler(r); got != tt.want {
				t.Errorf("isLinkPreviewCrawler(%q) = %v, want %v", tt.ua, got, tt.want)
			}
		})
	}
}

// The whole point of the fix is that the crawler stops here: a 200 with Hive's
// tags, never a redirect toward GitHub.
func TestWriteLinkPreviewServesHiveTags(t *testing.T) {
	w := httptest.NewRecorder()
	writeLinkPreview(w)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (a redirect would send the crawler to GitHub)", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := w.Body.String()
	for _, want := range []string{
		`property="og:title"`,
		`property="og:description"`,
		`property="og:image"`,
		`name="twitter:card" content="summary_large_image"`,
		hubPublicURL() + "/og-card.png",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview HTML missing %q", want)
		}
	}

	// An SVG og:image renders blank on every major unfurler.
	if strings.Contains(body, ".svg") {
		t.Error("preview references an SVG image; unfurlers will not render it")
	}
	// Slackbot only reads the first 32KB of the response.
	const slackbotReadLimit = 32 * 1024
	if len(body) > slackbotReadLimit {
		t.Errorf("preview HTML is %d bytes, exceeds Slackbot's %d-byte read limit", len(body), slackbotReadLimit)
	}
}

// The image must be a real PNG and must be reachable, since registerOAuth
// returning early previously would have left it 404ing.
func TestHandleOGCardServesPNG(t *testing.T) {
	s := &HubServer{}
	w := httptest.NewRecorder()
	s.handleOGCard(w, httptest.NewRequest(http.MethodGet, "/og-card.png", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}

	body := w.Body.Bytes()
	// PNG magic number: the card must be a genuine raster image, not an SVG.
	pngMagic := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if len(body) < len(pngMagic) || string(body[:len(pngMagic)]) != string(pngMagic) {
		t.Fatal("og-card.png is not a valid PNG")
	}
	// Slack silently drops preview images over 1MB.
	const slackImageLimit = 1024 * 1024
	if len(body) > slackImageLimit {
		t.Errorf("og-card.png is %d bytes, exceeds Slack's %d-byte limit", len(body), slackImageLimit)
	}
}

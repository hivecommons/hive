package hub

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestHubLandingCrawlerMetadata(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{"Claude Code", "GitHub Copilot CLI", "IBM Bob", "Goose", "vLLM", "llm-d", "Akamai (Linode)", "Oracle Cloud (OKE)", "Cloudflare", "Bluehost"} {
		if !strings.Contains(html, want) {
			t.Fatalf("landing HTML missing crawler-visible text %q", want)
		}
	}
	if strings.Count(html, `type="application/ld+json"`) == 0 {
		t.Fatal("landing HTML missing JSON-LD script")
	}
	if desc := metaContent(html, `name="description"`); len(desc) == 0 || len(desc) > 160 {
		t.Fatalf("meta description length = %d, want 1..160: %q", len(desc), desc)
	}
	for _, alt := range []string{"Claude Code by Anthropic", "GitHub Copilot CLI by GitHub", "vLLM inference engine", "Akamai (Linode) infrastructure provider"} {
		if !strings.Contains(html, `alt="`+alt+`"`) {
			t.Fatalf("landing HTML missing descriptive alt text %q", alt)
		}
	}
	parseJSONLDScripts(t, html)
}

func TestHubCrawlerTextEndpoints(t *testing.T) {
	for _, name := range []string{"static/robots.txt", "static/sitemap.xml", "static/llms.txt"} {
		b, err := fs.ReadFile(staticFS, name)
		if err != nil {
			t.Fatalf("%s missing from embedded static FS: %v", name, err)
		}
		text := string(b)
		if name == "static/robots.txt" && !strings.Contains(text, "Sitemap: https://hive.hivecommons.dev/sitemap.xml") {
			t.Fatalf("robots.txt missing sitemap pointer: %q", text)
		}
		if name == "static/llms.txt" {
			for _, want := range []string{"Claude Code", "vLLM", "Akamai (Linode)", "Bluehost"} {
				if !strings.Contains(text, want) {
					t.Fatalf("llms.txt missing %q", want)
				}
			}
		}
	}
}

func metaContent(html, attr string) string {
	re := regexp.MustCompile(`<meta\s+` + attr + `\s+content="([^"]*)"`)
	m := re.FindStringSubmatch(html)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

func parseJSONLDScripts(t *testing.T, html string) {
	t.Helper()
	re := regexp.MustCompile(`(?s)<script\s+type="application/ld\+json">(.*?)</script>`)
	matches := re.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		t.Fatal("no JSON-LD scripts found")
	}
	for _, match := range matches {
		var v any
		if err := json.Unmarshal([]byte(match[1]), &v); err != nil {
			t.Fatalf("JSON-LD does not parse: %v", err)
		}
	}
}

package hub

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Every public page is reachable on the retired hive.kubestellar.io host, which
// 301s to the current one — and that redirect DROPS THE PATH, so every legacy
// URL lands on the homepage. Search engines and social unfurlers therefore need
// an explicit self-referencing canonical on the current host; without one the
// only canonical signal a crawler has is og:url, and og:url named the retired
// host, so the hub was actively advertising a domain it no longer serves.

var (
	reOGURL     = regexp.MustCompile(`<meta property="og:url" content="([^"]+)"`)
	reCanonical = regexp.MustCompile(`<link rel="canonical" href="([^"]+)"`)
)

// staticPageCanonicals maps each embedded page to the path it is served on.
// my-hives.html is served at /fleet (with /my-hives redirecting to it), which
// is exactly why it needs a canonical: two paths, one page.
var staticPageCanonicals = map[string]string{
	"index.html":                       "",
	"get-started.html":                 "/get-started",
	"learn.html":                       "/learn",
	"reading.html":                     "/reading",
	"api-docs.html":                    "/api/docs",
	"cncf-reference-architecture.html": "/cncf-reference-architecture",
	"my-hives.html":                    "/fleet",
}

const canonicalOrigin = "https://hive.hivecommons.dev"

func TestStaticPagesDeclareCanonicalOnTheCurrentHost(t *testing.T) {
	for page, path := range staticPageCanonicals {
		b, err := fs.ReadFile(staticFS, "static/"+page)
		if err != nil {
			t.Errorf("reading embedded static/%s: %v", page, err)
			continue
		}
		body := string(b)
		want := canonicalOrigin + path

		m := reCanonical.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s has no <link rel=\"canonical\">; the legacy host's redirect drops the "+
				"path, so without one every legacy URL consolidates onto the homepage", page)
			continue
		}
		if m[1] != want {
			t.Errorf("%s canonical = %q, want %q", page, m[1], want)
		}

		// og:url, where present, must agree with the canonical. Two different
		// answers is worse than one wrong answer: crawlers pick per-crawler.
		if og := reOGURL.FindStringSubmatch(body); og != nil && og[1] != want {
			t.Errorf("%s og:url = %q, want %q (must agree with canonical)", page, og[1], want)
		}
	}
}

// No page may advertise the retired host anywhere a user or crawler will act on
// it — that includes the copy-pasteable curl examples in the API docs, which
// resolve to the homepage's HTML rather than to JSON.
func TestStaticPagesDoNotAdvertiseTheRetiredHost(t *testing.T) {
	entries, err := fs.ReadDir(staticFS, "static")
	if err != nil {
		t.Fatalf("listing embedded static dir: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		b, err := fs.ReadFile(staticFS, "static/"+e.Name())
		if err != nil {
			t.Errorf("reading %s: %v", e.Name(), err)
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "hive.kubestellar.io") {
				continue
			}
			// A comment may name the retired host to explain why something is
			// the way it is; that is documentation, not an advertised URL.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "<!--") {
				continue
			}
			t.Errorf("%s:%d advertises the retired host: %s", e.Name(), i+1, trimmed)
		}
	}
}

// The unfurl-bot fallback is the ONLY thing social crawlers see for /dashboard,
// so a stale og:url there is the link preview every share of the dashboard gets.
func TestOGFallbackNamesTheCurrentHost(t *testing.T) {
	if strings.Contains(ogFallbackHTML, "hive.kubestellar.io") {
		t.Error("ogFallbackHTML still names the retired host; this is the metadata every " +
			"social unfurl of /dashboard renders from")
	}
	m := reOGURL.FindStringSubmatch(ogFallbackHTML)
	if m == nil {
		t.Fatal("ogFallbackHTML has no og:url")
	}
	if want := canonicalOrigin + "/dashboard"; m[1] != want {
		t.Errorf("ogFallbackHTML og:url = %q, want %q", m[1], want)
	}
}

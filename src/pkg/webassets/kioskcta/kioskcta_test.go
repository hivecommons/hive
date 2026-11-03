package kioskcta

import (
	"strings"
	"testing"
)

func TestMarkupDefaultsAndEscaping(t *testing.T) {
	got := string(Markup(Options{}))
	for _, want := range []string{
		`<aside id="kiosk-cta" class="kiosk-cta"`,
		`src="data:image/png;base64,`,
		`class="kiosk-cta-text">🐝 Want this for your repos?<`,
		`class="kiosk-cta-url">hive.hivecommons.dev/get-started<`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markup missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "style=") || strings.Contains(got, "<script") {
		t.Error("markup must carry no inline style or script (page CSP)")
	}

	got = string(Markup(Options{URL: "https://example.test/x?a=1&b=<2>", Text: `"Get" <one>`}))
	if !strings.Contains(got, `class="kiosk-cta-text">&#34;Get&#34; &lt;one&gt;<`) || !strings.Contains(got, `example.test/x?a=1&amp;b=&lt;2&gt;<`) {
		t.Errorf("operator strings not escaped:\n%s", got)
	}
}

func TestInjectReplacesPlaceholderOnce(t *testing.T) {
	page := []byte("<body>" + Placeholder + "<p>x</p>" + Placeholder + "</body>")
	out := string(Inject(page, Options{}))
	if strings.Count(out, `id="kiosk-cta"`) != 1 || strings.Count(out, Placeholder) != 1 {
		t.Errorf("inject must replace exactly the first placeholder:\n%s", out)
	}
	plain := []byte("<body></body>")
	if string(Inject(plain, Options{})) != string(plain) {
		t.Error("a page without the placeholder must be returned untouched")
	}
}

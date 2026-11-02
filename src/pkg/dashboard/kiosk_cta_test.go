package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/webassets/kioskcta"
)

// The served index carries the kiosk call-to-action badge (#16236) where the
// shipped document leaves its placeholder, with dashboard.kiosk_cta_* text.
func TestKioskCTAInjectedIntoIndex(t *testing.T) {
	raw := indexHTML(t)
	if !strings.Contains(raw, kioskcta.Placeholder) {
		t.Fatal("static/index.html has no kiosk CTA placeholder")
	}
	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	s.deps.Config.Dashboard.KioskCTAURL = "https://hive.example/go"
	s.deps.Config.Dashboard.KioskCTAText = "Ask us"
	out := string(kioskcta.Inject([]byte(raw), s.kioskCTAOptions()))
	for _, want := range []string{`<aside id="kiosk-cta" class="kiosk-cta"`, `class="kiosk-cta-url">hive.example/go<`, `class="kiosk-cta-text">Ask us<`} {
		if !strings.Contains(out, want) {
			t.Errorf("injected index missing %q", want)
		}
	}
	if (&Server{}).kioskCTAOptions() != (kioskcta.Options{}) {
		t.Error("a server without config must fall back to the package defaults")
	}
}

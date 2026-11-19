package inbox

import (
	"net/url"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// maxFeaturedInputMethods keeps the Inbox row readable: the rest of the
// catalogue is listed in the docs only.
const maxFeaturedInputMethods = 8

func TestInputMethodsCatalogue(t *testing.T) {
	// Ids that already mean something in the Inbox: every connector type
	// and the hive's own item sources.
	taken := map[string]bool{RecordingType: true, UploadType: true, EmailType: true}
	for _, typ := range connector.DefaultRegistry().Types() {
		taken[typ] = true
	}
	states := map[string]bool{InputWorksToday: true, InputComingSoon: true}
	kinds := map[string]bool{InputKindNeural: true, InputKindSilentSpeech: true, InputKindGaze: true, InputKindSwitch: true, InputKindVoice: true}
	// Words that would claim a relationship the listing does not have.
	claims := []string{"partner", "integrat", "official", "supported by", "certified"}

	methods := InputMethods()
	if len(methods) == 0 {
		t.Fatal("the catalogue is empty")
	}
	seen := map[string]bool{}
	featured, soon, works := 0, 0, 0
	for _, m := range methods {
		if m.ID == "" || m.ID != strings.ToLower(m.ID) || strings.ContainsAny(m.ID, " _/") {
			t.Errorf("%q: id must be lowercase, with dashes", m.ID)
		}
		if seen[m.ID] {
			t.Errorf("%s: duplicate id", m.ID)
		}
		seen[m.ID] = true
		if taken[m.ID] {
			t.Errorf("%s: id is a connector type or an Inbox source; input methods are not connectors", m.ID)
		}
		if strings.TrimSpace(m.Name) == "" {
			t.Errorf("%s: no name", m.ID)
		}
		if !kinds[m.Kind] {
			t.Errorf("%s: kind %q", m.ID, m.Kind)
		}
		if !states[m.State] {
			t.Errorf("%s: state %q", m.ID, m.State)
		}
		if note := strings.TrimSpace(m.Note); note == "" || !strings.HasSuffix(note, ".") || strings.Contains(note, "\n") {
			t.Errorf("%s: the note must be one sentence ending in a full stop: %q", m.ID, m.Note)
		}
		for _, c := range claims {
			if strings.Contains(strings.ToLower(m.Note), c) {
				t.Errorf("%s: the note must not suggest a relationship (%q): %q", m.ID, c, m.Note)
			}
		}
		u, err := url.Parse(m.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			t.Errorf("%s: url must be an https link: %q", m.ID, m.URL)
		}
		if m.Featured {
			featured++
		}
		switch m.State {
		case InputComingSoon:
			soon++
		case InputWorksToday:
			works++
		}
	}
	if soon == 0 || works == 0 {
		t.Errorf("want entries in both states, got %d coming soon and %d working today", soon, works)
	}
	if featured == 0 || featured > maxFeaturedInputMethods {
		t.Errorf("featured = %d, want 1 to %d so the Inbox row stays readable", featured, maxFeaturedInputMethods)
	}

	// The caller gets a copy.
	methods[0].Name = "changed"
	if InputMethods()[0].Name == "changed" {
		t.Error("InputMethods must return a copy")
	}
}

package dashboard

import (
	"strings"
	"testing"
)

func TestDashboardVisibleCopyUsesWorkSourceNeutralTerms(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	body := string(b)
	for _, phrase := range []string{
		"<span class=\"oc-nav-text\">Repos</span>",
		"Move Repositories section",
		"> Repositories <span id=\"repos-host-badge\"",
		"Every repo in this hive is on this single GitHub host",
		"Using GitHub Issues</b> — hive reads open issues from the repos listed in the Repos tab",
		"Your hive is a fleet of AI <strong>agents</strong> that work your repositories",
		"Settings → <strong>Repos</strong>: add every repository",
	} {
		if strings.Contains(body, phrase) {
			t.Fatalf("static/index.html still contains generic GitHub/repository wording %q", phrase)
		}
	}
}

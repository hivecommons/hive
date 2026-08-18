package dashboard

import (
	"strings"
	"testing"
)

func TestTimeOfDayCadenceUIPinning(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, snippet := range []string{
		"Daily at…",
		"Weekdays at…",
		"Custom cron",
		"Intl.DateTimeFormat().resolvedOptions().timeZone",
		"times: (document.getElementById(`cad-${mode}-times`)",
		"Interval and time-of-day modes are mutually exclusive",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
}

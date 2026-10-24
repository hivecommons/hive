package watchdog

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyCopilotLoginPointsAtDashboard(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		backend string
		want    bool
	}{{"copilot", true}, {"claude", false}} {
		c := Classify(Observation{Backend: tc.backend, SessionExists: true, ShowsLoginPrompt: true, StartedAt: now.Add(-time.Hour)}, now, Settings{BootGrace: time.Minute})
		if c.Class != ClassAuthRequired {
			t.Fatalf("%s: class = %v", tc.backend, c.Class)
		}
		if got := strings.Contains(c.Reason, "dashboard device-flow login"); got != tc.want {
			t.Errorf("%s: reason %q mentions dashboard = %v, want %v", tc.backend, c.Reason, got, tc.want)
		}
	}
}

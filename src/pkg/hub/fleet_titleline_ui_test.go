package hub

import (
	"os"
	"strings"
	"testing"
)

func fleetStaticHTML(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("static/my-hives.html")
	if err != nil {
		t.Fatalf("read fleet static html: %v", err)
	}
	return string(b)
}

func TestFleetTitleLineReusesAccessAvatarHelpers(t *testing.T) {
	html := fleetStaticHTML(t)
	for _, want := range []string{
		"function accessAvatarTitle(a)",
		"function inlineAccessAvatar(a)",
		"function hiveAccessAvatars(h)",
		"return linkedAvatar(uname, INLINE_ACCESS_AVATAR_PX, accessAvatarTitle(a),",
		"hiveNameHTML(h) + hiveAccessAvatars(h)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("fleet static html missing %q", want)
		}
	}
}

func TestFleetShowsCadenceAndRosterWarning(t *testing.T) {
	html := fleetStaticHTML(t)
	for _, want := range []string{
		`rs === "working" && a.kickIntervalSec`,
		"function formatCadence(seconds)",
		"function rosterMismatchChipHTML(h)",
		"agent roster mismatch",
		`if (h.agentRosterMismatch && h.agentRosterMismatch.reason) add("agent roster"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("fleet static html missing %q", want)
		}
	}
}

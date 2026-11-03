package panesignal

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/cline"
)

func TestClineMarkers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pane    string
		present bool
		working bool
		prompt  bool
	}{
		{"empty", "", false, false, false},
		{"bare shell", "$ ", false, false, false},
		{"ready", cline.ReadyMarker, true, false, true},
		{"running", cline.RunningMarker, true, true, false},
		{"started turn", cline.ReadyMarker + "\n" + cline.RunningMarker, true, true, true},
		{"completed turn", cline.RunningMarker + "\n" + cline.ReadyMarker, true, false, true},
		{"next turn", cline.RunningMarker + "\n" + cline.ReadyMarker + "\n" + cline.RunningMarker, true, true, true},
		{"ready scrolled away", strings.Repeat(cline.RunningMarker+"\n", 40), true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasCLIMarker(tc.pane); got != tc.present {
				t.Errorf("HasCLIMarker = %v, want %v", got, tc.present)
			}
			if got := ShowsAgentWorking(tc.pane); got != tc.working {
				t.Errorf("ShowsAgentWorking = %v, want %v", got, tc.working)
			}
			if got := ShowsInputPrompt(tc.pane); got != tc.prompt {
				t.Errorf("ShowsInputPrompt = %v, want %v", got, tc.prompt)
			}
		})
	}
}

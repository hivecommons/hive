package dashboard

import (
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An issue still carrying needs-triage reports the needs-triage modifier so
// the activity monitor's action chip can read "triage needed".
func TestStageTrackerModifiers_NeedsTriage(t *testing.T) {
	got := stageTrackerModifiers([]string{"needs-triage", "bug"}, "hv", stagelabels.Record{}, false, nil, time.Now(), false)
	if !slices.Equal(got, []string{modNeedsTriage}) {
		t.Fatalf("modifiers = %v, want [%s]", got, modNeedsTriage)
	}
	if got := stageTrackerModifiers([]string{"bug"}, "hv", stagelabels.Record{}, false, nil, time.Now(), false); len(got) != 0 {
		t.Fatalf("modifiers = %v, want none", got)
	}
}

package dashboard

import "testing"

func TestBasePreparationCaptionNeverAdvertisesReady(t *testing.T) {
	for reason, want := range map[string]string{
		"base-refresh:queued-behind#42": "queued for base refresh behind PR #42",
		"base-refresh:queued-behind#0":  "waiting for admission to final base preparation",
		"base-refresh:preparing":        "preparing the current base; current-head CI and review must complete",
	} {
		if got := humanizeStageReason(reason); got != want {
			t.Errorf("%s caption=%q, want %q", reason, got, want)
		}
	}
}

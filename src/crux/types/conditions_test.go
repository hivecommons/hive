package types

import "testing"

type recordingReporter struct{ observed, cleared []string }

func (r *recordingReporter) Observe(class, key, message, diagnostics string) {
	r.observed = append(r.observed, class+"/"+key+": "+message+" ["+diagnostics+"]")
}
func (r *recordingReporter) Clear(class, key string) { r.cleared = append(r.cleared, class+"/"+key) }

func TestConditionReporterContract(t *testing.T) {
	var rep ConditionReporter = &recordingReporter{}
	rep.Observe(ClassAgentStall, "scanner", "stalled", "diag")
	rep.Clear(ClassAgentStall, "scanner")
	r := rep.(*recordingReporter)
	if len(r.observed) != 1 || r.observed[0] != "agent_stall/scanner: stalled [diag]" {
		t.Fatalf("observed = %q", r.observed)
	}
	if len(r.cleared) != 1 || r.cleared[0] != "agent_stall/scanner" {
		t.Fatalf("cleared = %q", r.cleared)
	}
}

func TestClassWireNamesAreStable(t *testing.T) {
	want := map[string]string{
		ClassRestartStorm:             "restart_storm",
		ClassKickFailedRepeatedly:     "kick_failed_repeatedly",
		ClassDataDiskPressure:         "data_disk_pressure",
		ClassGitHubAppTokenMintFailed: "github_app_token_mint_failed",
		ClassAgentStall:               "agent_stall",
		ClassAgentCrashLoop:           "agent_crash_loop",
		ClassCIPollNudge:              "ci_poll_nudge",
	}
	for got, exp := range want {
		if got != exp {
			t.Errorf("class %q != %q", got, exp)
		}
	}
}

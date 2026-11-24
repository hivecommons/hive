package reviewrules

import "testing"

// SetConfig changes trust and phrases for comments read from then on
// (hivecommons/hive#19135).
func TestStore_SetConfigAppliesToLaterComments(t *testing.T) {
	s, _, _ := openStore(t, Config{})
	body := "Worth considering for every roster-based assessment you write."
	if res := s.CaptureComment(cmt("inline:1", "CONTRIBUTOR", body), t0); res.Outcome != OutcomeNotRule {
		t.Fatalf("default phrases: %+v", res)
	}
	s.SetConfig(Config{Trusted: ParseAssociations("OWNER,MEMBER,COLLABORATOR,CONTRIBUTOR"), ExtraPhrases: []string{"worth considering"}})
	if res := s.CaptureComment(cmt("inline:2", "CONTRIBUTOR", body), t0); res.Outcome != OutcomeCaptured {
		t.Fatalf("extra phrase and trusted contributor: %+v", res)
	}
	s.SetConfig(Config{DisabledSignals: map[string]bool{SignalGoingForward: true}})
	if res := s.CaptureComment(cmt("inline:3", "OWNER", "Going forward, cite the scorecard with a pinned link."), t0); res.Outcome != OutcomeNotRule {
		t.Fatalf("disabled signal: %+v", res)
	}
}

func TestParseListAndConfigFromEnv(t *testing.T) {
	if got := ParseList(" a, ,b ,"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("ParseList = %q", got)
	}
	t.Setenv(ExtraPhrasesEnvVar, "worth considering, house rule")
	t.Setenv(DisabledSignalsEnvVar, "Authoritative")
	cfg := ConfigFromEnv()
	if len(cfg.ExtraPhrases) != 2 || !cfg.DisabledSignals[SignalAuthoritative] {
		t.Fatalf("cfg = %+v", cfg)
	}
}

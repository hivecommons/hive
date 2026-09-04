package agent

import (
	"testing"
	"time"
)

func TestRecordBlockedAndCheckWindowAndCooldown(t *testing.T) {
	now := time.Unix(1000, 0)
	st := &thrashState{times: []time.Time{now.Add(-2 * time.Minute), now.Add(-30 * time.Second)}}
	if recordBlockedAndCheck(st, now, time.Minute, 3, time.Minute) {
		t.Fatal("threshold should not trip after pruning old entries")
	}
	if len(st.times) != 2 {
		t.Fatalf("kept times = %d, want 2", len(st.times))
	}
	if !recordBlockedAndCheck(st, now.Add(time.Second), time.Minute, 3, time.Minute) {
		t.Fatal("threshold should trip outside cooldown")
	}
	if recordBlockedAndCheck(st, now.Add(2*time.Second), time.Minute, 3, time.Minute) {
		t.Fatal("cooldown should suppress repeated trip")
	}
	st.times = append(st.times, now.Add(119*time.Second), now.Add(119*time.Second))
	if !recordBlockedAndCheck(st, now.Add(2*time.Minute), time.Minute, 3, time.Minute) {
		t.Fatal("trip should reset after cooldown")
	}
}

func TestCheckBlockedThrashIgnoresNonMarkersAndRecordsMarkers(t *testing.T) {
	m := &Manager{}
	m.checkBlockedThrash("scanner", "ordinary output")
	if len(m.thrash) != 0 {
		t.Fatalf("non-marker initialized thrash map: %+v", m.thrash)
	}
	m.checkBlockedThrash("scanner", "git push blocked: advisory mode")
	if len(m.thrash) != 1 {
		t.Fatalf("marker did not create thrash state: %+v", m.thrash)
	}
	if got := len(m.thrash["scanner"].times); got != 1 {
		t.Fatalf("recorded times = %d, want 1", got)
	}
}

func TestBackendLaunchCmdRemainingBranches(t *testing.T) {
	cases := []struct {
		name      string
		binary    string
		model     string
		backend   string
		inference bool
		want      string
	}{
		{"gemini", "gemini", "flash", "gemini", false, "gemini --model flash"},
		{"pi", "pi", "pi-model", "pi", false, "pi --model pi-model"},
		{"goose with model", "goose", "gpt", "goose", false, "goose run -s --model gpt"},
		{"goose no model", "goose", "", "goose", false, "goose run -s"},
		{"default", "custom", "ignored", "custom", false, "custom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := backendLaunchCmd(tc.binary, tc.model, tc.backend, tc.inference); got != tc.want {
				t.Fatalf("backendLaunchCmd() = %q, want %q", got, tc.want)
			}
		})
	}
}

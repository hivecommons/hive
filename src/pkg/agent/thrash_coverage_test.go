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
	m.checkBlockedThrash("scanner", "+\t\"git push blocked: advisory mode\",")
	if len(m.thrash) != 0 {
		t.Fatalf("displayed marker initialized thrash map: %+v", m.thrash)
	}
	m.checkBlockedThrash("scanner", "⛔ git push blocked: advisory mode")
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
			if got := backendLaunchCmd(tc.binary, tc.model, tc.backend, tc.inference, ""); got != tc.want {
				t.Fatalf("backendLaunchCmd() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsBlockedActionLine pins #12015: only the credential helper's own
// "⛔ "-prefixed stderr counts as a blocked attempt. An agent displaying a file
// that contains the marker text (diff, grep -n, cat, markdown) must not trip
// the breaker and pause itself.
func TestIsBlockedActionLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"helper missing token", "⛔ git push blocked: per-agent scoped GitHub token not available (unset).", true},
		{"helper unknown uid", "⛔ git push blocked: unknown agent UID 1234", true},
		{"proxy hard deny", "⛔ blocked by hive policy: push to main", true},
		{"leading whitespace", "   \t⛔ git push blocked: no token", true},
		{"ansi colored", "\x1b[31m⛔ git push blocked: no token\x1b[0m", true},
		{"claude tool gutter", "  ⎿  ⛔ git push blocked: no token", true},
		{"box rail gutter", "│ ⛔ blocked by hive policy", true},
		{"helper refusal follow-up", "⛔ Refusing to fall back to the shared full-privilege App token", false},
		{"ordinary output", "pushed 3 commits", false},
		{"bare marker without glyph", "git push blocked: no token", false},
		{"diff added fixture", "+\t\"git push blocked: no token\",", false},
		{"diff removed fixture", "-\t\"⛔ git push blocked: no token\",", false},
		{"diff of helper script", "+    echo \"⛔ git push blocked: unknown agent UID ${CURRENT_UID}\" >&2", false},
		{"grep -n match", "42:\t\t\"git push blocked: no token\"", false},
		{"grep -n context", "43-\t\t\"⛔ git push blocked: no token\"", false},
		{"grep -rn with path", "bin/git-credential-hive.sh:76:    echo \"⛔ git push blocked: unknown agent UID\" >&2", false},
		{"cat of helper script", "    echo \"⛔ git push blocked: unknown agent UID ${CURRENT_UID}\" >&2", false},
		{"go string literal", "\t\"git push blocked:\",", false},
		{"go string literal with glyph", "\tline := \"⛔ git push blocked: no token\"", false},
		{"markdown fence", "```⛔ git push blocked: no token```", false},
		{"markdown quote", "> ⛔ git push blocked: no token", false},
		{"inline code", "`⛔ git push blocked: no token`", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBlockedActionLine(tc.line); got != tc.want {
				t.Fatalf("isBlockedActionLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestCheckBlockedThrashIgnoresDisplayedFixtures replays the #12015 incident:
// a reviewer printing a test file full of marker fixtures must never be
// paused, no matter how many lines it displays.
func TestCheckBlockedThrashIgnoresDisplayedFixtures(t *testing.T) {
	m := &Manager{}
	for i := 0; i < 3*thrashThreshold; i++ {
		m.checkBlockedThrash("reviewer", "+\t\"git push blocked: no token\",")
		m.checkBlockedThrash("reviewer", "42:\t\t\"git push blocked: no token\"")
	}
	if len(m.thrash) != 0 {
		t.Fatalf("displayed fixtures fed the thrash breaker: %+v", m.thrash)
	}
}

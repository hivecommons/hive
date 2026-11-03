package spoke

import (
	"testing"
	"time"
)

func TestFormatTurns(t *testing.T) {
	turns := []UpgradeTurn{{Agent: "scanner", ElapsedS: 12 * 60}, {Agent: "reviewer", ElapsedS: 38 * 60}, {Agent: "triage"}}
	if got := FormatTurns(turns, 5); got != "3/5 agents in turn: reviewer 38m, scanner 12m, triage" {
		t.Fatalf("FormatTurns = %q", got)
	}
	if got := FormatTurns(turns[:1], 0); got != "1 agent in turn: scanner 12m" {
		t.Fatalf("FormatTurns single = %q", got)
	}
	if got := FormatTurns(nil, 3); got != "no agents in turn" {
		t.Fatalf("FormatTurns empty = %q", got)
	}
	if turns[0].Agent != "scanner" {
		t.Fatal("FormatTurns must not reorder its input")
	}
}

func TestFormatShortDuration(t *testing.T) {
	cases := map[time.Duration]string{-time.Second: "0s", 45 * time.Second: "45s", 38 * time.Minute: "38m", 72 * time.Minute: "1h12m"}
	for d, want := range cases {
		if got := FormatShortDuration(d); got != want {
			t.Fatalf("FormatShortDuration(%s) = %q want %q", d, got, want)
		}
	}
}

func TestTurnsFromNames(t *testing.T) {
	if TurnsFromNames(nil) != nil {
		t.Fatal("nil in, nil out")
	}
	got := TurnsFromNames([]string{"a", "b"})
	if len(got) != 2 || got[1].Agent != "b" || got[1].ElapsedS != 0 {
		t.Fatalf("TurnsFromNames = %+v", got)
	}
	if !(UpgradeAttempt{}).Open() || (UpgradeAttempt{Outcome: UpgradeOutcomeDone}).Open() {
		t.Fatal("Open must key on Outcome")
	}
}

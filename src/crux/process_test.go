package crux

import (
	"reflect"
	"testing"
	"time"
)

func TestProcess_PaneLinesWithoutCapture(t *testing.T) {
	var p Process
	if got := p.PaneLines(10); got != nil {
		t.Fatalf("PaneLines on an empty snapshot = %v, want nil", got)
	}
	// A capture without a filter is unanswerable too: the runtime owns the
	// prompt-aware filtering, and the snapshot never guesses at it.
	p.SetPaneCapture([]string{"a", "b"}, nil)
	if got := p.FilteredPaneLines(1); got != nil {
		t.Fatalf("FilteredPaneLines with no filter = %v, want nil", got)
	}
}

func TestProcess_PaneLinesApplyRuntimeFilter(t *testing.T) {
	var p Process
	var sawN int
	p.SetPaneCapture([]string{"one", "two", "three"}, func(lines []string, n int) []string {
		sawN = n
		if n >= len(lines) {
			return lines
		}
		return lines[len(lines)-n:]
	})

	if got := p.PaneLines(2); !reflect.DeepEqual(got, []string{"two", "three"}) {
		t.Fatalf("PaneLines(2) = %v, want [two three]", got)
	}
	if sawN != 2 {
		t.Fatalf("filter saw n=%d, want 2", sawN)
	}
	if got := p.FilteredPaneLines(10); !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("FilteredPaneLines(10) = %v, want the whole capture", got)
	}
}

// TestProcess_TurnInFlight pins the predicate the dashboard busy flag and the
// reviewer autoscaler share. #18458: a running agent never kicked read as
// mid-task forever, and a never-tasked reviewer replica counted as busy.
func TestProcess_TurnInFlight(t *testing.T) {
	kickAt := time.Now().Add(-time.Minute)
	settled := KickOutcome{Kind: KickOutcomeEnded, At: kickAt.Add(time.Second), KickAt: kickAt}
	cases := []struct {
		name    string
		state   ProcessState
		kick    *time.Time
		outcome KickOutcome
		want    bool
	}{
		{"never kicked", StateRunning, nil, KickOutcome{}, false},
		{"kicked, turn open", StateRunning, &kickAt, KickOutcome{}, true},
		{"kicked, turn settled", StateRunning, &kickAt, settled, false},
		{"kicked, not running", StateStopped, &kickAt, KickOutcome{}, false},
	}
	for _, c := range cases {
		p := Process{State: c.state, LastKick: c.kick, KickOutcome: c.outcome}
		if got := p.TurnInFlight(); got != c.want {
			t.Errorf("%s: TurnInFlight() = %v, want %v", c.name, got, c.want)
		}
	}
}

package watchdog

import (
	"testing"
	"time"
)

// TestClassifyLoginChromeInsideBootGrace pins the fix for a live incident on a
// hosted spoke: rolling the pod paged an operator about quality, scanner and
// ci-maintainer ~3 minutes after launch --
//
//	watchdog: agent credential failure agent=quality backend=copilot
//	reason=PaneShowsLogin detail="pane shows a login/credential screen"
//
// one event each, never repeated. All three were authenticated and working the
// entire time; the CLI had simply painted login chrome while completing its own
// startup auth handshake, which the pane poller already documents and guards
// against with a multi-poll loginStreak before it will act.
//
// Two things had to line up for this to reach a human, so both are covered
// here: ClassAuthRequired was evaluated above the boot-grace guard that every
// dead verdict honours, and the reconciler's "healable" downgrade could not
// rescue it because that requires CredentialProven, which the fleet only sets
// for the claude backend -- so copilot agents always took the paging branch.
func TestClassifyLoginChromeInsideBootGrace(t *testing.T) {
	s := classifySettings()
	loginPane := "Please run /login to authenticate"

	cases := []struct {
		name    string
		since   time.Duration
		backend string
		want    PaneClass
	}{
		{
			name:    "copilot flashing login chrome seconds after launch is not a credential failure",
			since:   3 * time.Second,
			backend: "copilot",
			want:    ClassUnknown,
		},
		{
			name:    "still inside grace just before it expires",
			since:   s.BootGrace - time.Second,
			backend: "copilot",
			want:    ClassUnknown,
		},
		{
			name:    "claude gets the same grace",
			since:   3 * time.Second,
			backend: "claude",
			want:    ClassUnknown,
		},
		{
			name:    "past grace a login screen still pages, exactly as before",
			since:   s.BootGrace + time.Second,
			backend: "copilot",
			want:    ClassAuthRequired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := Observation{
				Backend:       tc.backend,
				SessionExists: true,
				Pane:          loginPane,
				StartedAt:     fresh(tc.since),
				LastChange:    fresh(time.Second),
			}
			got := Classify(obs, testClock, s)
			if got.Class != tc.want {
				t.Errorf("Classify() = %v (%q), want %v", got.Class, got.Reason, tc.want)
			}
		})
	}
}

// TestClassifyLoginChromeWithoutStartedAt keeps the "cannot be dated gets no
// grace" rule intact: an agent with no launch timestamp must not receive an
// unbounded grace window, which would suppress a real credential failure
// forever. This mirrors how the no-session branch already treats a zero
// StartedAt.
func TestClassifyLoginChromeWithoutStartedAt(t *testing.T) {
	obs := Observation{
		Backend:       "copilot",
		SessionExists: true,
		Pane:          "Please run /login to authenticate",
		LastChange:    fresh(time.Second),
	}
	if got := Classify(obs, testClock, classifySettings()); got.Class != ClassAuthRequired {
		t.Errorf("Classify() = %v (%q), want %v", got.Class, got.Reason, ClassAuthRequired)
	}
}

// TestClassifyLoginPromptObservationInsideBootGrace covers the other input
// path: ShowsLoginPrompt is set by the fleet from the pane poller rather than
// read out of Pane text here, and it reached the same paging branch.
func TestClassifyLoginPromptObservationInsideBootGrace(t *testing.T) {
	s := classifySettings()
	obs := Observation{
		Backend:          "copilot",
		SessionExists:    true,
		Pane:             "❯",
		ShowsLoginPrompt: true,
		StartedAt:        fresh(2 * time.Second),
		LastChange:       fresh(time.Second),
	}
	if got := Classify(obs, testClock, s); got.Class != ClassUnknown {
		t.Errorf("inside grace: Classify() = %v (%q), want %v", got.Class, got.Reason, ClassUnknown)
	}

	obs.StartedAt = fresh(s.BootGrace + time.Second)
	if got := Classify(obs, testClock, s); got.Class != ClassAuthRequired {
		t.Errorf("past grace: Classify() = %v (%q), want %v", got.Class, got.Reason, ClassAuthRequired)
	}
}

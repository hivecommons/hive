package agent

import (
	"strings"
	"sync"
	"testing"
)

// conditionObservation is one Observe call the runtime forwarded to its
// ConditionReporter.
type conditionObservation struct {
	Class, Key, Message, Diagnostics string
}

// conditionRecorder is the test ConditionReporter: it keeps every Observe
// and Clear in order so a test can assert exactly what the runtime emitted.
// Threshold and streak semantics belong to the host (pkg/fleetreport) and
// are tested there; here an observation is an observation.
type conditionRecorder struct {
	mu       sync.Mutex
	observed []conditionObservation
	cleared  []conditionObservation
}

func (r *conditionRecorder) Observe(class, key, message, diagnostics string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, conditionObservation{Class: class, Key: key, Message: message, Diagnostics: diagnostics})
}

func (r *conditionRecorder) Clear(class, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleared = append(r.cleared, conditionObservation{Class: class, Key: key})
}

// observations returns every observation of class, in order.
func (r *conditionRecorder) observations(class string) []conditionObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []conditionObservation
	for _, o := range r.observed {
		if o.Class == class {
			out = append(out, o)
		}
	}
	return out
}

// active returns the observations of class whose (class, key) has not been
// cleared since — the recorder's view of what a host would still hold open.
func (r *conditionRecorder) active(class string) []conditionObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	open := map[string]bool{}
	var out []conditionObservation
	for _, o := range r.observed {
		if o.Class == class {
			open[o.Key] = true
		}
	}
	for _, c := range r.cleared {
		if c.Class == class {
			open[c.Key] = false
		}
	}
	for _, o := range r.observed {
		if o.Class == class && open[o.Key] {
			out = append(out, o)
		}
	}
	return out
}

// clears returns every Clear of class, in order.
func (r *conditionRecorder) clears(class string) []conditionObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []conditionObservation
	for _, c := range r.cleared {
		if c.Class == class {
			out = append(out, c)
		}
	}
	return out
}

// recordConditions installs a conditionRecorder on m for the test.
func recordConditions(t *testing.T, m *Manager) *conditionRecorder {
	t.Helper()
	rec := &conditionRecorder{}
	m.SetConditionReporter(rec)
	t.Cleanup(func() { m.SetConditionReporter(nil) })
	return rec
}

func TestConditionReporterNilDropsAndRedacts(t *testing.T) {
	m := &Manager{}
	// Unset: observations and clears are dropped without panicking.
	m.observeCondition("c", "k", "msg", "diag")
	m.clearCondition("c", "k")

	rec := recordConditions(t, m)
	m.observeCondition("c", "k", "token ghp_abcdefghijklmnopqrstuvwxyz123456 in message", "pane: ghp_abcdefghijklmnopqrstuvwxyz123456")
	obs := rec.observations("c")
	if len(obs) != 1 {
		t.Fatalf("observations = %d, want 1", len(obs))
	}
	for _, s := range []string{obs[0].Message, obs[0].Diagnostics} {
		if containsToken := len(s) > 0 && strings.Contains(s, "ghp_abcdefghijklmnopqrstuvwxyz123456"); containsToken {
			t.Fatalf("token reached the reporter: %q", s)
		}
	}
	m.clearCondition("c", "k")
	if len(rec.active("c")) != 0 || len(rec.clears("c")) != 1 {
		t.Fatalf("clear not forwarded: active=%d clears=%d", len(rec.active("c")), len(rec.clears("c")))
	}

	// Setting nil uninstalls.
	m.SetConditionReporter(nil)
	m.observeCondition("c", "k", "again", "")
	if len(rec.observations("c")) != 1 {
		t.Fatal("observation reached an uninstalled reporter")
	}
}

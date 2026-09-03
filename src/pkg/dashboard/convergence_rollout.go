package dashboard

import (
	"sync"

	"github.com/hivecommons/hive/pkg/config"
)

// ── #4263: off/shadow/enforce rollout — captured mode + configuration generation
//
// Every enrolled evaluation pass must judge ALL of its candidates under ONE
// (mode, generation) pair, even when an owner flips the runtime setting midway
// through the pass — the strict acceptance matrix forbids a mixed-mode sweep.
// This tracker is that capture point: the eval-cycle seam calls
// CaptureConvergenceMode exactly once at the start of a pass, uses the returned
// pair for every candidate, and the NEXT pass picks up any change.
//
// The generation is a monotonic, process-local counter that advances only when
// the EFFECTIVE mode actually changes (API PUT, external YAML reload through
// the watcher, or an environment override observed at capture time all funnel
// through the same effective-mode read, so every source is covered uniformly).
// It exists for soak attribution: a fixed-commit comparison must be able to
// tell records from "shadow before the flip to enforce and back" apart from
// "shadow after", even though the mode string is identical.
//
// changed is reported ONLY on a genuine transition — never on the first
// capture after boot — so the caller's one-time notification fires on the
// crossing, not once per process start and never once per cycle (the #4305
// probe-notification discipline).
type convergenceModeTracker struct {
	mu         sync.Mutex
	seen       bool
	mode       string
	generation uint64
}

// capture folds the current effective mode into the tracker, returning the
// pair to use for this pass plus transition facts.
func (t *convergenceModeTracker) capture(effective string) (mode string, generation uint64, changed bool, previous string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.seen {
		t.seen = true
		t.mode = effective
		t.generation = 1
		return t.mode, t.generation, false, ""
	}
	if effective != t.mode {
		previous = t.mode
		t.mode = effective
		t.generation++
		return t.mode, t.generation, true, previous
	}
	return t.mode, t.generation, false, ""
}

// current returns the last captured pair without folding a new observation.
// Before the first capture it reports generation 0 with the given effective
// mode, so read-only surfaces (the settings API, the soak endpoint) never
// perturb the generation the eval loop owns.
func (t *convergenceModeTracker) current(effective string) (string, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.seen {
		return effective, 0
	}
	return t.mode, t.generation
}

// convergenceRollout lazily constructs the tracker so a zero-value Server —
// as used in tests that do `&Server{}` — works without a constructor change,
// exactly like budgetWindows does for the per-window history.
func (s *Server) convergenceRollout() *convergenceModeTracker {
	s.convergenceModeOnce.Do(func() {
		s.convergenceModeTrk = &convergenceModeTracker{}
	})
	return s.convergenceModeTrk
}

// effectiveConvergenceMode reads the current effective mode through the config
// resolution chain (env override, then configured value, then "off").
// Nil-safe: a Server with no config resolves to "off".
func (s *Server) effectiveConvergenceMode() string {
	if s == nil || s.deps == nil {
		return (*config.Config)(nil).ConvergenceMode()
	}
	return s.deps.Config.ConvergenceMode()
}

// CaptureConvergenceMode is the once-per-pass capture point for the enrolled
// eval-cycle seam (#4247's kick boundary). The caller supplies the effective
// mode it resolved from the live config (cfg.ConvergenceMode()), so the seam
// and the tracker cannot disagree about which configuration object is
// authoritative. Every candidate in the pass that calls this MUST use the
// returned pair; a concurrent settings change becomes effective on the next
// pass.
func (s *Server) CaptureConvergenceMode(effective string) (mode string, generation uint64, changed bool, previous string) {
	if _, ok := config.NormalizeConvergenceMode(effective); !ok {
		effective = config.ConvergenceModeOff
	}
	return s.convergenceRollout().capture(effective)
}

// ConvergenceModeGeneration reports the last captured (mode, generation) pair
// for read-only surfaces, without advancing anything.
func (s *Server) ConvergenceModeGeneration() (string, uint64) {
	return s.convergenceRollout().current(s.effectiveConvergenceMode())
}

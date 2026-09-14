// Package contributorquota evaluates contributor-local subscription quota
// headroom before a relay accepts new work.
package contributorquota

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/rotation"
)

const (
	ModeAsk   = "ask"
	ModePause = "pause"
	ModeOff   = "off"

	StateAvailable = "available"
	StateGuarded   = "guarded"
	StateUnknown   = "unknown"
	StateStale     = "stale"

	// StateGuardedUnknownWindow distinguishes a refusal caused by a window
	// kind this build does not recognize, so an operator can tell "your quota
	// is low" apart from "the provider reported a limit we have not taught the
	// guard about yet".
	StateGuardedUnknownWindow = "guarded_unknown_window"

	TierSimple  = "simple"
	TierMedium  = "medium"
	TierComplex = "complex"
	TierUnknown = "unknown"
)

// Config is the contributor-owned reserve policy. Nil override pointers inherit
// the lower-precedence reserve rather than meaning zero.
type Config struct {
	Mode             string
	BaseReservePct   int
	ShortReservePct  *int
	WeeklyReservePct *int
	TierReservePct   map[string]*int
}

// EnvSource is a tiny seam so startup parsing and tests use the same validation.
type EnvSource func(string) (string, bool)

// ParseConfig reads HIVE_CONTRIBUTOR_QUOTA_* settings and validates every
// configured percentage. Invalid values are startup errors because silently
// ignoring a bad reserve would fail open.
func ParseConfig(env EnvSource) (Config, error) {
	if env == nil {
		env = func(string) (string, bool) { return "", false }
	}
	mode := ModeAsk
	if raw, ok := env("HIVE_CONTRIBUTOR_QUOTA_GUARD"); ok && strings.TrimSpace(raw) != "" {
		mode = strings.ToLower(strings.TrimSpace(raw))
	}
	switch mode {
	case ModeAsk, ModePause, ModeOff:
	default:
		return Config{}, fmt.Errorf("HIVE_CONTRIBUTOR_QUOTA_GUARD must be ask, pause, or off (got %q)", mode)
	}
	base, err := parsePct(env, "HIVE_CONTRIBUTOR_QUOTA_MIN_REMAINING_PCT", 20)
	if err != nil {
		return Config{}, err
	}
	short, err := parseOptionalPct(env, "HIVE_CONTRIBUTOR_QUOTA_SHORT_MIN_REMAINING_PCT")
	if err != nil {
		return Config{}, err
	}
	weekly, err := parseOptionalPct(env, "HIVE_CONTRIBUTOR_QUOTA_WEEKLY_MIN_REMAINING_PCT")
	if err != nil {
		return Config{}, err
	}
	tiers := make(map[string]*int, 4)
	for tier, name := range map[string]string{
		TierSimple:  "HIVE_CONTRIBUTOR_QUOTA_SIMPLE_MIN_REMAINING_PCT",
		TierMedium:  "HIVE_CONTRIBUTOR_QUOTA_MEDIUM_MIN_REMAINING_PCT",
		TierComplex: "HIVE_CONTRIBUTOR_QUOTA_COMPLEX_MIN_REMAINING_PCT",
		TierUnknown: "HIVE_CONTRIBUTOR_QUOTA_UNKNOWN_MIN_REMAINING_PCT",
	} {
		pct, err := parseOptionalPct(env, name)
		if err != nil {
			return Config{}, err
		}
		tiers[tier] = pct
	}
	return Config{Mode: mode, BaseReservePct: base, ShortReservePct: short, WeeklyReservePct: weekly, TierReservePct: tiers}, nil
}

func parsePct(env EnvSource, name string, def int) (int, error) {
	if raw, ok := env(name); ok && strings.TrimSpace(raw) != "" {
		pct, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || pct < 0 || pct > 100 {
			return 0, fmt.Errorf("%s must be a percentage from 0 to 100 (got %q)", name, raw)
		}
		return pct, nil
	}
	return def, nil
}

func parseOptionalPct(env EnvSource, name string) (*int, error) {
	if raw, ok := env(name); ok && strings.TrimSpace(raw) != "" {
		pct, err := parsePct(env, name, 0)
		if err != nil {
			return nil, err
		}
		return &pct, nil
	}
	return nil, nil
}

// Reading is the contributor-local subscription quota source. Limits reuse
// rotation.LimitWindow, the normalized RFC #5698 window model used by
// GET /api/providers/headroom.
type Reading struct {
	Provider             string
	QuotaPoolID          string
	ObservedAt           time.Time
	Source               string
	State                string
	Limits               []rotation.LimitWindow
	PaidCreditsAvailable bool
}

// Decision is the result of evaluating one task admission.
type Decision struct {
	Admit     bool
	Wait      bool
	Reason    string
	WindowID  string
	Required  int
	Remaining int
}

func (c Config) EffectiveWindowReserve(kind string) int {
	reserve := c.BaseReservePct
	switch kind {
	case "session", "short", "five_hour":
		if c.ShortReservePct != nil {
			reserve = *c.ShortReservePct
		}
	case "weekly", "weekly_scoped":
		if c.WeeklyReservePct != nil {
			reserve = *c.WeeklyReservePct
		}
	}
	return reserve
}

// RequiredReserve implements issue #6833's max(window reserve, task tier reserve)
// rule. Missing tier settings inherit the base reserve.
func (c Config) RequiredReserve(window rotation.LimitWindow, tier string) int {
	reserve := c.EffectiveWindowReserve(window.Kind)
	tier = normalizeTier(tier)
	if c.TierReservePct != nil {
		if pct, ok := c.TierReservePct[tier]; ok && pct != nil && *pct > reserve {
			reserve = *pct
		}
	}
	return reserve
}

func normalizeTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case TierSimple:
		return TierSimple
	case TierMedium:
		return TierMedium
	case TierComplex:
		return TierComplex
	default:
		return TierUnknown
	}
}

// Evaluate applies the admission rule: off disables the guard, unknown/stale
// waits safely, and any recognized window at or below the required reserve
// refuses work. Equality is intentionally refused.
func (c Config) Evaluate(r Reading, tier string) Decision {
	if c.Mode == ModeOff {
		return Decision{Admit: true}
	}
	switch r.State {
	case StateUnknown, StateStale:
		return Decision{Wait: true, Reason: r.State}
	}
	for _, w := range r.Limits {
		required := c.RequiredReserve(w, tier)
		if w.PctRemaining <= required {
			reason := StateGuarded
			if !recognizedWindow(w.Kind) {
				// An unrecognized window is still a real limit. Skipping it
				// would fail open exactly when a provider introduces a new
				// window kind - which is how "weekly_scoped" arrived - so it
				// is evaluated against the base reserve instead.
				reason = StateGuardedUnknownWindow
			}
			return Decision{Wait: true, Reason: reason, WindowID: w.ID, Required: required, Remaining: w.PctRemaining}
		}
	}
	return Decision{Admit: true}
}

func recognizedWindow(kind string) bool {
	switch kind {
	case "session", "short", "five_hour", "weekly", "weekly_scoped":
		return true
	default:
		return false
	}
}

// Guard keeps between-task pause state. It never interrupts an in-flight task;
// readings that cross the threshold while active only affect the next admission.
type Guard struct {
	Config
	reading      Reading
	paused       bool
	stayPaused   bool
	continueOnce bool
	activeTask   bool
	lastDecision Decision
}

func NewGuard(cfg Config) *Guard { return &Guard{Config: cfg} }

func (g *Guard) StartTask()    { g.activeTask = true }
func (g *Guard) FinishTask()   { g.activeTask = false; g.continueOnce = false }
func (g *Guard) StayPaused()   { g.paused = true; g.stayPaused = true }
func (g *Guard) ContinueOnce() { g.continueOnce = true; g.paused = false }
func (g *Guard) Paused() bool  { return g.paused }

func (g *Guard) Update(r Reading, tier string) Decision {
	g.reading = r
	d := g.Config.Evaluate(r, tier)
	g.lastDecision = d
	if g.activeTask {
		return Decision{Admit: true, Reason: "active_task_not_interrupted"}
	}
	if d.Admit {
		if !g.stayPaused {
			g.paused = false
			return d
		}
		return Decision{Wait: true, Reason: "explicit_pause"}
	}
	g.paused = true
	return d
}

func (g *Guard) Admit(tier string) Decision {
	if g.Config.Mode == ModeOff || g.continueOnce {
		return Decision{Admit: true}
	}
	if g.stayPaused {
		return Decision{Wait: true, Reason: "explicit_pause"}
	}
	return g.Update(g.reading, tier)
}

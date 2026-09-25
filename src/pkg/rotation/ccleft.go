package rotation

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	ccleft "github.com/tuna-os/ccleft"
)

const defaultCCLeftThresholdPct = 85

type ccleftGetter interface {
	Get(context.Context, ccleft.Source) ccleft.Reading
}

type ccleftHeadroomSource struct {
	provider     string
	thresholdPct int
	source       ccleft.Source
	client       ccleftGetter
}

func (s ccleftHeadroomSource) Provider() string { return s.provider }

func (s ccleftHeadroomSource) Probe(ctx context.Context) Headroom {
	client := s.client
	if client == nil {
		client = ccleft.NewClient(nil)
	}
	return ccleftReadingToHeadroom(s.provider, s.thresholdPct, client.Get(ctx, s.source))
}

func newCCLeftHeadroomSource(provider string, thresholdPct int) (HeadroomSource, bool) {
	cp, ok := ccleftProviderForHive(provider)
	if !ok {
		return nil, false
	}
	return ccleftHeadroomSource{
		provider:     provider,
		thresholdPct: thresholdPct,
		source: ccleft.Source{
			Provider: cp,
			Home:     sharedCLIHomePath,
			Env:      ccleftEnv(),
		},
		client: ccleft.NewClient(nil),
	}, true
}

func ccleftProviderForHive(provider string) (ccleft.Provider, bool) {
	switch provider {
	case "anthropic":
		return ccleft.Claude, true
	case "openai":
		return ccleft.Codex, true
	case "google":
		return ccleft.Agy, true
	case "github":
		return ccleft.Copilot, true
	case "deepseek":
		return ccleft.DeepSeek, true
	case "aws-kiro":
		return ccleft.Kiro, true
	default:
		return "", false
	}
}

func ccleftEnv() map[string]string {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			env[k] = v
		}
	}
	return env
}

func ccleftReadingToHeadroom(provider string, thresholdPct int, r ccleft.Reading) Headroom {
	h := Headroom{
		Provider:   provider,
		CapturedAt: r.FetchedAt,
		Stale:      r.Stale,
		PlanType:   r.Plan,
		Limits:     ccleftWindowsToLimits(r.Windows),
	}
	if h.CapturedAt.IsZero() {
		h.CapturedAt = time.Now().UTC()
	}
	remaining, reset, ok := ccleftBindingRemaining(r)
	if ok {
		h.PctRemaining = remaining
		h.ResetAt = reset
	} else if r.State == ccleft.StateOK {
		h.PctRemaining = fullPct
	}

	switch r.State {
	case ccleft.StateOK:
		h.Available = remainingAvailable(remaining, ok, thresholdPct)
	case ccleft.StateLimited, ccleft.StateExhausted:
		h.Available = false
		if !ok {
			h.PctRemaining = 0
		}
	default:
		h.Available = true
		h.ProbeErrCause = ccleftProbeCause(r)
		h.ProbeErr = withCause(h.ProbeErrCause, ccleftProbeError(r))
	}
	if r.Stale {
		cause := ccleftProbeCause(r)
		if cause == ProbeCauseUnspecified {
			cause = ProbeCauseProbeFailed
		}
		h.ProbeErrCause = cause
		h.ProbeErr = withCause(cause, ccleftProbeError(r))
	}
	return h
}

func remainingAvailable(remaining int, hasRemaining bool, thresholdPct int) bool {
	if !hasRemaining {
		return true
	}
	threshold := thresholdPct
	if threshold <= 0 {
		threshold = defaultCCLeftThresholdPct
	}
	return fullPct-remaining < threshold
}

func ccleftWindowsToLimits(windows []ccleft.Window) []LimitWindow {
	limits := make([]LimitWindow, 0, len(windows))
	for _, w := range windows {
		used, remaining := ccleftWindowPercents(w)
		lw := LimitWindow{
			ID:           w.ID,
			Kind:         string(w.Kind),
			PercentUsed:  used,
			PctRemaining: remaining,
			DurationMins: ccleftKindDurationMins(w.Kind),
		}
		if w.ResetsAt != nil {
			lw.ResetAt = *w.ResetsAt
		}
		if w.Scope != "" || w.Unit != "" || !w.Binding {
			lw.Scope = map[string]string{}
			if w.Scope != "" {
				lw.Scope["scope"] = w.Scope
			}
			if w.Unit != "" {
				lw.Scope["unit"] = w.Unit
			}
			if !w.Binding {
				lw.Scope["binding"] = "false"
			}
		}
		limits = append(limits, lw)
	}
	return limits
}

func ccleftWindowPercents(w ccleft.Window) (used, remaining int) {
	if w.UsedPct != nil {
		used = roundedPct(*w.UsedPct)
	} else if w.RemainingPct != nil {
		used = fullPct - roundedPct(*w.RemainingPct)
	} else if w.Kind == ccleft.KindBalance || w.Kind == ccleft.KindCredits {
		if w.Remaining != nil && *w.Remaining > 0 {
			used = 0
		} else {
			used = fullPct
		}
	}
	if w.RemainingPct != nil {
		remaining = roundedPct(*w.RemainingPct)
	} else {
		remaining = fullPct - used
	}
	return used, remaining
}

func ccleftBindingRemaining(r ccleft.Reading) (int, time.Time, bool) {
	var reset time.Time
	found := false
	bestRemaining := fullPct
	for _, w := range r.Windows {
		if !w.Binding {
			continue
		}
		_, rem := ccleftWindowPercents(w)
		if !found || rem < bestRemaining {
			found = true
			bestRemaining = rem
			reset = time.Time{}
			if w.ResetsAt != nil {
				reset = *w.ResetsAt
			}
		}
	}
	if found {
		return bestRemaining, reset, true
	}
	if r.State == ccleft.StateOK || r.State == ccleft.StateLimited || r.State == ccleft.StateExhausted {
		return stateOnlyRemaining(r.State), time.Time{}, true
	}
	return 0, time.Time{}, false
}

func stateOnlyRemaining(state ccleft.State) int {
	if state == ccleft.StateOK {
		return fullPct
	}
	return 0
}

func roundedPct(v float64) int {
	pct := int(math.Round(v))
	if pct < 0 {
		return 0
	}
	if pct > fullPct {
		return fullPct
	}
	return pct
}

func ccleftKindDurationMins(kind ccleft.Kind) int {
	switch kind {
	case ccleft.KindFiveHour:
		return 300
	case ccleft.KindDaily:
		return 1440
	case ccleft.KindWeekly:
		return 10080
	default:
		return 0
	}
}

func ccleftProbeCause(r ccleft.Reading) ProbeErrorCause {
	switch r.State {
	case ccleft.StateRateLimited:
		return ProbeCauseRateLimited
	case ccleft.StateAuthRequired:
		return ProbeCauseNoCredentials
	case ccleft.StateUnsupported:
		return ProbeCauseNoCredentials
	}
	switch r.Cause {
	case "not_installed":
		return ProbeCauseNotInstalled
	case "no_credentials", "login_required", "token_expired", "login_expired", "api_key_login":
		return ProbeCauseNoCredentials
	case "schema":
		return ProbeCauseUnrecognizedSchema
	case "timeout":
		return ProbeCauseTimeout
	case "http_429", "throttled":
		return ProbeCauseRateLimited
	default:
		if strings.Contains(r.Cause, "429") {
			return ProbeCauseRateLimited
		}
		if strings.Contains(r.Cause, "schema") {
			return ProbeCauseUnrecognizedSchema
		}
		if strings.Contains(r.Cause, "timeout") {
			return ProbeCauseTimeout
		}
	}
	return ProbeCauseProbeFailed
}

func ccleftProbeError(r ccleft.Reading) error {
	if err := r.Err(); err != nil {
		return err
	}
	msg := strings.TrimSpace(r.Message)
	if msg == "" {
		msg = fmt.Sprintf("ccleft %s reading state %s", r.Provider, r.State)
		if r.Cause != "" {
			msg += " (" + r.Cause + ")"
		}
	}
	return fmt.Errorf("%s", msg)
}

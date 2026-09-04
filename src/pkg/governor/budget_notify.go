package governor

import (
	"sync"
	"time"
)

// ProviderBudgetNotifyState is the one-shot guard for provider spend-rebuff
// notifications. It records transition crossings, not every cycle where the
// provider remains clipped.
type ProviderBudgetNotifyState struct {
	mu sync.Mutex
	// notifiedSince is the latch time already notified about; zero when the
	// provider is serving or the current latch has not been notified yet.
	notifiedSince time.Time
}

// ShouldSend reports whether this latch still owes the operator a notification,
// and records that it has been sent. Returns true at most once per latch.
func (p *ProviderBudgetNotifyState) ShouldSend(since time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.notifiedSince.IsZero() && p.notifiedSince.Equal(since) {
		return false
	}
	p.notifiedSince = since
	return true
}

// Reset forgets the notified latch so the next clip pages again, and reports
// whether a notified latch was in force. A true return means this cycle is the
// recovery crossing and owes the operator a "serving again" notification.
func (p *ProviderBudgetNotifyState) Reset() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	wasNotified := !p.notifiedSince.IsZero()
	p.notifiedSince = time.Time{}
	return wasNotified
}

// ProviderBudgetSuppresses reports whether a latched spend rebuff should
// withhold this cycle's kicks, or whether the cycle is a probe that must be let
// through.
func ProviderBudgetSuppresses(latched bool, lastRebuff, now time.Time, probeInterval time.Duration) bool {
	if !latched {
		return false
	}
	// A latched signal with no stamp probes immediately rather than suppressing
	// indefinitely: spending one run is recoverable, muting the hive forever is
	// not.
	if lastRebuff.IsZero() {
		return false
	}
	return now.Sub(lastRebuff) < probeInterval
}

// ProviderBudgetProbeState remembers when the last probe kick was released, so
// exactly one probe flies per interval while a provider spend rebuff is latched.
type ProviderBudgetProbeState struct {
	mu sync.Mutex
	// lastProbe is when a probe kick was last released; zero when the provider
	// is serving or no probe has flown for the current latch.
	lastProbe time.Time
}

// Freshest returns the later of the last observed rebuff and the last released
// probe — the stamp suppression freshness is measured from.
func (p *ProviderBudgetProbeState) Freshest(lastRebuff time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastProbe.After(lastRebuff) {
		return p.lastProbe
	}
	return lastRebuff
}

// MarkReleased records that a probe kick actually went out this cycle.
func (p *ProviderBudgetProbeState) MarkReleased(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastProbe = now
}

// Reset forgets the probe stamp. Called on every cycle where the provider is
// serving, so a new latch starts its probe clock from its own rebuffs.
func (p *ProviderBudgetProbeState) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastProbe = time.Time{}
}

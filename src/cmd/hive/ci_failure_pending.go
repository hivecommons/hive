package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	ciFailureKickImmediateAttempts = 3
	ciFailureKickMaxAttempts       = 6
	ciFailureKickMaxPending        = 128
)

type ciFailurePendingKey struct {
	repo, check string
	number      int
}

type ciFailurePending struct {
	key      ciFailurePendingKey
	sig      github.CIFailureSignal
	message  string
	attempts int
	next     time.Time
	inFlight bool
}

func (k *ciFailureKicker) reserve(sig github.CIFailureSignal, message string) (*ciFailurePending, string) {
	key := ciFailurePendingKey{strings.ToLower(strings.TrimSpace(sig.Repo)), strings.TrimSpace(sig.Check), sig.Number}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending == nil {
		k.pending = make(map[ciFailurePendingKey]*ciFailurePending)
	}
	if prev := k.pending[key]; prev != nil && strings.EqualFold(prev.sig.HeadSHA, sig.HeadSHA) {
		return nil, "pending"
	}
	if k.pending[key] == nil && len(k.pending) >= ciFailureKickMaxPending {
		return nil, "pending queue full"
	}
	// A newer head replaces the old pending signal without growing the queue.
	// The old worker checks pointer identity before attempting another send.
	p := &ciFailurePending{key: key, sig: sig, message: message, inFlight: true}
	k.pending[key] = p
	return p, ""
}

func (k *ciFailureKicker) remove(p *ciFailurePending) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending[p.key] == p {
		delete(k.pending, p.key)
	}
}

func (k *ciFailureKicker) clock() time.Time {
	if k.now != nil {
		return k.now()
	}
	return time.Now()
}

// Drain retries pending kicks once at the governor's next evaluation. It does
// no delivery IO on the evaluation goroutine and never runs a second worker
// for an entry already being delivered. Retries bypass the webhook cooldown:
// that cooldown was consumed by the original dispatch, not by a delivery.
func (k *ciFailureKicker) Drain() {
	if k == nil {
		return
	}
	now := k.clock()
	k.mu.Lock()
	var due []*ciFailurePending
	for key, p := range k.pending {
		if !ciFailureStillPending(p.sig) {
			delete(k.pending, key)
			continue
		}
		if !p.inFlight && !now.Before(p.next) {
			p.inFlight = true
			due = append(due, p)
		}
	}
	k.mu.Unlock()
	for _, p := range due {
		k.dispatch(p, 1)
	}
}

func ciFailureStillPending(sig github.CIFailureSignal) bool {
	for _, current := range github.CIFailureSignals().ForPR(sig.Repo, sig.Number) {
		if strings.EqualFold(current.HeadSHA, sig.HeadSHA) && strings.TrimSpace(current.Check) == strings.TrimSpace(sig.Check) {
			return true
		}
	}
	return false
}

func ciFailureKickBackoff(attempts int) time.Duration {
	return time.Second << (attempts - 1)
}

func (k *ciFailureKicker) dispatch(p *ciFailurePending, limit int) {
	spawn := k.spawn
	if spawn == nil {
		spawn = func(fn func()) { go fn() }
	}
	spawn(func() { k.deliver(p, limit) })
}

func (k *ciFailureKicker) deliver(p *ciFailurePending, limit int) {
	parent := k.ctx
	if parent == nil {
		parent = context.Background()
	}
	logger := k.logger
	if logger == nil {
		logger = slog.Default()
	}
	for i := 0; i < limit; i++ {
		k.mu.Lock()
		current := k.pending[p.key] == p
		k.mu.Unlock()
		if !current {
			return
		}
		if parent.Err() != nil || (p.attempts > 0 && !ciFailureStillPending(p.sig)) {
			k.remove(p)
			return
		}
		if k.gate(p.sig, p.sig.Agent) != "" {
			break
		}
		message := p.message
		if p.attempts > 0 {
			message = k.message(p.sig.Agent, p.sig)
		}
		if strings.TrimSpace(message) == "" {
			k.remove(p)
			return
		}
		p.attempts++
		// Bound each send separately: a busy runtime may spend the full
		// prompt timeout before returning ErrAgentBusy. That must still
		// leave room for retries unless the process context was cancelled.
		ctx, cancel := context.WithTimeout(parent, ciFailureKickSendTimeout)
		err := k.send(ctx, p.sig.Agent, message)
		cancel()
		if err == nil {
			k.remove(p)
			if gov := k.governor(); gov != nil {
				gov.RecordKickForRepo(p.sig.Agent, p.sig.Repo)
			}
			logger.Info("ci failure webhook kick delivered", "agent", p.sig.Agent, "repo", p.sig.Repo, "number", p.sig.Number, "check", p.sig.Check, "attempts", p.attempts)
			return
		}
		if parent.Err() != nil {
			k.remove(p)
			return
		}
		if !errors.Is(err, crux.ErrAgentBusy) || p.attempts >= ciFailureKickMaxAttempts {
			k.remove(p)
			logger.Warn("ci failure webhook kick failed", "agent", p.sig.Agent, "repo", p.sig.Repo, "number", p.sig.Number, "check", p.sig.Check, "attempts", p.attempts, "error", err)
			return
		}
		if i+1 < limit && !k.waitBackoff(parent, ciFailureKickBackoff(p.attempts)) {
			k.remove(p)
			return
		}
	}
	k.mu.Lock()
	p.inFlight = false
	p.next = k.clock().Add(ciFailureKickBackoff(max(1, p.attempts)))
	k.mu.Unlock()
}

func (k *ciFailureKicker) waitBackoff(ctx context.Context, delay time.Duration) bool {
	if k.wait != nil {
		return k.wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

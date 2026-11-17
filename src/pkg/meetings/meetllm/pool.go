package meetllm

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CLIPool is one meeting's set of warm completion CLIs, so the persona can
// run several turns at once (hivecommons/hive#18590). A warm CLI answers
// one turn at a time on one stdin/stdout stream (its result events carry no
// request id to tell turns apart), so concurrency comes from more
// processes, never from interleaving turns on one.
//
// The first CLI is warmed when the meeting starts. The others start only
// when the meeting actually overlaps requests: a call that takes the last
// idle warm CLI while another call is in flight warms the next one in the
// background, so a burst of questions pays at most one cold start and the
// rest of the meeting stays warm. Memory is bounded by Size processes.
type CLIPool struct {
	cfg Config
	// sem holds one place per call in the pool; a holder always finds an
	// idle session (or room to start one).
	sem chan struct{}

	mu       sync.Mutex
	sessions []*CLISession
	closed   bool
}

// NewCLIPool builds a pool of up to n warm CLIs for c.Backend; n <= 0
// means one. Warm starts the first.
func NewCLIPool(c Config, n int) (*CLIPool, error) {
	if n <= 0 {
		n = 1
	}
	first, err := NewCLISession(c)
	if err != nil {
		return nil, err
	}
	return &CLIPool{cfg: c, sem: make(chan struct{}, n), sessions: []*CLISession{first}}, nil
}

// Size is the most CLIs the pool runs.
func (p *CLIPool) Size() int { return cap(p.sem) }

// Warm starts the first CLI if it is not running.
func (p *CLIPool) Warm() error {
	p.mu.Lock()
	s := p.sessions[0]
	p.mu.Unlock()
	return s.Warm()
}

// Started is how many of the pool's CLIs have a process now.
func (p *CLIPool) Started() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.sessions {
		if s.running() {
			n++
		}
	}
	return n
}

// Close stops every CLI; later turns fail with ErrSessionClosed.
func (p *CLIPool) Close() error {
	p.mu.Lock()
	p.closed = true
	sessions := append([]*CLISession(nil), p.sessions...)
	p.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
	return nil
}

// take waits for a place, then returns an idle session holding its turn
// lock: a warm one when there is one, else an idle cold one, else a new
// session. Release it with give.
func (p *CLIPool) take(ctx context.Context) (*CLISession, error) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("meeting-llm: CLI pool busy: %w", ctx.Err())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		<-p.sem
		return nil, ErrSessionClosed
	}
	s := p.idleLocked(true)
	if s == nil {
		s = p.idleLocked(false)
	}
	if s == nil {
		// The place guarantees room: every existing session is held.
		var err error
		if s, err = p.addLocked(); err != nil {
			<-p.sem
			return nil, err
		}
		s.tryLock()
	}
	p.warmAheadLocked()
	return s, nil
}

// give returns a session taken with take.
func (p *CLIPool) give(s *CLISession) {
	s.unlock()
	<-p.sem
}

// idleLocked locks and returns an idle session, warm ones only when warm.
func (p *CLIPool) idleLocked(warm bool) *CLISession {
	for _, s := range p.sessions {
		if warm && !s.running() {
			continue
		}
		if s.tryLock() {
			return s
		}
	}
	return nil
}

func (p *CLIPool) addLocked() (*CLISession, error) {
	s, err := NewCLISession(p.cfg)
	if err != nil {
		return nil, err
	}
	p.sessions = append(p.sessions, s)
	return s, nil
}

// warmAheadLocked starts one more CLI in the background when calls overlap
// and no idle warm CLI is left for the next one.
func (p *CLIPool) warmAheadLocked() {
	if len(p.sem) < 2 || len(p.sessions) >= cap(p.sem) {
		return
	}
	for _, s := range p.sessions {
		if s.running() && s.idle() {
			return
		}
	}
	s, err := p.addLocked()
	if err != nil {
		return
	}
	go func() { _ = s.Warm() }()
}

// completePool runs one call on an idle CLI of the meeting's pool under the
// hive-wide in-flight limit and records its usage. The pool place is taken
// before the slot, so a call queued behind its own meeting holds no slot.
func completePool(ctx context.Context, c Config, prompt string) (string, error) {
	s, err := c.Pool.take(ctx)
	if err != nil {
		return "", err
	}
	defer c.Pool.give(s)
	release, err := acquire(ctx, c.SlotsDir, c.MaxInFlight)
	if err != nil {
		return "", err
	}
	defer release()
	reply, tokens, err := s.turnLocked(ctx, c.System, prompt)
	if uerr := c.charge(tokens, time.Now()); uerr != nil && err == nil {
		err = fmt.Errorf("meeting-llm: recording usage: %w", uerr)
	}
	return reply, err
}

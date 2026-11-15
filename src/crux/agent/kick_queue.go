package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
)

// Queued, ID-addressed kicks (#18285).
//
// SendKickAsync allows exactly one in-flight kick per agent and refuses the
// rest; that is the right contract for an operator's dashboard click, and it
// is unchanged. A caller that has a stream of work for an agent (the meetings
// research pool is the first) instead needs to hand the runtime a kick, get
// an ID back, and later ask "was it delivered, has its turn ended?". EnqueueKick
// gives it a bounded FIFO per agent that delivers each kick once the agent is
// idle, and KickStatus answers by ID.

const (
	// kickQueueSizeEnv overrides defaultKickQueueSize: the most kicks that may
	// wait (not yet delivered) for a single agent.
	kickQueueSizeEnv     = "HIVE_KICK_QUEUE_SIZE"
	defaultKickQueueSize = 8

	// kickQueueWaitTimeoutEnv overrides defaultKickQueueWaitTimeout: how long a
	// queued kick may wait for its agent to become idle before it fails.
	kickQueueWaitTimeoutEnv     = "HIVE_KICK_QUEUE_WAIT_TIMEOUT"
	defaultKickQueueWaitTimeout = 30 * time.Minute

	// kickQueueTurnTimeoutEnv overrides defaultKickQueueTurnTimeout: how long a
	// delivered kick's turn may run before the queue stops waiting for it to
	// end. A turn the runtime never classifies (the CLI was restarted under
	// it, the pane never changed) would otherwise wedge the queue forever.
	kickQueueTurnTimeoutEnv     = "HIVE_KICK_QUEUE_TURN_TIMEOUT"
	defaultKickQueueTurnTimeout = 60 * time.Minute

	// defaultKickQueuePollInterval is how often a waiting queue re-checks
	// whether its agent became idle.
	defaultKickQueuePollInterval = 2 * time.Second

	// maxRetainedKickStatuses bounds the terminal statuses kept for
	// KickStatus lookups. The oldest terminal entries are evicted first;
	// non-terminal entries are already bounded by the queue size.
	maxRetainedKickStatuses = 1024

	// maxKickIDLen bounds a caller-supplied kick ID.
	maxKickIDLen = 128
	// generatedKickIDBytes is the random part of a generated kick ID.
	generatedKickIDBytes = 8
	// generatedKickIDPrefix marks a runtime-generated kick ID.
	generatedKickIDPrefix = "k-"

	kickQueueErrWaitTimeout = "agent did not become idle within %s"
	kickQueueErrTurnTimeout = "turn did not end within %s"
)

var kickIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// Compile-time proof the in-process runtime offers queued kicks.
var _ crux.KickQueuer = (*Manager)(nil)

// queuedKick is one entry in an agent's FIFO, and after delivery the record
// of the in-flight turn.
type queuedKick struct {
	id      string
	agent   string
	message string
	// kickAt is the agent's LastKick right after this kick was delivered; the
	// turn that ends with an outcome for kickAt (or later) completes it.
	kickAt time.Time
}

// kickQueue is the per-agent FIFO plus the by-ID status table. Its own mutex,
// never m.mu: the pump reads manager state under m.mu separately and the
// completion hook runs from notifyKickOutcome with m.mu held.
type kickQueue struct {
	mu       sync.Mutex
	pending  map[string][]*queuedKick
	pumping  map[string]bool
	inflight map[string]*queuedKick
	statuses map[string]*crux.KickStatus
	order    []string
	// emit fans a lifecycle event out to the manager's kick observers. Set
	// on first enqueue; called with q.mu held, and never takes q.mu itself.
	emit func(crux.KickEvent)
}

func (q *kickQueue) initLocked() {
	if q.pending == nil {
		q.pending = make(map[string][]*queuedKick)
		q.pumping = make(map[string]bool)
		q.inflight = make(map[string]*queuedKick)
		q.statuses = make(map[string]*crux.KickStatus)
	}
}

// recordLocked stores a new status and evicts the oldest terminal ones over
// the retention cap.
func (q *kickQueue) recordLocked(st *crux.KickStatus) {
	q.statuses[st.ID] = st
	q.order = append(q.order, st.ID)
	if len(q.order) <= maxRetainedKickStatuses {
		return
	}
	kept := q.order[:0]
	excess := len(q.order) - maxRetainedKickStatuses
	for _, id := range q.order {
		if excess > 0 {
			if s, ok := q.statuses[id]; ok && s.Terminal() {
				delete(q.statuses, id)
				excess--
				continue
			}
		}
		kept = append(kept, id)
	}
	q.order = kept
}

// settleLocked moves a status to a terminal phase and, when it did, fans the
// matching kick-completed / kick-failed event out to the observers (#18286).
// emit may be nil (bare queue in tests).
func (q *kickQueue) settleLocked(id, phase, errMsg, outcome string, now time.Time) {
	st, ok := q.statuses[id]
	if !ok || st.Terminal() {
		return
	}
	st.Phase = phase
	st.Error = errMsg
	st.Outcome = outcome
	st.SettledAt = now
	if q.emit == nil {
		return
	}
	ev := crux.KickEvent{Agent: st.Agent, Event: crux.KickEventCompleted, Detail: outcome, KickID: id, At: now}
	if phase == crux.KickPhaseFailed {
		ev.Event, ev.Detail = crux.KickEventFailed, errMsg
	}
	q.emit(ev)
}

// turnEnded completes the agent's in-flight queued kick when the outcome
// belongs to its turn (or a later one). Safe to call with m.mu held.
func (q *kickQueue) turnEnded(agent string, outcome KickOutcome, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	item, ok := q.inflight[agent]
	if !ok || outcome.KickAt.Before(item.kickAt) {
		return
	}
	delete(q.inflight, agent)
	q.settleLocked(item.id, crux.KickPhaseCompleted, "", outcome.Kind, now)
}

// EnqueueKick appends a kick to the agent's bounded FIFO and returns its kick
// ID; see crux.KickQueuer. kickID may be empty to have one generated. It
// fails synchronously for an unknown agent, one the operator paused or
// stopped (it would never become idle on its own), an invalid or duplicate
// kickID, and a full queue (crux.ErrKickQueueFull).
func (m *Manager) EnqueueKick(name, kickID, message string) (string, error) {
	m.mu.RLock()
	agent, ok := m.agents[name]
	var reason string
	if ok && (agent.Paused || agent.State == StatePaused || agent.OperatorStopped) {
		reason = notRunningReason(agent)
	}
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("agent %s not found", name)
	}
	if reason != "" {
		return "", fmt.Errorf("agent %s cannot be kicked: %s", name, reason)
	}

	if kickID == "" {
		id, err := newKickID()
		if err != nil {
			return "", err
		}
		kickID = id
	} else if len(kickID) > maxKickIDLen || !kickIDPattern.MatchString(kickID) {
		return "", fmt.Errorf("%w: %q", crux.ErrInvalidKickID, kickID)
	}

	q := &m.kickQueue
	q.mu.Lock()
	q.initLocked()
	if q.emit == nil {
		q.emit = m.emitKickEvent
	}
	if _, dup := q.statuses[kickID]; dup {
		q.mu.Unlock()
		return "", fmt.Errorf("%w: %s", crux.ErrKickIDInUse, kickID)
	}
	if size := kickQueueSize(); len(q.pending[name]) >= size {
		q.mu.Unlock()
		return "", fmt.Errorf("%w: agent %s has %d kicks waiting", crux.ErrKickQueueFull, name, size)
	}
	q.pending[name] = append(q.pending[name], &queuedKick{id: kickID, agent: name, message: message})
	queuedAt := time.Now()
	q.recordLocked(&crux.KickStatus{ID: kickID, Agent: name, Phase: crux.KickPhaseQueued, QueuedAt: queuedAt})
	q.emit(crux.KickEvent{Agent: name, Event: crux.KickEventQueued, KickID: kickID, At: queuedAt})
	start := !q.pumping[name]
	q.pumping[name] = true
	q.mu.Unlock()

	if start {
		go m.pumpKickQueue(name)
	}
	return kickID, nil
}

// KickStatus returns a queued kick's status by ID; see crux.KickQueuer.
func (m *Manager) KickStatus(kickID string) (crux.KickStatus, bool) {
	q := &m.kickQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	st, ok := q.statuses[kickID]
	if !ok {
		return crux.KickStatus{}, false
	}
	return *st, true
}

// KickQueueDepth reports how many kicks wait, undelivered, for the agent.
func (m *Manager) KickQueueDepth(name string) int {
	q := &m.kickQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending[name])
}

// InFlightKickID returns the ID of the queued kick whose turn the agent is
// running, or "" when the agent is not running one.
func (m *Manager) InFlightKickID(name string) string {
	q := &m.kickQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	if item, ok := q.inflight[name]; ok {
		return item.id
	}
	return ""
}

// pumpKickQueue drains one agent's FIFO, one kick per idle agent, and exits
// when the FIFO is empty. Exactly one pump runs per agent (q.pumping).
func (m *Manager) pumpKickQueue(name string) {
	q := &m.kickQueue
	for {
		now := time.Now()
		q.mu.Lock()
		items := q.pending[name]
		if len(items) == 0 {
			delete(q.pending, name)
			q.pumping[name] = false
			q.mu.Unlock()
			return
		}
		head := items[0]
		st := q.statuses[head.id]
		if st != nil && now.Sub(st.QueuedAt) > kickQueueWaitTimeout() {
			q.pending[name] = items[1:]
			q.settleLocked(head.id, crux.KickPhaseFailed, fmt.Sprintf(kickQueueErrWaitTimeout, kickQueueWaitTimeout()), "", now)
			q.mu.Unlock()
			continue
		}
		q.mu.Unlock()

		if !m.kickQueueAgentIdle(name, now) {
			time.Sleep(m.kickQueuePollEvery())
			continue
		}

		q.mu.Lock()
		q.pending[name] = q.pending[name][1:]
		q.inflight[name] = head
		if st := q.statuses[head.id]; st != nil {
			st.Phase = crux.KickPhasePending
		}
		q.mu.Unlock()

		err := m.kickQueueDeliver(name, head.message)
		if errors.Is(err, crux.ErrAgentBusy) {
			// The agent turned busy between the idle check and delivery: put
			// the kick back at the head and wait again.
			q.mu.Lock()
			delete(q.inflight, name)
			q.pending[name] = append([]*queuedKick{head}, q.pending[name]...)
			if st := q.statuses[head.id]; st != nil {
				st.Phase = crux.KickPhaseQueued
			}
			q.mu.Unlock()
			time.Sleep(m.kickQueuePollEvery())
			continue
		}

		delivered := time.Now()
		var kickAt time.Time
		if err == nil {
			m.mu.RLock()
			if agent, ok := m.agents[name]; ok && agent.LastKick != nil {
				kickAt = *agent.LastKick
			}
			m.mu.RUnlock()
		}
		q.mu.Lock()
		if err != nil {
			delete(q.inflight, name)
			q.settleLocked(head.id, crux.KickPhaseFailed, err.Error(), "", delivered)
		} else {
			head.kickAt = kickAt
			if st := q.statuses[head.id]; st != nil && !st.Terminal() {
				st.Phase = crux.KickPhaseDelivered
				st.DeliveredAt = delivered
			}
		}
		q.mu.Unlock()
	}
}

// kickQueuePollEvery is the idle re-check interval (test override first).
func (m *Manager) kickQueuePollEvery() time.Duration {
	if m.kickQueuePollInterval > 0 {
		return m.kickQueuePollInterval
	}
	return defaultKickQueuePollInterval
}

// kickQueueDeliver delivers one queued kick synchronously (test override
// first).
func (m *Manager) kickQueueDeliver(name, message string) error {
	if m.kickQueueDeliverFn != nil {
		return m.kickQueueDeliverFn(name, message)
	}
	return m.deliverQueuedKick(name, message)
}

// kickQueueAgentIdle reports whether the agent can take the next queued kick:
// it is live, nothing is being typed into it, no async dispatch is pending,
// and its last kicked turn ended (or ran past the turn timeout). It also
// fails an in-flight queued kick whose turn ran past the turn timeout.
// Must NOT be called with m.mu held.
func (m *Manager) kickQueueAgentIdle(name string, now time.Time) bool {
	if m.kickQueueIdleFn != nil {
		return m.kickQueueIdleFn(name)
	}
	turnTimeout := kickQueueTurnTimeout()

	q := &m.kickQueue
	q.mu.Lock()
	if item, ok := q.inflight[name]; ok {
		st := q.statuses[item.id]
		if st == nil || st.DeliveredAt.IsZero() || now.Sub(st.DeliveredAt) <= turnTimeout {
			q.mu.Unlock()
			return false
		}
		delete(q.inflight, name)
		q.settleLocked(item.id, crux.KickPhaseFailed, fmt.Sprintf(kickQueueErrTurnTimeout, turnTimeout), "", now)
	}
	q.mu.Unlock()

	if d, ok := m.kickDispatches.get(name); ok && d.Pending() {
		return false
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, ok := m.agents[name]
	if !ok || agent.Paused || agent.OperatorStopped || agent.kickDelivering.Load() {
		return false
	}
	if m.agentSandboxEnabledLocked(agent) {
		// A sandbox agent is busy exactly while its execution runs.
		return agent.State != StateRunning && agent.State != StateStopped && agent.State != StatePaused
	}
	if agent.State != StateRunning && (m.sharedPool == nil || agent.State != StateStopped) {
		return false
	}
	if agent.LastKick != nil && !agent.KickOutcome.Settled(agent.LastKick) && now.Sub(*agent.LastKick) <= turnTimeout {
		return false
	}
	return true
}

// deliverQueuedKick is SendKickAsync run synchronously on the pump goroutine:
// the same preconditions, the same exactly-once dispatch slot (so a
// dashboard kick during a queued delivery is deduplicated, not doubled), and
// the same delivery path. A busy slot is reported as crux.ErrAgentBusy so the
// pump re-queues instead of failing the kick.
func (m *Manager) deliverQueuedKick(name, message string) error {
	m.mu.Lock()
	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent %s not found", name)
	}
	if m.agentSandboxEnabledLocked(agent) {
		err := m.startSandboxKickLocked(agent, message)
		m.mu.Unlock()
		return err
	}
	if err := m.validatePaneKickLocked(agent, name); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	dispatch, fresh := m.kickDispatches.begin(name)
	if !fresh {
		return fmt.Errorf("%w: agent %s has a kick in flight", crux.ErrAgentBusy, name)
	}
	if err := m.deliverKickAsync(name, message); err != nil {
		m.kickDispatches.settleDispatch(dispatch, KickPhaseFailed, err.Error())
		return err
	}
	m.kickDispatches.settleDispatch(dispatch, KickPhaseDelivered, "")
	return nil
}

func newKickID() (string, error) {
	b := make([]byte, generatedKickIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate kick id: %w", err)
	}
	return generatedKickIDPrefix + hex.EncodeToString(b), nil
}

func kickQueueSize() int {
	if v, err := strconv.Atoi(os.Getenv(kickQueueSizeEnv)); err == nil && v > 0 {
		return v
	}
	return defaultKickQueueSize
}

func kickQueueWaitTimeout() time.Duration {
	return envDurationOr(kickQueueWaitTimeoutEnv, defaultKickQueueWaitTimeout)
}

func kickQueueTurnTimeout() time.Duration {
	return envDurationOr(kickQueueTurnTimeoutEnv, defaultKickQueueTurnTimeout)
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return fallback
}

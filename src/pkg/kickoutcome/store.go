package kickoutcome

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Trigger says who or what is about to kick an agent.
type Trigger struct {
	Kind  string
	Actor string
}

type intent struct {
	t  Trigger
	at time.Time
}

type fileShape struct {
	Version int                 `json:"version"`
	Agents  map[string][]Record `json:"agents"`
}

const fileVersion = 1

// Store is the durable record of kicks. The zero value is not usable; open one
// with Open. A nil *Store accepts every call and records nothing.
type Store struct {
	path   string
	logger *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	agents  map[string][]Record
	intents map[string]intent
}

// Open loads the store at path ("" keeps it in memory). A missing file is an
// empty store; an unreadable or corrupt one is returned empty with the error,
// so the caller can warn and keep recording.
func Open(path string, logger *slog.Logger) (*Store, error) {
	s := &Store{path: path, logger: logger, now: time.Now, agents: map[string][]Record{}, intents: map[string]intent{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("reading kick outcomes %s: %w", path, err)
	}
	var f fileShape
	if err := json.Unmarshal(b, &f); err != nil {
		return s, fmt.Errorf("parsing kick outcomes %s: %w", path, err)
	}
	for a, rs := range f.Agents {
		if len(rs) > MaxRecordsPerAgent {
			rs = rs[len(rs)-MaxRecordsPerAgent:]
		}
		if a != "" && len(s.agents) < MaxAgents {
			s.agents[a] = rs
		}
	}
	return s, nil
}

// SetClock replaces the clock; for tests.
func (s *Store) SetClock(now func() time.Time) {
	if s != nil && now != nil {
		s.now = now
	}
}

// Intend declares who or what is about to kick agent, so the delivery that
// follows is attributed to them. An intent unclaimed after IntentTTL is
// dropped. The runtime reports only a bare trigger word, so a caller that
// knows more says it here.
func (s *Store) Intend(agent string, t Trigger) {
	if s == nil || agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intents[agent] = intent{t: cleanTrigger(t), at: s.now()}
}

func cleanTrigger(t Trigger) Trigger {
	switch t.Kind {
	case TriggerDashboard, TriggerGovernor, TriggerWorkflowDispatch:
	default:
		t.Kind = TriggerOther
	}
	t.Actor = cleanRef(t.Actor, MaxActorRunes)
	return t
}

// takeIntent returns and clears the live intent for agent.
func (s *Store) takeIntent(agent string, at time.Time) Trigger {
	in, ok := s.intents[agent]
	delete(s.intents, agent)
	if !ok || at.Sub(in.at) > IntentTTL || at.Before(in.at.Add(-IntentTTL)) {
		return Trigger{Kind: TriggerOther}
	}
	return in.t
}

// Delivered records that a kick was typed into agent at at.
func (s *Store) Delivered(agent string, at time.Time) {
	if s == nil || agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.takeIntent(agent, at)
	r := s.add(agent, t, at)
	s.log("kick outcome: started", r, "")
}

// Refused records a kick the hive did not deliver, with the refusal reason
// code from the kick-refusals docs and the hive's own message. The declared
// trigger is t (an intent is consumed).
func (s *Store) Refused(agent string, t Trigger, code, reason string, at time.Time) {
	if s == nil || agent == "" || code == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.intents, agent)
	r := s.add(agent, cleanTrigger(t), at)
	end := at
	r.EndedAt = &end
	r.RefusalCode = cleanRef(code, 40)
	r.RefusalReason = Quote(reason)
	s.set(agent, r)
	s.log("kick outcome: refused", r, "")
}

// Declined marks the agent's open kick as refused by the agent's own model
// (the agent_declined reason), quoting its line.
func (s *Store) Declined(agent, line string, at time.Time) {
	s.update(agent, at, "declined", func(r *Record) bool {
		if r.RefusalCode != "" {
			return false
		}
		r.RefusalCode, r.RefusalReason = codeAgentDeclined, Quote(line)
		return true
	})
}

// TurnEnded records that the turn of the kick delivered at kickAt ended at at,
// classified as kind, with the agent's own line reason (sanitized here).
func (s *Store) TurnEnded(agent, kind, reason string, kickAt, at time.Time) {
	s.updateKick(agent, kickAt, "ended", func(r *Record) bool {
		if r.EndedAt != nil {
			return false
		}
		end := at
		r.EndedAt, r.TurnKind, r.TurnReason = &end, cleanRef(kind, 40), Quote(reason)
		return true
	})
}

// PROpened records a pull request the relay opened for agent.
func (s *Store) PROpened(agent, url string, number int, at time.Time) {
	s.update(agent, at, "pr", func(r *Record) bool {
		if r.PRAt != nil {
			return false
		}
		t := at
		r.PRAt, r.PRURL, r.PRNumber = &t, cleanRef(url, MaxRefRunes), number
		return true
	})
}

// Pushed records a branch the relay pushed for agent.
func (s *Store) Pushed(agent, branch, sha string, at time.Time) {
	s.update(agent, at, "push", func(r *Record) bool {
		if r.PushAt != nil {
			return false
		}
		t := at
		r.PushAt, r.PushBranch, r.PushSHA = &t, cleanRef(branch, MaxRefRunes), cleanRef(sha, 64)
		return true
	})
}

// Commented records a comment the relay posted for agent.
func (s *Store) Commented(agent string, at time.Time) {
	s.update(agent, at, "comment", func(r *Record) bool {
		if r.CommentAt != nil {
			return false
		}
		t := at
		r.CommentAt = &t
		return true
	})
}

// codeAgentDeclined is the refusal code of a kick the agent's own model
// refused: that kick ran, unlike one the hive refused to deliver.
const codeAgentDeclined = "agent_declined"

// update applies fn to the agent's latest kick that an action at at can be
// attributed to: started before at, and either still open or ended no more
// than ActionGrace before. A kick the hive refused never ran, so it is
// skipped; the latest kick that could have run is the only candidate.
func (s *Store) update(agent string, at time.Time, what string, fn func(*Record) bool) {
	if s == nil || agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.agents[agent]
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i]
		if (r.RefusalCode != "" && r.RefusalCode != codeAgentDeclined) || r.StartedAt.After(at) {
			continue
		}
		switch {
		case r.EndedAt != nil && at.Sub(*r.EndedAt) > ActionGrace:
			return
		case r.EndedAt == nil && at.Sub(r.StartedAt) > RunningCeiling:
			return
		}
		if fn(&r) {
			s.set(agent, r)
			s.log("kick outcome: signal", r, what)
		}
		return
	}
}

// updateKick applies fn to the agent's kick that started at kickAt (within
// KickSlack): a turn end belongs to one specific kick, never to a neighbour.
func (s *Store) updateKick(agent string, kickAt time.Time, what string, fn func(*Record) bool) {
	if s == nil || agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.agents[agent]
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i]
		if r.RefusalCode != "" && r.RefusalCode != codeAgentDeclined {
			continue
		}
		if d := r.StartedAt.Sub(kickAt); d < -KickSlack || d > KickSlack {
			continue
		}
		if fn(&r) {
			s.set(agent, r)
			s.log("kick outcome: signal", r, what)
		}
		return
	}
}

// KickSlack is how far the runtime's kick time may be from the time the
// delivery was recorded and still be the same kick.
const KickSlack = 2 * time.Minute

// add appends a new record for agent and persists. Caller holds mu.
func (s *Store) add(agent string, t Trigger, at time.Time) Record {
	r := Record{ID: "k" + strconv.FormatInt(at.UnixNano(), 36), Agent: agent, TriggerKind: t.Kind, TriggerActor: t.Actor, StartedAt: at}
	rs := append(s.agents[agent], r)
	if len(rs) > MaxRecordsPerAgent {
		rs = rs[len(rs)-MaxRecordsPerAgent:]
	}
	s.agents[agent] = rs
	s.trimAgentsLocked()
	s.saveLocked()
	return r
}

// set replaces the record with r's id and persists. Caller holds mu.
func (s *Store) set(agent string, r Record) {
	rs := s.agents[agent]
	for i := range rs {
		if rs[i].ID == r.ID {
			rs[i] = r
			break
		}
	}
	s.saveLocked()
}

// trimAgentsLocked bounds the number of agents tracked, dropping the ones
// whose latest kick is oldest.
func (s *Store) trimAgentsLocked() {
	if len(s.agents) <= MaxAgents {
		return
	}
	type last struct {
		name string
		at   time.Time
	}
	var all []last
	for n, rs := range s.agents {
		all = append(all, last{n, rs[len(rs)-1].StartedAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	for _, l := range all[MaxAgents:] {
		delete(s.agents, l.name)
	}
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(fileShape{Version: fileVersion, Agents: s.agents})
	if err == nil {
		err = os.MkdirAll(filepath.Dir(s.path), 0o750)
	}
	if err == nil {
		tmp := s.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, s.path)
		}
	}
	if err != nil && s.logger != nil {
		s.logger.Warn("kick outcome: saving failed", "error", err)
	}
}

// log writes an audit line with ids and kinds only: never agent text.
func (s *Store) log(msg string, r Record, signal string) {
	if s.logger == nil {
		return
	}
	o := Derive(r, s.now(), true)
	s.logger.Info(msg, "agent", r.Agent, "kick_id", r.ID, "trigger_kind", r.TriggerKind, "outcome_kind", o.Kind, "signal", signal)
}

// Records returns the agent's kicks, newest first, at most n (all kept when n
// is not positive).
func (s *Store) Records(agent string, n int) []Record {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.agents[agent]
	out := make([]Record, 0, len(rs))
	for i := len(rs) - 1; i >= 0; i-- {
		out = append(out, rs[i])
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// Agents lists the agents with a kick on record.
func (s *Store) Agents() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.agents))
	for a := range s.agents {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

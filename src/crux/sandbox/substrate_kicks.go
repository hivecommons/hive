package sandbox

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

// Substrate kick reader (hivecommons/hive#17777), the substrate counterpart
// of the OpenShell log reader in openshell_logs.go.
//
// A substrate-runtime agent has no tmux session, so the dashboard's
// read-only kick viewer needs the agent's live actor and its output. The
// Control API has no ListActors and Substrate has no logs RPC, so neither
// can come from the control plane: the launcher, which runs in the same
// hive process as the dashboard, records each kick here instead — the
// actor's name, every state it is seen in (RESUMING, RUNNING, then
// SUSPENDED or DELETING), the worker's SPIFFE id, and the kick's stdout
// and stderr as the bridge streams /run. Unlike the OpenShell gateway log,
// this is the kick's own output and the exit code is the kick's.

// SubstrateActorInfo is what the viewer needs to know about a kick's actor.
type SubstrateActorInfo struct {
	Name     string
	Atespace string
	// State is the actor's ActorState without its ACTOR_STATE_ prefix, or
	// CREATED before the control plane has reported one.
	State string
	// SpiffeID is the identity of the worker the actor was scheduled on
	// (WorkerAssignment.worker_spiffe_id), once known.
	SpiffeID string
	// Exited is true once the bridge's /run reported the kick's exit code.
	Exited   bool
	ExitCode int
	// Suspended is true when the launcher kept a snapshot of the actor
	// (snapshot_on_idle) instead of deleting it.
	Suspended bool
	// Done is true once the launcher has finished with the actor.
	Done bool
}

// Finished reports whether the kick will produce no more output.
func (i SubstrateActorInfo) Finished() bool { return i.Done }

// SubstrateLogReader is the dashboard's view of the substrate kicks this
// process runs. *SubstrateKicks implements it.
type SubstrateLogReader interface {
	// LatestActor returns the newest kick the launcher recorded for
	// launchName (see LaunchName). A finished kick is still returned so the
	// viewer can say how its actor ended; found is false when the agent has
	// not run a substrate kick since hive started.
	LatestActor(ctx context.Context, launchName string) (info SubstrateActorInfo, found bool, err error)
	// StreamRun writes the kick's output to w, calling onState for every
	// state the actor reaches after the call (the caller has already shown
	// the one LatestActor reported), polling every poll
	// until the launcher is done with the actor or ctx ends, and returns
	// the last state it observed.
	StreamRun(ctx context.Context, actorName string, poll time.Duration, w io.Writer, onState func(SubstrateActorInfo)) (SubstrateActorInfo, error)
}

// DefaultSubstrateKicks is the process-wide record the launcher writes and
// the dashboard reads.
var DefaultSubstrateKicks = &SubstrateKicks{}

var _ SubstrateLogReader = (*SubstrateKicks)(nil)

// SubstrateKicks records the latest substrate kick per launch name.
type SubstrateKicks struct {
	mu       sync.Mutex
	byLaunch map[string]*substrateKick
	// lastSuspended is the newest actor each launch name left suspended
	// (snapshot_on_idle), with the atespace it was created in; in memory
	// only, so a hive restart forgets it.
	lastSuspended map[string]SubstrateActorInfo
}

// substrateKick is one recorded kick: every state snapshot it went through
// and its output so far, capped like the bridge's.
type substrateKick struct {
	history []SubstrateActorInfo
	out     []byte
}

func (k *substrateKick) latest() SubstrateActorInfo { return k.history[len(k.history)-1] }

// begin records a new kick for launchName, replacing the previous one.
func (s *SubstrateKicks) begin(launchName string, info SubstrateActorInfo) *substrateKick {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byLaunch == nil {
		s.byLaunch = map[string]*substrateKick{}
	}
	k := &substrateKick{history: []SubstrateActorInfo{info}}
	s.byLaunch[launchName] = k
	return k
}

// swapSuspended records actor (name and atespace) as launchName's retained
// suspended actor and returns the one it replaces, or a zero value when
// there was none.
func (s *SubstrateKicks) swapSuspended(launchName string, actor SubstrateActorInfo) SubstrateActorInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastSuspended == nil {
		s.lastSuspended = map[string]SubstrateActorInfo{}
	}
	prev := s.lastSuspended[launchName]
	s.lastSuspended[launchName] = SubstrateActorInfo{Name: actor.Name, Atespace: actor.Atespace}
	return prev
}

// update applies fn to the kick's latest state and records the result as
// a new snapshot when anything changed.
func (s *SubstrateKicks) update(k *substrateKick, fn func(*SubstrateActorInfo)) {
	if k == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := k.latest()
	fn(&next)
	if next != k.latest() {
		k.history = append(k.history, next)
	}
}

// observe records what GetActor reported: state and worker SPIFFE id.
func (s *SubstrateKicks) observe(k *substrateKick, actor *substrateapi.Actor) {
	s.update(k, func(i *SubstrateActorInfo) {
		if st := substrateStateName(actor.GetStatus().GetState()); st != "" {
			i.State = st
		}
		if id := actor.GetStatus().GetWorkerAssignment().GetWorkerSpiffeId(); id != "" {
			i.SpiffeID = id
		}
	})
}

// output returns a writer that appends to the kick's recorded output.
func (s *SubstrateKicks) output(k *substrateKick) io.Writer {
	if k == nil {
		return nil
	}
	return substrateKickWriter{s: s, k: k}
}

type substrateKickWriter struct {
	s *SubstrateKicks
	k *substrateKick
}

func (w substrateKickWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	chunk := p
	if room := substratebridge.MaxOutput - len(w.k.out); len(chunk) > room {
		chunk = chunk[:max(room, 0)]
	}
	w.k.out = append(w.k.out, chunk...)
	return len(p), nil
}

// LatestActor implements SubstrateLogReader.
func (s *SubstrateKicks) LatestActor(_ context.Context, launchName string) (SubstrateActorInfo, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byLaunch[launchName]
	if !ok {
		return SubstrateActorInfo{}, false, nil
	}
	return k.latest(), true, nil
}

func (s *SubstrateKicks) byActor(name string) *substrateKick {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.byLaunch {
		if k.history[0].Name == name {
			return k
		}
	}
	return nil
}

// StreamRun implements SubstrateLogReader.
func (s *SubstrateKicks) StreamRun(ctx context.Context, actorName string, poll time.Duration, w io.Writer, onState func(SubstrateActorInfo)) (SubstrateActorInfo, error) {
	k := s.byActor(actorName)
	if k == nil {
		// Replaced by a newer kick before the viewer got here: this one is over.
		return SubstrateActorInfo{Name: actorName, Done: true}, nil
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	// The caller has just rendered LatestActor's snapshot, the newest one:
	// replaying older history would show the actor going backwards.
	s.mu.Lock()
	seenStates, seenOut := len(k.history), 0
	s.mu.Unlock()
	for {
		s.mu.Lock()
		states := append([]SubstrateActorInfo(nil), k.history[seenStates:]...)
		out := append([]byte(nil), k.out[seenOut:]...)
		seenStates, seenOut = len(k.history), len(k.out)
		latest := k.latest()
		s.mu.Unlock()
		// States reached before the kick exited come before this poll's
		// output, the rest after it, so the pane reads in order.
		split := len(states)
		for i, st := range states {
			if st.Exited || st.Done {
				split = i
				break
			}
		}
		emit := func(sts []SubstrateActorInfo) {
			if onState != nil {
				for _, st := range sts {
					onState(st)
				}
			}
		}
		emit(states[:split])
		if len(out) > 0 {
			if _, err := w.Write(out); err != nil {
				return latest, err
			}
		}
		emit(states[split:])
		if latest.Done {
			return latest, nil
		}
		select {
		case <-ctx.Done():
			return latest, ctx.Err()
		case <-ticker.C:
		}
	}
}

// substrateStateName is an ActorState as the viewer shows it: RUNNING, not
// ACTOR_STATE_RUNNING; empty for UNSPECIFIED.
func substrateStateName(st substrateapi.ActorState) string {
	if st == substrateapi.ActorState_ACTOR_STATE_UNSPECIFIED {
		return ""
	}
	return strings.TrimPrefix(st.String(), "ACTOR_STATE_")
}

// Substrate actor states as SubstrateActorInfo.State carries them.
const (
	SubstrateStateCreated   = "CREATED"
	SubstrateStateResuming  = "RESUMING"
	SubstrateStateRunning   = "RUNNING"
	SubstrateStateSuspended = "SUSPENDED"
	SubstrateStateDeleting  = "DELETING"
)

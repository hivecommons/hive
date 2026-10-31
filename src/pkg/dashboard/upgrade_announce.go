package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// Every roll gets the upgrade animation (hivecommons/hive#15788). The spoke's
// upgrade state machine (upgrade_progress.go) used to derive its state only
// from the self-upgrade path — drain latch, PVC marker, landed outcome — so a
// hub-managed kubectl roll, an operator's `kubectl rollout restart` or a
// plain SIGTERM showed nothing until the SSE stream dropped. Two layers fix
// that, both rendered through the same UpgradeProgress the self-upgrade uses:
//
//   - INTENT before the kill: AnnounceUpgrade (POST /api/upgrade/announce, or
//     the heartbeat's upgrade_announce from the hub) marks the progress
//     in-flight with phase=announced and the target, and publishes an SSE
//     "upgrading" event.
//   - FALLBACK at the kill: AnnounceRestart runs from the SIGTERM hook,
//     publishes "upgrading" with phase=restarting (target from the announce
//     or the hub's armed/target SHA when known) and marks the progress
//     rolling before the process exits.
//
// The SPA adopts either into the bee animation and, after reconnecting,
// compares the new build's SHA and process start to say "Upgraded → <ver>" or
// "Restarted".

// upgradeAnnounceStaleAfter bounds how long an announce without a kill keeps
// the progress in flight. A hub may abort a roll it announced, an operator
// script may fail between the announce and the restart; after this the pill
// falls back to the scheduled state instead of claiming an upgrade forever.
const upgradeAnnounceStaleAfter = 15 * time.Minute

// upgradeAnnounceMaxField caps the target/reason strings an announce may
// carry, so a stray body cannot bloat /api/status.
const upgradeAnnounceMaxField = 200

// sseUpgradingEvent is the SSE event name the dashboard listens for.
const sseUpgradingEvent = "upgrading"

// upgradeAnnounceState is the live announce/restart record behind
// buildUpgradeProgress's Announce input.
type upgradeAnnounceState struct {
	Target string
	Source string
	Reason string
	// At is when the announce arrived; StartedAt for the progress.
	At time.Time
	// Restarting is set by AnnounceRestart: the process is exiting now.
	Restarting bool
	// RestartAt is when the SIGTERM hook ran.
	RestartAt time.Time
}

// upgradeAnnounceStore guards the record; a zero value is usable so tests
// may build a bare Server.
type upgradeAnnounceStore struct {
	mu    sync.Mutex
	state *upgradeAnnounceState
}

func (st *upgradeAnnounceStore) get() *upgradeAnnounceState {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.state == nil {
		return nil
	}
	cp := *st.state
	return &cp
}

func (st *upgradeAnnounceStore) set(a *upgradeAnnounceState) {
	st.mu.Lock()
	st.state = a
	st.mu.Unlock()
}

// upgradeAnnounceEvent is the JSON body of the SSE "upgrading" event and of
// the announce endpoint's response. Field names mirror UpgradeProgress so the
// SPA can adopt either shape.
type upgradeAnnounceEvent struct {
	Phase     string `json:"phase"`
	State     string `json:"state"`
	Source    string `json:"source,omitempty"`
	Target    string `json:"target,omitempty"`
	From      string `json:"from,omitempty"`
	Reason    string `json:"reason,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	// ProcessStartedAt identifies the process that announced, so the SPA can
	// tell "same process, roll aborted" from "new process, restarted".
	ProcessStartedAt string `json:"processStartedAt,omitempty"`
	Detail           string `json:"detail"`
}

// liveUpgradeAnnounce returns the announce record if it is still current:
// a restart never expires (the process is exiting), an announce without a
// kill expires after upgradeAnnounceStaleAfter.
func liveUpgradeAnnounce(a *upgradeAnnounceState, now time.Time) *upgradeAnnounceState {
	if a == nil {
		return nil
	}
	if a.Restarting {
		return a
	}
	if a.At.IsZero() || now.Sub(a.At) > upgradeAnnounceStaleAfter {
		return nil
	}
	return a
}

// announceProgress renders the announce/restart record in the shared state
// vocabulary: announced → pulling (the instruction is delivered, the pod is
// about to be rolled), restarting → rolling (the pod is going down now).
func announceProgress(a *upgradeAnnounceState, runningSHA, nextWindow string, now time.Time) *spoke.UpgradeProgress {
	if a == nil {
		return nil
	}
	rfc := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	target := shortSHADashboard(a.Target)
	p := &spoke.UpgradeProgress{
		Target:       target,
		From:         shortSHADashboard(runningSHA),
		StartedAt:    rfc(a.At),
		NextWindowAt: nextWindow,
		Reason:       a.Reason,
		Source:       a.Source,
	}
	who := a.Source
	if who == "" {
		who = "the " + spoke.UpgradeSourceOperator
	} else if who == spoke.UpgradeSourceHub || who == spoke.UpgradeSourceOperator {
		who = "the " + who
	}
	if a.Restarting {
		p.State = spoke.UpgradeStateRolling
		p.Phase = spoke.UpgradePhaseRestarting
		if a.Source == "" {
			p.Source = spoke.UpgradeSourceSignal
		}
		if !a.RestartAt.IsZero() {
			if a.At.IsZero() {
				p.StartedAt = rfc(a.RestartAt)
			}
		}
		if target != "" {
			p.Detail = "restarting onto " + target + " · reconnecting"
		} else {
			p.Detail = "restarting · reconnecting"
		}
		return p
	}
	p.State = spoke.UpgradeStatePulling
	p.Phase = spoke.UpgradePhaseAnnounced
	if a.Source == "" {
		p.Source = spoke.UpgradeSourceOperator
	}
	if target != "" {
		p.Detail = "roll announced by " + who + ": restarting onto " + target + " " + spoke.FormatShortDuration(now.Sub(a.At)) + " ago"
	} else {
		p.Detail = "restart announced by " + who + " " + spoke.FormatShortDuration(now.Sub(a.At)) + " ago"
	}
	if a.Reason != "" {
		p.Detail += " — " + a.Reason
	}
	return p
}

// AnnounceUpgrade records a roll intent and publishes it on the SSE stream.
// A repeat announce for the same target refreshes the clock; a different
// target replaces the record. Returns the progress the announce renders as.
func (s *Server) AnnounceUpgrade(a spoke.UpgradeAnnounce, now time.Time) *spoke.UpgradeProgress {
	if now.IsZero() {
		now = time.Now()
	}
	source := strings.TrimSpace(a.Source)
	if source != spoke.UpgradeSourceHub {
		source = spoke.UpgradeSourceOperator
	}
	st := &upgradeAnnounceState{
		Target: clipAnnounceField(a.Target),
		Source: source,
		Reason: clipAnnounceField(a.Reason),
		At:     now,
	}
	if cur := liveUpgradeAnnounce(s.upgradeAnnounce.get(), now); cur != nil && !cur.Restarting && sameAnnounceTarget(cur.Target, st.Target) {
		st.At = cur.At
	}
	s.upgradeAnnounce.set(st)
	p := announceProgress(st, s.runningVersionHash(), "", now)
	s.broadcastUpgrading(p, now)
	if s.logger != nil {
		s.logger.Info("upgrade announced", "source", st.Source, "target", st.Target, "reason", st.Reason)
	}
	return p
}

// AnnounceRestart is the SIGTERM hook: the process is going down. It keeps
// an announced target when one is live, else falls back to the hub's
// armed/target SHA when that differs from the running build, else announces
// a bare restart. Publishes the SSE "upgrading" event before returning so
// the dashboard shows the animation instead of a bare "reconnecting".
func (s *Server) AnnounceRestart(now time.Time) *spoke.UpgradeProgress {
	if now.IsZero() {
		now = time.Now()
	}
	st := &upgradeAnnounceState{Restarting: true, RestartAt: now, Source: spoke.UpgradeSourceSignal}
	if cur := liveUpgradeAnnounce(s.upgradeAnnounce.get(), now); cur != nil {
		st.Target, st.Source, st.Reason, st.At = cur.Target, cur.Source, cur.Reason, cur.At
	}
	if st.Target == "" {
		st.Target = s.hubTargetForRestart()
	}
	if st.At.IsZero() {
		st.At = now
	}
	s.upgradeAnnounce.set(st)
	p := announceProgress(st, s.runningVersionHash(), "", now)
	s.broadcastUpgrading(p, now)
	if s.logger != nil {
		s.logger.Info("restart announced to dashboard", "source", st.Source, "target", st.Target)
	}
	return p
}

// hubTargetForRestart is the best guess at what the pod restarts onto when
// nobody announced: the hub's armed target, else its resolved target SHA,
// when either differs from the running build. Empty when unknown.
func (s *Server) hubTargetForRestart() string {
	s.versionMu.RLock()
	policy := s.hubUpgradePolicy
	s.versionMu.RUnlock()
	if policy == nil {
		return ""
	}
	running := s.runningVersionHash()
	for _, cand := range []string{policy.ArmedTarget, policy.TargetSHA} {
		cand = strings.TrimSpace(cand)
		if cand == "" || (running != "" && sameCommitDashboard(running, cand)) {
			continue
		}
		return cand
	}
	return ""
}

func (s *Server) runningVersionHash() string {
	if versionHash == "unknown" {
		return ""
	}
	return versionHash
}

// broadcastUpgrading publishes the progress as an SSE "upgrading" event.
func (s *Server) broadcastUpgrading(p *spoke.UpgradeProgress, now time.Time) {
	if p == nil {
		return
	}
	ev := upgradeAnnounceEvent{
		Phase:     p.Phase,
		State:     p.State,
		Source:    p.Source,
		Target:    p.Target,
		From:      p.From,
		Reason:    p.Reason,
		StartedAt: p.StartedAt,
		Detail:    p.Detail,
	}
	if !s.startedAt.IsZero() {
		ev.ProcessStartedAt = s.startedAt.UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.broadcastFrame("event: " + sseUpgradingEvent + "\ndata: " + string(data) + "\n\n")
}

// upgradeAnnounceRequest is the body of POST /api/upgrade/announce.
type upgradeAnnounceRequest struct {
	Target  string `json:"target"`
	Version string `json:"version"`
	Source  string `json:"source"`
	Reason  string `json:"reason"`
}

// handleUpgradeAnnounce — POST /api/upgrade/announce. Owner-only. Lets the
// hub or an operator script say "I am about to roll this pod" so the
// dashboard animates the roll before the kill. Body: {"target": "<sha|tag>",
// "source": "hub"|"operator", "reason": "..."}; "version" is accepted as an
// alias of target. Responds with the UpgradeProgress the spoke now reports.
func (s *Server) handleUpgradeAnnounce(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var req upgradeAnnounceRequest
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil && err.Error() != "EOF" {
			jsonError(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	target := strings.TrimSpace(req.Target)
	if target == "" {
		target = strings.TrimSpace(req.Version)
	}
	p := s.AnnounceUpgrade(spoke.UpgradeAnnounce{Target: target, Source: req.Source, Reason: req.Reason}, time.Now())
	s.AuditLog("dashboard", "upgrade_announce", "source="+p.Source+" target="+orDashDashboard(p.Target), "")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upgradeProgress": p})
}

func clipAnnounceField(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > upgradeAnnounceMaxField {
		v = v[:upgradeAnnounceMaxField]
	}
	return v
}

func sameAnnounceTarget(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return sameCommitDashboard(a, b) || strings.EqualFold(a, b)
}

// Startup phases (#15788). cmd/hive names each boot phase as it starts;
// while the first status payload has not been published, /api/status and
// /api/version carry startupPhase so the dashboard reconnecting onto the
// new pod can show what it is waiting on.

// SetStartupPhase records the boot phase now running. Empty clears it.
func (s *Server) SetStartupPhase(phase string) {
	s.startupPhase.Store(strings.TrimSpace(phase))
}

// StartupPhase returns the recorded phase, "" once the first status has been
// published (the boot is over as far as the dashboard is concerned).
func (s *Server) StartupPhase() string {
	s.statusMu.RLock()
	ready := s.status != nil
	s.statusMu.RUnlock()
	if ready {
		return ""
	}
	v, _ := s.startupPhase.Load().(string)
	return v
}

// startupPhaseLabels maps cmd/hive boot phase names to the words the banner
// shows. Unknown names fall back to the raw phase.
var startupPhaseLabels = map[string]string{
	"bootGitHub":       "connecting to GitHub",
	"bootGovernor":     "starting governor",
	"bootAdvisory":     "opening mutation ledger",
	"bootAgents":       "preparing agents",
	"bootState":        "restoring state",
	"bootDashboard":    "serving dashboard",
	"bootStores":       "opening stores",
	"bootCollectors":   "starting collectors",
	"bootKnowledge":    "indexing knowledge",
	"bootSupervision":  "starting supervision",
	"bootDashboardAPI": "wiring dashboard API",
	"bootPolicies":     "loading policies",
	"bootWatchers":     "starting watchers",
	"bootProxy":        "starting proxy",
	"bootLaunch":       "launching agents",
	"bootHeartbeat":    "registering with hub",
	"bootLanes":        "opening lanes",
	"firstEval":        "first eval",
}

// StartupPhaseLabel is the human label for a phase name.
func StartupPhaseLabel(phase string) string {
	if l, ok := startupPhaseLabels[phase]; ok {
		return l
	}
	return phase
}

// initializingStatus is the /api/status body before the first eval: the
// legacy {"status":"initializing"} plus the build identity and boot phase
// the upgrade banner needs to say "Upgraded → <ver>" before the first eval.
func (s *Server) initializingStatus() map[string]any {
	out := map[string]any{"status": "initializing"}
	if phase := s.StartupPhase(); phase != "" {
		out["startupPhase"] = phase
		out["startupPhaseLabel"] = StartupPhaseLabel(phase)
	}
	if h := s.runningVersionHash(); h != "" {
		out["hash"] = h
		out["short"] = shortSHADashboard(h)
	}
	if !s.startedAt.IsZero() {
		out["startedAt"] = s.startedAt.UTC().Format(time.RFC3339)
	}
	return out
}

package dashboard

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/watchdog"
)

// This file backs the Health tab's "Watchdog activity" strip (#7254).
//
// Settings → Health → Agent Watchdog tells the operator to review the
// `watchdog-*-observed` audit entries and then switch to Heal — RFC #4665
// makes that promotion a data-driven decision — but until this endpoint the
// data lived only in /data/audit.jsonl behind a manual search. A hive that sat
// 6 h in Observe with 0 would-have-acted events out of 28,775 audit entries
// showed nothing on the page that said so. GET /api/watchdog/activity turns
// the audit trail into the number, the 30-day histogram, and the per-agent
// liveness the decision actually needs.

const (
	// watchdogActivityDefaultDays is the trailing window when ?days is absent.
	watchdogActivityDefaultDays = 30
	// watchdogActivityMaxDays caps the window at the audit log's own
	// retention (auditMaxAgeDays): asking for more would report a partial
	// window as if it were complete.
	watchdogActivityMaxDays = auditMaxAgeDays
	// watchdogActivityDateLayout is the per-day bucket key: a UTC calendar date.
	watchdogActivityDateLayout = "2006-01-02"

	// watchdogStateCrashLoop is the per-agent liveness state reported when the
	// watchdog has paused the agent as crash-looping. It is not a pane class —
	// the reconciler's CrashLooping latch lives behind the Fleet interface —
	// but the pause it produces is stamped with CrashLoopTrigger on the agent,
	// and that stamp outranks whatever the pane looks like now.
	watchdogStateCrashLoop = "crash-loop"
	// watchdogStateUnknown is reported for an agent the watchdog has published
	// conditions for but never a Ready verdict (should not happen; the
	// reconciler always sets Ready first, but a reader must never guess).
	watchdogStateUnknown = string(watchdog.ClassUnknown)
)

// watchdogActivityAuditPath is where the readout scans the on-disk audit log;
// "" means the production auditLogPath. A test seam in the shape of the
// package-level auditLogPath itself, so a handler test can point the scan at
// a fixture without touching /data. Package-level rather than a Server field
// so the seam adds no line to server.go, whose route registrations are cited
// by line number from docs/api-reference.md.
var watchdogActivityAuditPath = ""

// WatchdogActivity is the GET /api/watchdog/activity response.
type WatchdogActivity struct {
	// Days is the trailing window actually applied (after clamping).
	Days int `json:"days"`
	// Since is the UTC start of the window (midnight, Days-1 days ago), so a
	// reader can tell which calendar days the buckets cover.
	Since string `json:"since"`
	// Mode is the watchdog's RESOLVED authority (off/observe/heal, with the
	// fleet-wide kill switch already applied), and Acting whether that mode
	// takes fleet-changing action. The count's headline depends on it: in
	// observe these are actions the watchdog WOULD have taken.
	Mode   string `json:"mode"`
	Acting bool   `json:"acting"`
	// Total is every watchdog-* audit action in the window; Taken and Observed
	// split it by whether the action was performed or only recorded.
	Total    int `json:"total"`
	Taken    int `json:"taken"`
	Observed int `json:"observed"`
	// ByAction counts per action, keyed by the audit action name minus the
	// `watchdog-` prefix: restart, crashloop-pause, giveup, healthy-reset, and
	// their -observed twins.
	ByAction map[string]int `json:"byAction"`
	// Daily is one bucket per UTC calendar day in the window, oldest first,
	// zero-filled — the sparkline's series.
	Daily []WatchdogActivityDay `json:"daily"`
	// Agents is the current watchdog liveness state per probed agent.
	Agents []WatchdogAgentLiveness `json:"agents"`
	// LastAction/LastActionAt name the most recent entry in the window (taken
	// or observed); empty when there is none.
	LastAction   string `json:"lastAction,omitempty"`
	LastActionAt string `json:"lastActionAt,omitempty"`
	// Promotion is the Observe → Heal hint. Present only in observe mode: in
	// heal the decision has been made, and in off there is no evidence.
	Promotion *WatchdogPromotionHint `json:"promotion,omitempty"`
}

// WatchdogActivityDay is one daily histogram bucket.
type WatchdogActivityDay struct {
	Date     string         `json:"date"`
	Count    int            `json:"count"`
	ByAction map[string]int `json:"byAction,omitempty"`
}

// WatchdogAgentLiveness is one agent's current watchdog state.
type WatchdogAgentLiveness struct {
	Name string `json:"name"`
	// State is the liveness verdict: a watchdog.PaneClass (ready,
	// stuck-overlay, shell-prompt, no-output, no-session, auth-required,
	// unknown) or "crash-loop" when the watchdog has paused the agent.
	State string `json:"state"`
	// Since is when the agent entered State (RFC3339); empty when unknown.
	Since string `json:"since,omitempty"`
	// Message is the reconciler's reasoning behind State.
	Message string `json:"message,omitempty"`
	// Authenticated and Producing are the other two condition axes' statuses
	// (True/False/Unknown), so the strip can show a live-but-not-producing
	// agent without a second lookup.
	Authenticated string `json:"authenticated,omitempty"`
	Producing     string `json:"producing,omitempty"`
	// Paused reports the manager's pause flag, whatever set it.
	Paused bool `json:"paused"`
}

// WatchdogPromotionHint is the Observe → Heal verdict.
type WatchdogPromotionHint struct {
	// Safe is true when nothing in the window argues against promotion: no
	// crash-loop pause and no give-up verdict, observed or taken.
	Safe bool `json:"safe"`
	// Reason says why, in the operator's terms.
	Reason string `json:"reason"`
}

// handleWatchdogActivity serves GET /api/watchdog/activity?days=N.
//
// Gated at the same tier as GET /api/audit: everything here is derived from
// the audit log, and the audit log is a read-write-role surface.
func (s *Server) handleWatchdogActivity(w http.ResponseWriter, r *http.Request) {
	if !config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleReadWrite) {
		jsonError(w, "insufficient access", http.StatusForbidden)
		return
	}
	days := watchdogActivityDefaultDays
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			jsonError(w, "days must be a positive integer", http.StatusBadRequest)
			return
		}
		days = n
	}
	if days > watchdogActivityMaxDays {
		days = watchdogActivityMaxDays
	}

	now := time.Now().UTC()
	since := watchdogActivitySince(now, days)

	// The file is the record: the ring holds 500 entries and forgets them on
	// restart, which on a busy hive is minutes of history, not 30 days. The
	// ring is consulted only when there is no file at all (no /data volume).
	var entries []AuditEntry
	if s.audit.HasOnDiskLog(watchdogActivityAuditPath) {
		entries = s.audit.ActionsWithPrefixSince(since, watchdog.AuditActionPrefix, watchdogActivityAuditPath)
	} else {
		entries = s.audit.RecentWithPrefixSince(since, watchdog.AuditActionPrefix)
	}

	var cfg *config.Config
	var statuses map[string]*agent.AgentProcess
	if s.deps != nil {
		cfg = s.deps.Config
		if s.deps.AgentMgr != nil {
			statuses = s.deps.AgentMgr.AllStatuses()
		}
	}
	settings := watchdog.DefaultSettings()
	if cfg != nil {
		settings, _ = watchdog.SettingsFrom(cfg.Governor.Watchdog)
	}

	jsonResponse(w, buildWatchdogActivity(entries, statuses, cfg, settings, now, days))
}

// watchdogActivitySince is the UTC midnight that opens a trailing window of
// `days` calendar days ending today, so the daily buckets are whole days and
// "30 days" means thirty dates rather than 720 hours that split a bucket.
func watchdogActivitySince(now time.Time, days int) time.Time {
	today := now.UTC().Truncate(24 * time.Hour)
	return today.AddDate(0, 0, -(days - 1))
}

// buildWatchdogActivity aggregates the pre-filtered watchdog audit entries
// and the agents' published conditions into the response. Pure: every input
// is a value, so the handler test and the unit tests share it.
func buildWatchdogActivity(entries []AuditEntry, statuses map[string]*agent.AgentProcess, cfg *config.Config, settings watchdog.Settings, now time.Time, days int) WatchdogActivity {
	since := watchdogActivitySince(now, days)
	out := WatchdogActivity{
		Days:     days,
		Since:    since.Format(time.RFC3339),
		Mode:     string(settings.Mode),
		Acting:   settings.MayAct(),
		ByAction: map[string]int{},
		Daily:    make([]WatchdogActivityDay, 0, days),
		Agents:   watchdogAgentLiveness(statuses, cfg),
	}

	byDay := make(map[string]*WatchdogActivityDay, days)
	for i := 0; i < days; i++ {
		d := since.AddDate(0, 0, i).Format(watchdogActivityDateLayout)
		out.Daily = append(out.Daily, WatchdogActivityDay{Date: d})
		byDay[d] = &out.Daily[len(out.Daily)-1]
	}

	severe := 0
	var lastAt time.Time
	for _, e := range entries {
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || t.Before(since) {
			// The scanner already applied the window; this is belt-and-braces
			// for a caller that hands over unfiltered entries.
			continue
		}
		key := strings.TrimPrefix(e.Action, watchdog.AuditActionPrefix)
		out.Total++
		out.ByAction[key]++
		if strings.HasSuffix(e.Action, watchdog.AuditObservedSuffix) {
			out.Observed++
		} else {
			out.Taken++
		}
		if watchdogActionSevere(e.Action) {
			severe++
		}
		if day, ok := byDay[t.UTC().Format(watchdogActivityDateLayout)]; ok {
			day.Count++
			if day.ByAction == nil {
				day.ByAction = map[string]int{}
			}
			day.ByAction[key]++
		}
		if !t.Before(lastAt) {
			lastAt = t
			out.LastAction = key
			out.LastActionAt = t.UTC().Format(time.RFC3339)
		}
	}

	if settings.Mode == watchdog.ModeObserve {
		out.Promotion = watchdogPromotionHint(out, severe, days)
	}
	return out
}

// watchdogActionSevere reports whether an action — taken or observed — is a
// crash-loop pause or a give-up: the verdicts that mean the watchdog decided
// (or would have decided) that restarting was not enough. These are what an
// operator must look at before granting it the authority to act, so their
// presence in the window blocks the promotion hint and colours the sparkline.
func watchdogActionSevere(action string) bool {
	base := strings.TrimSuffix(action, watchdog.AuditObservedSuffix)
	return base == watchdog.AuditActionPause || base == watchdog.AuditActionGiveUp
}

// watchdogPromotionHint applies the Observe → Heal rule from #7254: promotion
// is called safe when the window holds no pause/give-up verdict. It says how
// many would-be restarts the data rests on, so "safe" on a window with zero
// events reads as "nothing happened" rather than "everything is fine".
func watchdogPromotionHint(a WatchdogActivity, severe, days int) *WatchdogPromotionHint {
	if severe > 0 {
		return &WatchdogPromotionHint{
			Safe: false,
			Reason: fmt.Sprintf("%d crash-loop pause / give-up verdict(s) in %d d — the watchdog would have paused an agent. Investigate those agents before promoting.",
				severe, days),
		}
	}
	restarts := a.ByAction["restart"+watchdog.AuditObservedSuffix] + a.ByAction["restart"]
	if a.Total == 0 {
		return &WatchdogPromotionHint{
			Safe:   true,
			Reason: fmt.Sprintf("no would-have-acted events in %d d: the watchdog found nothing to restart or pause.", days),
		}
	}
	return &WatchdogPromotionHint{
		Safe: true,
		Reason: fmt.Sprintf("%d would-be restart(s) and no pause / give-up verdict in %d d — healing would have restarted agents and never escalated.",
			restarts, days),
	}
}

// watchdogAgentLiveness derives each probed agent's current watchdog state
// from the condition set the reconciler publishes onto the agent (via
// Fleet.SetConditions → AgentProcess.WatchdogConditions). Agents without
// conditions are omitted rather than listed as healthy: the watchdog has not
// looked at them, and a row would claim an observation it never made. Agents
// disabled in config are omitted too — they are not supposed to be alive.
func watchdogAgentLiveness(statuses map[string]*agent.AgentProcess, cfg *config.Config) []WatchdogAgentLiveness {
	out := make([]WatchdogAgentLiveness, 0, len(statuses))
	for name, proc := range statuses {
		if proc == nil || len(proc.WatchdogConditions) == 0 || agentDisabledInConfig(cfg, name, proc) {
			continue
		}
		row := WatchdogAgentLiveness{Name: name, State: watchdogStateUnknown, Paused: proc.Paused}
		if ready, ok := watchdog.FindCondition(proc.WatchdogConditions, watchdog.ConditionReady); ok {
			if ready.Reason != "" {
				row.State = ready.Reason
			}
			row.Message = ready.Message
			row.Since = formatOptionalTime(ready.LastTransitionTime)
		}
		if auth, ok := watchdog.FindCondition(proc.WatchdogConditions, watchdog.ConditionAuthenticated); ok {
			row.Authenticated = string(auth.Status)
		}
		if prod, ok := watchdog.FindCondition(proc.WatchdogConditions, watchdog.ConditionProducing); ok {
			row.Producing = string(prod.Status)
		}
		// A watchdog pause outranks the pane: the agent is dead BECAUSE the
		// watchdog gave up on it, and that is the state the operator needs.
		if proc.Paused && proc.PausedTrigger == watchdog.CrashLoopTrigger {
			row.State = watchdogStateCrashLoop
			row.Since = formatOptionalTime(proc.PausedAt)
			if proc.PausedReason != "" {
				row.Message = proc.PausedReason
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/watchdog"
)

// ── #7254: GET /api/watchdog/activity ────────────────────────────────────────

// watchdogActivityFixture writes an audit log that exercises every watchdog
// action name (taken and observed), interleaved with non-watchdog noise and
// with entries older than the window that must be excluded. Timestamps are
// relative to now so the test does not rot.
func watchdogActivityFixture(t *testing.T, now time.Time) string {
	t.Helper()
	dir := t.TempDir()
	day := 24 * time.Hour
	return writeAuditFixture(t, dir, []AuditEntry{
		// Today: one taken restart, one observed restart.
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), User: "watchdog", Action: watchdog.AuditActionRestart, Detail: "class=shell-prompt", Agent: "scanner"},
		{Timestamp: rfc3339(now.Add(-2 * time.Hour)), User: "watchdog", Action: watchdog.AuditActionRestart + watchdog.AuditObservedSuffix, Detail: "would restart", Agent: "guide"},
		// Three days ago: the whole escalation, observed.
		{Timestamp: rfc3339(now.Add(-3 * day)), User: "watchdog", Action: watchdog.AuditActionPause + watchdog.AuditObservedSuffix, Detail: "would pause", Agent: "guide"},
		{Timestamp: rfc3339(now.Add(-3 * day)), User: "watchdog", Action: watchdog.AuditActionGiveUp + watchdog.AuditObservedSuffix, Detail: "would give up", Agent: "guide"},
		{Timestamp: rfc3339(now.Add(-3*day + time.Minute)), User: "watchdog", Action: watchdog.AuditActionHealthyReset, Detail: "counter cleared", Agent: "guide"},
		// Ten days ago: a taken pause + give-up.
		{Timestamp: rfc3339(now.Add(-10 * day)), User: "watchdog", Action: watchdog.AuditActionPause, Detail: "paused", Agent: "quality"},
		{Timestamp: rfc3339(now.Add(-10 * day)), User: "watchdog", Action: watchdog.AuditActionGiveUp, Detail: "gave up", Agent: "quality"},
		// Noise the prefix filter must drop, including an action that merely
		// MENTIONS the watchdog and the operator's own mode change.
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), User: "alice", Action: "config_governor_watchdog", Detail: "section=watchdog, mode=observe"},
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), User: "alice", Action: "agent_restart", Detail: "trigger=dashboard", Agent: "scanner"},
		{Timestamp: rfc3339(now.Add(-5 * day)), User: "system", Action: "agent_pr_created", Detail: "repo=o/r, number=1"},
		// Older than the 30-day window: excluded even though it matches.
		{Timestamp: rfc3339(now.Add(-40 * day)), User: "watchdog", Action: watchdog.AuditActionRestart, Detail: "ancient", Agent: "scanner"},
		{Timestamp: rfc3339(now.Add(-31 * day)), User: "watchdog", Action: watchdog.AuditActionGiveUp, Detail: "ancient", Agent: "scanner"},
		// Malformed timestamp: skipped, never counted.
		{Timestamp: "yesterday", User: "watchdog", Action: watchdog.AuditActionRestart, Detail: "bad ts"},
	})
}

// watchdogActivityServer builds a bare server in the given watchdog mode and
// points the readout's audit scan at auditPath for the test's duration.
func watchdogActivityServer(t *testing.T, auditPath string, mode string) *Server {
	t.Helper()
	prev := watchdogActivityAuditPath
	watchdogActivityAuditPath = auditPath
	t.Cleanup(func() { watchdogActivityAuditPath = prev })
	cfg := &config.Config{}
	cfg.Governor.Watchdog.Mode = mode
	return &Server{
		audit: &AuditLog{},
		deps:  &Dependencies{Config: cfg},
	}
}

func getWatchdogActivity(t *testing.T, s *Server, query string) (*httptest.ResponseRecorder, WatchdogActivity) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/watchdog/activity"+query, nil)
	req.Header.Set("X-Hive-Role", config.RoleReadWrite)
	rec := httptest.NewRecorder()
	s.handleWatchdogActivity(rec, req)
	var body WatchdogActivity
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, body
}

// TestWatchdogActivityCountsEveryActionInWindow is the core contract: every
// watchdog-* action name is counted under its short key, taken and observed
// are split, noise is dropped, and entries older than the window are excluded.
func TestWatchdogActivityCountsEveryActionInWindow(t *testing.T) {
	now := time.Now().UTC()
	// Pin the clock away from midnight so "-1h" and "now" land on the same
	// UTC date and the today-bucket assertion below is stable.
	if now.Hour() < 3 {
		now = now.Add(3 * time.Hour)
	}
	p := watchdogActivityFixture(t, now)
	s := watchdogActivityServer(t, p, "observe")

	rec, a := getWatchdogActivity(t, s, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if a.Days != 30 {
		t.Errorf("default days = %d, want 30", a.Days)
	}
	if a.Mode != "observe" || a.Acting {
		t.Errorf("mode/acting = %q/%v, want observe/false", a.Mode, a.Acting)
	}
	if a.Total != 7 {
		t.Errorf("total = %d, want 7 (in-window watchdog-* entries only); byAction=%v", a.Total, a.ByAction)
	}
	if a.Taken != 4 || a.Observed != 3 {
		t.Errorf("taken/observed = %d/%d, want 4/3", a.Taken, a.Observed)
	}
	want := map[string]int{
		"restart":                  1,
		"restart-observed":         1,
		"crashloop-pause-observed": 1,
		"giveup-observed":          1,
		"healthy-reset":            1,
		"crashloop-pause":          1,
		"giveup":                   1,
	}
	for k, n := range want {
		if a.ByAction[k] != n {
			t.Errorf("byAction[%q] = %d, want %d", k, a.ByAction[k], n)
		}
	}
	for k := range a.ByAction {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected byAction key %q — the watchdog- prefix must be stripped and noise dropped", k)
		}
	}
	// Non-watchdog actions must not leak in under any key.
	for _, leaked := range []string{"config_governor_watchdog", "agent_restart", "agent_pr_created"} {
		if _, ok := a.ByAction[leaked]; ok {
			t.Errorf("non-watchdog action %q was counted", leaked)
		}
	}

	// Daily: 30 zero-filled buckets, oldest first, today last, with today
	// holding the two restarts.
	if len(a.Daily) != 30 {
		t.Fatalf("daily buckets = %d, want 30", len(a.Daily))
	}
	today := a.Daily[len(a.Daily)-1]
	if today.Date != now.Format(watchdogActivityDateLayout) {
		t.Errorf("last bucket date = %q, want today %q", today.Date, now.Format(watchdogActivityDateLayout))
	}
	if today.Count != 2 || today.ByAction["restart"] != 1 || today.ByAction["restart-observed"] != 1 {
		t.Errorf("today bucket = %+v, want count 2 (restart + restart-observed)", today)
	}
	sum := 0
	for i, d := range a.Daily {
		sum += d.Count
		if i > 0 && d.Date <= a.Daily[i-1].Date {
			t.Errorf("daily not ascending at %d: %q after %q", i, d.Date, a.Daily[i-1].Date)
		}
	}
	if sum != a.Total {
		t.Errorf("daily buckets sum to %d, total is %d — every counted entry must land in exactly one day", sum, a.Total)
	}
	if a.Since != watchdogActivitySince(now, 30).Format(time.RFC3339) {
		t.Errorf("since = %q, want %q", a.Since, watchdogActivitySince(now, 30).Format(time.RFC3339))
	}
	if a.LastAction != "restart" || a.LastActionAt == "" {
		t.Errorf("lastAction = %q at %q, want the newest entry (restart)", a.LastAction, a.LastActionAt)
	}
	// Severe verdicts in the window block promotion.
	if a.Promotion == nil || a.Promotion.Safe {
		t.Fatalf("promotion = %+v, want present and NOT safe (pause/give-up verdicts in window)", a.Promotion)
	}
	if !strings.Contains(a.Promotion.Reason, "4 crash-loop pause / give-up") {
		t.Errorf("promotion reason should count the 4 severe verdicts: %q", a.Promotion.Reason)
	}
}

// TestWatchdogActivityDaysParam covers the window control: a narrower window
// drops the older entries, out-of-range is clamped to retention, and garbage
// is a 400 rather than a silent default.
func TestWatchdogActivityDaysParam(t *testing.T) {
	now := time.Now().UTC()
	if now.Hour() < 3 {
		now = now.Add(3 * time.Hour)
	}
	p := watchdogActivityFixture(t, now)
	s := watchdogActivityServer(t, p, "observe")

	rec, a := getWatchdogActivity(t, s, "?days=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("days=2 status = %d", rec.Code)
	}
	if a.Days != 2 || len(a.Daily) != 2 {
		t.Errorf("days=2 → days %d / buckets %d, want 2/2", a.Days, len(a.Daily))
	}
	if a.Total != 2 {
		t.Errorf("days=2 total = %d, want 2 (only today's restarts)", a.Total)
	}
	if a.Promotion == nil || !a.Promotion.Safe {
		t.Errorf("days=2 promotion = %+v, want safe: no pause/give-up inside the window", a.Promotion)
	}

	rec, a = getWatchdogActivity(t, s, "?days=400")
	if rec.Code != http.StatusOK || a.Days != watchdogActivityMaxDays {
		t.Errorf("days=400 → status %d days %d, want 200 clamped to %d (audit retention)", rec.Code, a.Days, watchdogActivityMaxDays)
	}

	for _, bad := range []string{"?days=0", "?days=-3", "?days=soon"} {
		rec, _ := getWatchdogActivity(t, s, bad)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s → status %d, want 400", bad, rec.Code)
		}
	}
}

// TestWatchdogActivityHealModeHeadline: in heal the same entries are actions
// TAKEN, and there is no promotion hint — the decision has been made.
func TestWatchdogActivityHealModeHeadline(t *testing.T) {
	t.Setenv(watchdog.WatchdogPauseEnv, "")
	now := time.Now().UTC()
	p := watchdogActivityFixture(t, now)
	s := watchdogActivityServer(t, p, "heal")

	rec, a := getWatchdogActivity(t, s, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if a.Mode != "heal" || !a.Acting {
		t.Errorf("mode/acting = %q/%v, want heal/true", a.Mode, a.Acting)
	}
	if a.Promotion != nil {
		t.Errorf("heal mode must carry no promotion hint, got %+v", a.Promotion)
	}
}

// TestWatchdogActivityKillSwitchReportsObserve: with HIVE_WATCHDOG_PAUSE set a
// saved heal runs as observe, and the readout must say the mode in FORCE.
func TestWatchdogActivityKillSwitchReportsObserve(t *testing.T) {
	t.Setenv(watchdog.WatchdogPauseEnv, "1")
	now := time.Now().UTC()
	p := watchdogActivityFixture(t, now)
	s := watchdogActivityServer(t, p, "heal")

	_, a := getWatchdogActivity(t, s, "")
	if a.Mode != "observe" || a.Acting {
		t.Errorf("kill switch engaged: mode/acting = %q/%v, want observe/false", a.Mode, a.Acting)
	}
	if a.Promotion == nil {
		t.Error("downgraded-to-observe must still carry the promotion hint")
	}
}

// TestWatchdogActivityFallsBackToRingWithoutFile: a hive with no /data volume
// never writes a file; the ring is then the only record and must be used.
func TestWatchdogActivityFallsBackToRingWithoutFile(t *testing.T) {
	dir := t.TempDir()
	s := watchdogActivityServer(t, dir+"/never-written.jsonl", "observe")
	s.audit.Log("watchdog", watchdog.AuditActionRestart+watchdog.AuditObservedSuffix, "would restart", "scanner")
	s.audit.Log("alice", "config_save", "file=hive.yaml", "")

	rec, a := getWatchdogActivity(t, s, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if a.Total != 1 || a.ByAction["restart-observed"] != 1 {
		t.Errorf("ring fallback: total %d byAction %v, want the one observed restart", a.Total, a.ByAction)
	}
}

// TestWatchdogActivityZeroEventsIsAnAnswer is the motivating case from #7254:
// 6 h in Observe, nothing to act on. The strip must say "0" with a safe hint,
// not render nothing.
func TestWatchdogActivityZeroEventsIsAnAnswer(t *testing.T) {
	now := time.Now().UTC()
	p := writeAuditFixture(t, t.TempDir(), []AuditEntry{
		{Timestamp: rfc3339(now.Add(-time.Hour)), User: "alice", Action: "config_governor_watchdog", Detail: "mode=observe"},
	})
	s := watchdogActivityServer(t, p, "observe")

	_, a := getWatchdogActivity(t, s, "")
	if a.Total != 0 || len(a.Daily) != 30 {
		t.Errorf("total %d / buckets %d, want 0 / 30 zero-filled", a.Total, len(a.Daily))
	}
	if a.Promotion == nil || !a.Promotion.Safe || !strings.Contains(a.Promotion.Reason, "no would-have-acted events") {
		t.Errorf("promotion = %+v, want safe with the zero-events wording", a.Promotion)
	}
	if a.LastAction != "" || a.LastActionAt != "" {
		t.Errorf("no entries → lastAction must be empty, got %q/%q", a.LastAction, a.LastActionAt)
	}
}

// TestWatchdogActivityRoleGate mirrors GET /api/audit: the strip is derived
// from the audit log and is gated at the same tier, failing closed.
func TestWatchdogActivityRoleGate(t *testing.T) {
	s := watchdogActivityServer(t, "", "observe")
	for _, role := range []string{"", config.RoleRead} {
		req := httptest.NewRequest(http.MethodGet, "/api/watchdog/activity", nil)
		if role != "" {
			req.Header.Set("X-Hive-Role", role)
		}
		rec := httptest.NewRecorder()
		s.handleWatchdogActivity(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("role %q → status %d, want 403", role, rec.Code)
		}
	}
}

// TestWatchdogActivityRouteRegistered guards the mux wiring.
func TestWatchdogActivityRouteRegistered(t *testing.T) {
	s := newTestServer()
	s.RegisterAPI(&Dependencies{Config: &config.Config{}})
	req := httptest.NewRequest(http.MethodGet, "/api/watchdog/activity", nil)
	req.Header.Set("X-Hive-Role", config.RoleReadWrite)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/watchdog/activity via mux → %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// ── per-agent liveness ───────────────────────────────────────────────────────

func condAt(typ watchdog.ConditionType, status watchdog.ConditionStatus, reason, msg string, at time.Time) watchdog.Condition {
	return watchdog.Condition{Type: typ, Status: status, Reason: reason, Message: msg, LastTransitionTime: at}
}

func TestWatchdogAgentLivenessFromConditions(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	enabled := config.AgentConfig{Enabled: true}
	statuses := map[string]*agent.AgentProcess{
		"scanner": {Name: "scanner", Config: enabled, WatchdogConditions: []watchdog.Condition{
			condAt(watchdog.ConditionReady, watchdog.ConditionTrue, string(watchdog.ClassReady), "ready marker present", now.Add(-2*time.Hour)),
			condAt(watchdog.ConditionAuthenticated, watchdog.ConditionTrue, "ProbeOK", "", now),
			condAt(watchdog.ConditionProducing, watchdog.ConditionFalse, "NoRecentProduction", "", now),
		}},
		"guide": {Name: "guide", Config: enabled, WatchdogConditions: []watchdog.Condition{
			condAt(watchdog.ConditionReady, watchdog.ConditionFalse, string(watchdog.ClassStuckOverlay), "modal unanswered for 12m", now.Add(-12*time.Minute)),
		}},
		// Paused by the watchdog: the pane class is irrelevant, the state is
		// crash-loop and "since" is the pause time.
		"quality": {Name: "quality", Config: enabled, Paused: true, PausedTrigger: watchdog.CrashLoopTrigger,
			PausedAt: now.Add(-3 * time.Hour), PausedReason: "crash loop: 5 consecutive failed restarts",
			WatchdogConditions: []watchdog.Condition{
				condAt(watchdog.ConditionReady, watchdog.ConditionFalse, string(watchdog.ClassShellPrompt), "bare shell", now.Add(-time.Hour)),
			}},
		// Paused by an operator: NOT crash-loop; the pane verdict stands.
		"outreach": {Name: "outreach", Config: enabled, Paused: true, PausedTrigger: "dashboard-api",
			WatchdogConditions: []watchdog.Condition{
				condAt(watchdog.ConditionReady, watchdog.ConditionTrue, string(watchdog.ClassReady), "", now),
			}},
		// Never swept: no row, rather than a row implying health.
		"strategist": {Name: "strategist", Config: enabled},
		// Disabled in config: not supposed to be alive, so not listed.
		"telemetry": {Name: "telemetry", Config: config.AgentConfig{Enabled: false}, WatchdogConditions: []watchdog.Condition{
			condAt(watchdog.ConditionReady, watchdog.ConditionFalse, string(watchdog.ClassNoSession), "", now),
		}},
	}

	rows := watchdogAgentLiveness(statuses, nil)
	byName := map[string]WatchdogAgentLiveness{}
	var order []string
	for _, r := range rows {
		byName[r.Name] = r
		order = append(order, r.Name)
	}
	if strings.Join(order, ",") != "guide,outreach,quality,scanner" {
		t.Fatalf("rows = %v, want guide,outreach,quality,scanner (sorted; unswept + disabled omitted)", order)
	}

	if r := byName["scanner"]; r.State != "ready" || r.Since != now.Add(-2*time.Hour).Format(time.RFC3339) || r.Authenticated != "True" || r.Producing != "False" || r.Paused {
		t.Errorf("scanner = %+v", r)
	}
	if r := byName["guide"]; r.State != "stuck-overlay" || r.Message != "modal unanswered for 12m" {
		t.Errorf("guide = %+v", r)
	}
	if r := byName["quality"]; r.State != watchdogStateCrashLoop || r.Since != now.Add(-3*time.Hour).Format(time.RFC3339) || !r.Paused || !strings.Contains(r.Message, "crash loop") {
		t.Errorf("quality = %+v, want crash-loop since the pause", r)
	}
	if r := byName["outreach"]; r.State != "ready" || !r.Paused {
		t.Errorf("outreach = %+v, want ready + paused (operator pause is not a crash loop)", r)
	}
}

func TestWatchdogAgentLivenessLiveConfigWins(t *testing.T) {
	now := time.Now().UTC()
	statuses := map[string]*agent.AgentProcess{
		"scanner": {Name: "scanner", Config: config.AgentConfig{Enabled: true}, WatchdogConditions: []watchdog.Condition{
			condAt(watchdog.ConditionReady, watchdog.ConditionTrue, "ready", "", now),
		}},
	}
	cfg := &config.Config{Agents: map[string]config.AgentConfig{"scanner": {Enabled: false}}}
	if rows := watchdogAgentLiveness(statuses, cfg); len(rows) != 0 {
		t.Errorf("agent disabled in LIVE config must be omitted, got %+v", rows)
	}
	if rows := watchdogAgentLiveness(nil, cfg); len(rows) != 0 {
		t.Errorf("nil statuses → no rows, got %+v", rows)
	}
}

// ── audit scanner ────────────────────────────────────────────────────────────

func TestActionsWithPrefixSince(t *testing.T) {
	now := time.Now().UTC()
	p := writeAuditFixture(t, t.TempDir(), []AuditEntry{
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), Action: "watchdog-restart", Agent: "a"},
		{Timestamp: rfc3339(now.Add(-2 * time.Hour)), Action: "watchdog-giveup-observed", Agent: "b"},
		{Timestamp: rfc3339(now.Add(-3 * time.Hour)), Action: "config_governor_watchdog"}, // contains the word, wrong prefix
		{Timestamp: rfc3339(now.Add(-30 * time.Hour)), Action: "watchdog-restart", Agent: "old"},
	})
	got := (&AuditLog{}).ActionsWithPrefixSince(now.Add(-24*time.Hour), "watchdog-", p)
	if len(got) != 2 {
		t.Fatalf("want 2 prefixed in-window entries, got %d: %+v", len(got), got)
	}
	if got[0].Agent != "b" || got[1].Agent != "a" {
		t.Errorf("want oldest first (b, a), got %s, %s", got[0].Agent, got[1].Agent)
	}
	if !(&AuditLog{}).HasOnDiskLog(p) {
		t.Error("HasOnDiskLog must see the fixture")
	}
	if (&AuditLog{}).HasOnDiskLog(p + ".missing") {
		t.Error("HasOnDiskLog must be false for a path with no files")
	}
}

// TestOutputActionsSinceUnchangedByRefactor pins the pre-existing caller's
// behaviour through the shared scanner: an empty action set means "all".
func TestOutputActionsSinceUnchangedByRefactor(t *testing.T) {
	now := time.Now().UTC()
	p := writeAuditFixture(t, t.TempDir(), []AuditEntry{
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), Action: "x"},
		{Timestamp: rfc3339(now.Add(-2 * time.Hour)), Action: "y"},
	})
	if got := (&AuditLog{}).OutputActionsSince(now.Add(-24*time.Hour), nil, p); len(got) != 2 {
		t.Errorf("nil action set must match every action, got %d", len(got))
	}
	if got := (&AuditLog{}).OutputActionsSince(now.Add(-24*time.Hour), map[string]bool{"y": true}, p); len(got) != 1 || got[0].Action != "y" {
		t.Errorf("action set filter broken: %+v", got)
	}
}

package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// baseNow is a fixed reference time so age-based cases are deterministic.
var advNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// rfc3339Ago formats a time `d` before advNow, as the spoke would report it.
func rfc3339Ago(d time.Duration) string {
	return advNow.Add(-d).UTC().Format(time.RFC3339)
}

// advisoryModeEntry is a hive that IS in advisory mode (has posted a digest)
// with an App that can write — the only shape that is ever eligible to be
// flagged stale. Individual cases override fields to exercise each gate.
func advisoryModeEntry() RegistryEntry {
	return RegistryEntry{
		ID:                   "hosted-adv",
		AdvisoryLastPostedAt: rfc3339Ago(5 * time.Minute), // fresh by default
		// App can write: no banner, no pending install, state OK.
		GitHubAppRequired:       false,
		PendingGitHubAppInstall: false,
		GitHubAppState:          "ok",
	}
}

func TestAdvisoryStale_FreshDigestNotStale(t *testing.T) {
	e := advisoryModeEntry() // posted 5 min ago, well within threshold
	if stale, reason := advisoryStale(e, advNow); stale {
		t.Fatalf("a freshly-posted digest must not be stale, got reason %q", reason)
	}
}

// TestAdvisoryStaleThresholdCoversMaxUpdateInterval pins the invariant that
// makes governor.advisory.update_interval_s (#4820) safe: the hub's staleness
// baseline is effectively max(configured interval, default cadence) because
// the SLOWEST legal cadence stays at least 30 minutes under the alarm
// threshold. If either side moves — the threshold shrinks or the config max
// grows — a hive healthily posting at its configured maximum would false-alarm
// as wedged, and this test fails first.
func TestAdvisoryStaleThresholdCoversMaxUpdateInterval(t *testing.T) {
	maxCadence := time.Duration(config.MaxAdvisoryUpdateIntervalS) * time.Second
	if advisoryStaleThreshold < maxCadence+30*time.Minute {
		t.Fatalf("advisoryStaleThreshold (%v) must exceed the max configurable advisory update interval (%v) by ≥30m of slack",
			advisoryStaleThreshold, maxCadence)
	}
}

// TestAdvisoryStale_HealthyLongCadenceNotWedged is the behavioral half of the
// invariant above: a hive whose operator slowed the digest to the maximum
// allowed hourly cadence, and which last posted a full such interval ago (plus
// eval-cycle slack), must NOT light the wedged-digest pill.
func TestAdvisoryStale_HealthyLongCadenceNotWedged(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = rfc3339Ago(time.Duration(config.MaxAdvisoryUpdateIntervalS)*time.Second + 5*time.Minute)
	if stale, reason := advisoryStale(e, advNow); stale {
		t.Fatalf("a healthy max-interval cadence must not read as wedged, got reason %q", reason)
	}
}

func TestAdvisoryStale_PastThresholdIsStale(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = rfc3339Ago(advisoryStaleThreshold + time.Minute)
	stale, reason := advisoryStale(e, advNow)
	if !stale {
		t.Fatalf("a digest older than the threshold must be stale")
	}
	if reason == "" {
		t.Fatalf("a stale flag must carry a reason for the tooltip")
	}
}

// pausedAgent / offScheduleAgent / activeAgent build the AgentSummary shapes
// the quiet-by-design gate discriminates. ExpectedActive/Enabled/CanOpenIssue
// are set so none of them read as legacy (legacyAgent must return false).
func pausedAgent(name string) AgentSummary {
	return AgentSummary{Name: name, State: agentStatePaused, Paused: true, Enabled: true, CanOpenIssue: true}
}
func offScheduleAgent(name string) AgentSummary {
	return AgentSummary{Name: name, State: "idle", ExpectedActive: false, Enabled: true, CanOpenIssue: true}
}
func activeAgent(name string) AgentSummary {
	return AgentSummary{Name: name, State: agentStateRunning, ExpectedActive: true, Enabled: true, CanOpenIssue: true}
}

// An aged digest on a hive whose EVERY agent is deliberately quiet (paused or
// off-schedule) is the operator's own pause, not a wedge — never a pill.
// An off-schedule agent whose persistent session is still alive
// (state=running) is quiet all the same — the suppression must not require
// the session to be torn down.
func TestAdvisoryStale_OffScheduleRunningSessionStillSuppresses(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = rfc3339Ago(advisoryStaleThreshold + time.Hour)
	e.Agents = []AgentSummary{
		pausedAgent("supervisor"),
		{Name: "scanner", State: agentStateRunning, ExpectedActive: false, Enabled: true, CanOpenIssue: true},
	}
	if stale, reason := advisoryStale(e, advNow); stale {
		t.Fatalf("an off-schedule idle session must still count as quiet, got %q", reason)
	}
}

func TestAdvisoryStale_AllAgentsQuietSuppressesAgedTimestamp(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = rfc3339Ago(advisoryStaleThreshold + time.Hour)
	e.Agents = []AgentSummary{pausedAgent("scanner"), offScheduleAgent("guide")}
	if stale, reason := advisoryStale(e, advNow); stale {
		t.Fatalf("all agents quiet by design must suppress the aged-digest alarm, got %q", reason)
	}
}

// A reported post ERROR is a broken post path regardless of who is paused —
// the quiet gate must not swallow it.
func TestAdvisoryStale_AllAgentsQuietDoesNotSuppressError(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryError = "403 posting digest comment"
	e.Agents = []AgentSummary{pausedAgent("scanner"), pausedAgent("guide")}
	if stale, _ := advisoryStale(e, advNow); !stale {
		t.Fatalf("a reported post error must stay stale even with every agent paused")
	}
}

// One working agent means findings are expected — the aged alarm stands.
func TestAdvisoryStale_OneActiveAgentKeepsAgedAlarm(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = rfc3339Ago(advisoryStaleThreshold + time.Hour)
	e.Agents = []AgentSummary{pausedAgent("scanner"), activeAgent("guide")}
	if stale, _ := advisoryStale(e, advNow); !stale {
		t.Fatalf("an active agent must keep the aged-digest alarm")
	}
}

// Unknowns never suppress: no reported agents, or a legacy agent that reports
// none of the new protocol fields, must leave the alarm in place.
func TestAdvisoryStale_UnknownAgentsNeverSuppress(t *testing.T) {
	aged := func() RegistryEntry {
		e := advisoryModeEntry()
		e.AdvisoryLastPostedAt = rfc3339Ago(advisoryStaleThreshold + time.Hour)
		return e
	}
	e := aged() // no agents reported at all
	if stale, _ := advisoryStale(e, advNow); !stale {
		t.Fatalf("a hive reporting no agents must not have the alarm suppressed")
	}
	e = aged()
	e.Agents = []AgentSummary{{Name: "old"}} // legacy shape: no new-protocol fields
	if stale, _ := advisoryStale(e, advNow); !stale {
		t.Fatalf("a legacy agent entry must not suppress the alarm")
	}
}

func TestAdvisoryStale_ErrorIsStaleWithCause(t *testing.T) {
	e := advisoryModeEntry() // fresh timestamp, but the last attempt errored
	e.AdvisoryError = "403 Resource not accessible by integration"
	stale, reason := advisoryStale(e, advNow)
	if !stale {
		t.Fatalf("a reported post error must flag the digest stale even if the timestamp is fresh")
	}
	if reason == "" || reason == "advisory digest has gone stale" {
		t.Fatalf("the error cause must appear in the reason, got %q", reason)
	}
}

// GATE 1 — not in advisory mode: a hive that has never posted a digest and
// reports no error (pure PR/merge mode, or an old spoke) is UNKNOWN, never stale.
func TestAdvisoryStale_NotAdvisoryModeNeverStale(t *testing.T) {
	e := RegistryEntry{
		ID:                   "hosted-prmode",
		AdvisoryLastPostedAt: "", // never posted
		AdvisoryError:        "",
		GitHubAppState:       "ok", // App is perfectly healthy — irrelevant here
	}
	if stale, _ := advisoryStale(e, advNow); stale {
		t.Fatalf("a hive that never posts advisories must never be flagged stale")
	}
}

// GATE 2 — App cannot write: a not-installed hive (or one running without
// credentials / with an undelivered key / a raised install banner) legitimately
// cannot post, so an aged timestamp must NOT alarm.
func TestAdvisoryStale_AppCannotWriteNeverStale(t *testing.T) {
	old := rfc3339Ago(advisoryStaleThreshold + time.Hour) // very stale timestamp

	cases := []struct {
		name  string
		mutfn func(*RegistryEntry)
	}{
		{"not-installed state", func(e *RegistryEntry) { e.GitHubAppState = "not-installed" }},
		{"key-missing state", func(e *RegistryEntry) { e.GitHubAppState = "key-missing" }},
		{"key-invalid state", func(e *RegistryEntry) { e.GitHubAppState = "key-invalid" }},
		{"wrong-installation state", func(e *RegistryEntry) { e.GitHubAppState = "wrong-installation" }},
		{"insufficient-permissions state", func(e *RegistryEntry) { e.GitHubAppState = "insufficient-permissions" }},
		{"app-required banner raised", func(e *RegistryEntry) { e.GitHubAppRequired = true }},
		{"pending install", func(e *RegistryEntry) { e.PendingGitHubAppInstall = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := advisoryModeEntry()
			e.AdvisoryLastPostedAt = old
			tc.mutfn(&e)
			if stale, reason := advisoryStale(e, advNow); stale {
				t.Fatalf("app-cannot-write hive must not alarm, got stale=%v reason=%q", stale, reason)
			}
		})
	}
}

// GATE 2 (error variant) — even a reported post ERROR must not alarm when the
// App cannot write: the error is the EXPECTED symptom of the missing
// credential, not a broken digest path.
func TestAdvisoryStale_AppCannotWriteSuppressesError(t *testing.T) {
	e := advisoryModeEntry()
	e.GitHubAppState = "not-installed"
	e.AdvisoryError = "403 Resource not accessible by integration"
	if stale, _ := advisoryStale(e, advNow); stale {
		t.Fatalf("a not-installed hive's post error must not alarm — it is expected")
	}
}

// Unknown/empty timestamp with no error is UNKNOWN, never a false alarm — even
// on an app-can-write hive. (Distinct from gate 1: here a malformed value could
// arrive; it must be treated the same as absence.)
func TestAdvisoryStale_UnparseableTimestampNeverStale(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = "not-a-timestamp"
	if stale, _ := advisoryStale(e, advNow); stale {
		t.Fatalf("an unparseable timestamp must be treated as UNKNOWN, not stale")
	}
}

// appCanWriteForAdvisory: empty/unknown state on a hive with no banner is
// writable-unless-proven-otherwise (a genuinely posting hive reports no error).
func TestAppCanWriteForAdvisory_EmptyStateIsWritable(t *testing.T) {
	if !appCanWriteForAdvisory(RegistryEntry{GitHubAppState: ""}) {
		t.Fatalf("empty app state with no banner should be treated as writable")
	}
	if !appCanWriteForAdvisory(RegistryEntry{GitHubAppState: "unknown"}) {
		t.Fatalf("unknown app state with no banner should be treated as writable")
	}
}

// TestHeartbeatPayload_CarriesAdvisoryFields proves the new fields survive the
// JSON round-trip the heartbeat actually uses (spoke marshals, hub unmarshals),
// and that empty values omit cleanly so an old spoke's payload is unchanged.
func TestHeartbeatPayload_CarriesAdvisoryFields(t *testing.T) {
	posted := advNow.Format(time.RFC3339)
	in := HeartbeatPayload{
		HiveID:               "hosted-adv",
		AdvisoryLastPostedAt: posted,
		AdvisoryError:        "rate limit",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out HeartbeatPayload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.AdvisoryLastPostedAt != posted {
		t.Fatalf("advisory_last_posted_at lost in round-trip: got %q", out.AdvisoryLastPostedAt)
	}
	if out.AdvisoryError != "rate limit" {
		t.Fatalf("advisory_error lost in round-trip: got %q", out.AdvisoryError)
	}

	// Empty fields must be omitted so an old spoke's wire format is untouched.
	empty, _ := json.Marshal(HeartbeatPayload{HiveID: "x"})
	var m map[string]any
	json.Unmarshal(empty, &m)
	if _, ok := m["advisory_last_posted_at"]; ok {
		t.Fatalf("advisory_last_posted_at must be omitted when empty")
	}
	if _, ok := m["advisory_error"]; ok {
		t.Fatalf("advisory_error must be omitted when empty")
	}
}

// A spoke that has NEVER posted since its last restart but reports a post error
// is a participant with a broken digest path — exactly the #4167 shape, where
// the advisory issue could not be resolved so nothing was ever posted. Gate 1
// must accept the error alone as proof of participation, otherwise the wedge
// stays invisible for as long as the process lives.
func TestAdvisoryStale_NeverPostedButErroredIsStale(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = "" // no successful post since restart
	e.AdvisoryError = "no advisory issue resolved for org/repo — digest not posted"
	stale, reason := advisoryStale(e, advNow)
	if !stale {
		t.Fatal("a reported post error with no successful post must still flag the digest stale")
	}
	if !strings.Contains(reason, "no advisory issue resolved") {
		t.Fatalf("the reason must carry the reported cause, got %q", reason)
	}
}

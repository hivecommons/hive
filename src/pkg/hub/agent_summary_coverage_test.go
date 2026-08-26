package hub

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// NewAgentSummary exists so the two heartbeat build sites in cmd/hive fill
// AgentSummary identically. The contract that actually matters is the
// timestamp one: a zero time must serialise as EMPTY, not as "0001-01-01T...".
// The hub reads an unparseable timestamp as UNKNOWN and an old one as stale,
// so emitting year 1 for "never observed" would make every agent on a spoke
// that does not report activity look maximally idle — the exact false alarm
// classifyInactiveAgent's parseAgentTime guard is written to avoid.
func TestNewAgentSummaryLeavesZeroTimestampsEmpty(t *testing.T) {
	as := NewAgentSummary("scanner", agentStateRunning, "continuous", AgentActivity{})

	if as.StartedAt != "" {
		t.Errorf("zero StartedAt serialised as %q, want empty", as.StartedAt)
	}
	if as.LastActivityAt != "" {
		t.Errorf("zero LastActivityAt serialised as %q, want empty", as.LastActivityAt)
	}

	// Prove the consumer side agrees: the hub must treat these as UNKNOWN.
	if _, ok := parseAgentTime(as.StartedAt); ok {
		t.Error("empty StartedAt must parse as unknown")
	}
	if _, ok := parseAgentTime(as.LastActivityAt); ok {
		t.Error("empty LastActivityAt must parse as unknown")
	}

	// And that unknown activity never yields an idle verdict even with work
	// queued and a start time long past the grace period.
	as.StartedAt = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	if got := classifyInactiveAgent(as, 50, time.Now()); got != agentInactiveNone {
		t.Errorf("unknown last-activity classified as %v, want none", got)
	}
}

// A populated activity struct must round-trip through RFC3339 in UTC, because
// the hub parses these with time.RFC3339 and a non-UTC or differently
// formatted value would read as unknown.
func TestNewAgentSummaryFormatsTimestampsAsUTCRFC3339(t *testing.T) {
	// Deliberately a non-UTC zone: the conversion must happen.
	zone := time.FixedZone("UTC+5", 5*60*60)
	started := time.Date(2026, 7, 31, 12, 0, 0, 0, zone)
	active := time.Date(2026, 7, 31, 12, 30, 0, 0, zone)

	as := NewAgentSummary("reviewer", agentStateRunning, agentModeOnDemand, AgentActivity{
		Paused:         true,
		NeedsLogin:     true,
		SessionMissing: true,
		StartedAt:      started,
		LastActivityAt: active,
	})

	if as.Name != "reviewer" || as.State != agentStateRunning || as.Mode != agentModeOnDemand {
		t.Errorf("identity fields not copied: %+v", as)
	}
	if !as.Paused || !as.NeedsLogin || !as.SessionMissing {
		t.Errorf("activity flags not copied: %+v", as)
	}

	// 12:00 at UTC+5 is 07:00Z. Asserting the exact instant catches both a
	// missing .UTC() and a wrong layout.
	if as.StartedAt != "2026-07-31T07:00:00Z" {
		t.Errorf("StartedAt = %q, want 2026-07-31T07:00:00Z", as.StartedAt)
	}
	if as.LastActivityAt != "2026-07-31T07:30:00Z" {
		t.Errorf("LastActivityAt = %q, want 2026-07-31T07:30:00Z", as.LastActivityAt)
	}

	got, ok := parseAgentTime(as.StartedAt)
	if !ok {
		t.Fatal("hub cannot parse the timestamp NewAgentSummary produced")
	}
	if !got.Equal(started) {
		t.Errorf("round-tripped instant %v != original %v", got, started)
	}
}

// The zero-timestamp contract has to survive JSON, since that is how the
// summary actually reaches the hub. omitempty on the wire is what keeps a
// "never observed" agent from arriving as a year-1 string.
func TestAgentSummaryZeroTimestampsOmittedFromJSON(t *testing.T) {
	b, err := json.Marshal(NewAgentSummary("scanner", agentStateRunning, "continuous", AgentActivity{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("year-1 timestamp leaked onto the wire: %s", b)
	}
	if strings.Contains(string(b), "kickIntervalSec") {
		t.Errorf("zero kick interval leaked onto the wire: %s", b)
	}
}

func TestNewAgentSummaryCarriesKickInterval(t *testing.T) {
	want := int64(2 * time.Hour / time.Second)
	as := NewAgentSummary("quality", agentStateRunning, "continuous", AgentActivity{
		KickInterval: 2 * time.Hour,
	})
	if as.KickIntervalSec != want {
		t.Fatalf("KickIntervalSec = %d, want %d", as.KickIntervalSec, want)
	}

	b, err := json.Marshal(as)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kickIntervalSec":`+strconv.FormatInt(want, 10)) {
		t.Fatalf("kick interval missing from heartbeat JSON: %s", b)
	}
}

// label() feeds the operator-visible alert text. Every kind that can be
// returned by classifyInactiveAgent must name itself, and the none/unknown
// case must render as empty so it can never contribute a bare parenthetical
// to the reason sentence.
func TestAgentInactivityKindLabels(t *testing.T) {
	for _, tc := range []struct {
		kind agentInactivityKind
		want string
	}{
		{agentInactiveSessionMissing, "session gone"},
		{agentInactiveNeedsLogin, "not logged in"},
		{agentInactiveIdleWithWork, "idle with work queued"},
		{agentInactiveNone, ""},
		{agentInactivityKind(99), ""},
	} {
		if got := tc.kind.label(); got != tc.want {
			t.Errorf("kind %d label = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// An agent whose kind has no label must not appear in the reason sentence at
// all. This pins the pairing between evaluateInactiveAgents and label(): if a
// new kind were added and left unlabelled, the alert would read
// "scanner ()" rather than failing loudly.
func TestEveryReportedKindContributesANonEmptyLabel(t *testing.T) {
	now := time.Now()
	old := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	stale := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)

	agents := []AgentSummary{
		{Name: "zombie", State: agentStateRunning, SessionMissing: true},
		{Name: "unauthed", State: agentStateRunning, NeedsLogin: true, StartedAt: old},
		{Name: "idle", State: agentStateRunning, StartedAt: old, LastActivityAt: stale},
	}

	rep := evaluateInactiveAgents(agents, 5, now)
	if rep.Count != 3 {
		t.Fatalf("Count = %d, want 3 (reason: %q)", rep.Count, rep.Reason)
	}
	if strings.Contains(rep.Reason, "()") {
		t.Errorf("an unlabelled kind reached the operator text: %q", rep.Reason)
	}
	for _, want := range []string{"session gone", "not logged in", "idle with work queued"} {
		if !strings.Contains(rep.Reason, want) {
			t.Errorf("reason %q is missing %q", rep.Reason, want)
		}
	}
}

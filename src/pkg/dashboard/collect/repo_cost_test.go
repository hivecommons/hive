package collect

import (
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/tokens"
)

func rcTime(base time.Time, min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

func rcEvent(ts time.Time, agent, repo string) AuditEntry {
	return AuditEntry{
		Timestamp: ts.UTC().Format(time.RFC3339),
		Action:    ghpkg.AuditActionAgentPRCreated,
		Agent:     agent,
		Detail:    "agent=" + agent + ", repo=" + repo + ", number=1",
	}
}

func rcUsage(ts time.Time, in, out int64) tokens.UsageEvent {
	return tokens.UsageEvent{TimestampMs: ts.UnixMilli(), Model: "claude-opus-4-1", Coalesced: 1, Input: in, Output: out}
}

// rcSession builds a Claude session whose summed fields agree with its timeline,
// exactly as the phase-2 scanner produces.
func rcSession(id, agent string, usage ...tokens.UsageEvent) tokens.SessionSummary {
	s := tokens.SessionSummary{SessionID: id, Agent: agent, Model: "claude-opus-4-1", Backend: tokens.BackendClaude, Usage: usage}
	for _, u := range usage {
		s.InputTokens += u.Input
		s.OutputTokens += u.Output
		s.CacheRead += u.CacheRead
		s.CacheCreate += u.CacheCreate
	}
	s.TotalTokens = s.InputTokens + s.OutputTokens + s.CacheRead + s.CacheCreate
	return s
}

func findRepo(t *testing.T, resp RepoCostResponse, repo string) RepoCostEntry {
	t.Helper()
	for _, e := range resp.ByRepo {
		if e.Repo == repo {
			return e
		}
	}
	t.Fatalf("repo %q not in by_repo: %+v", repo, resp.ByRepo)
	return RepoCostEntry{}
}

// TestRepoCostPartitionInvariant is the epic's hard requirement #1, written as
// an actual test: every token the hive counted lands in exactly one bucket, and
// the three buckets sum to the hive total. This is the assertion that makes the
// per-repo number trustworthy — without it a repo figure could silently be a
// fraction of reality.
func TestRepoCostPartitionInvariant(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-24 * time.Hour)

	summary := &tokens.AggregateSummary{
		Sessions: []tokens.SessionSummary{
			// Attributable: usage between two repo events.
			rcSession("s1", "scanner",
				rcUsage(rcTime(base, 5), 100, 10),  // leading -> unattributed
				rcUsage(rcTime(base, 15), 200, 20), // between e@10 and e@20 -> repo-b
				rcUsage(rcTime(base, 25), 300, 30), // trailing, no closer -> unattributed
			),
			// Agent with no repo events at all -> entirely unattributed.
			rcSession("s2", "lonely", rcUsage(rcTime(base, 15), 400, 40)),
			// Non-time-resolved Copilot backend -> backend_unsupported, in full.
			{SessionID: "s3", Agent: "reviewer", Model: "gpt-5", Backend: tokens.BackendCopilot,
				InputTokens: 500, OutputTokens: 50, TotalTokens: 550},
			// Claude session with NO timeline (e.g. a pre-phase-2 persisted
			// snapshot) must not vanish.
			{SessionID: "s4", Agent: "scanner", Model: "claude-opus-4-1", Backend: tokens.BackendClaude,
				InputTokens: 600, OutputTokens: 60, TotalTokens: 660},
		},
	}
	var hiveTotal int64
	for _, s := range summary.Sessions {
		hiveTotal += s.TotalTokens
	}

	entries := []AuditEntry{
		rcEvent(rcTime(base, 10), "scanner", "org/repo-a"),
		rcEvent(rcTime(base, 20), "scanner", "org/repo-b"),
	}

	resp := ComputeRepoCost(summary, entries, now)

	sum := resp.Unattributed.Tokens + resp.BackendUnsupported.Tokens
	for _, e := range resp.ByRepo {
		sum += e.Tokens
	}
	if sum != hiveTotal {
		t.Fatalf("partition broken: Σby_repo+unattributed+backend_unsupported = %d, hive total = %d", sum, hiveTotal)
	}
	if resp.TotalTokens != hiveTotal {
		t.Fatalf("TotalTokens = %d, want %d", resp.TotalTokens, hiveTotal)
	}
	if resp.AttributedTokens+resp.Unattributed.Tokens+resp.BackendUnsupported.Tokens != hiveTotal {
		t.Fatalf("AttributedTokens does not complete the partition")
	}
}

func TestRepoCostPartitionInvariantWithCopilotLiveCapture(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-24 * time.Hour)

	liveCopilot := rcSession("copilot-live-scanner", "scanner",
		tokens.UsageEvent{TimestampMs: rcTime(base, 15).UnixMilli(), Model: "gpt-5", Coalesced: 1, Input: 300, Output: 30},
		tokens.UsageEvent{TimestampMs: rcTime(base, 25).UnixMilli(), Model: "gpt-5", Coalesced: 1, Input: 200, Output: 20},
	)
	liveCopilot.Backend = tokens.BackendCopilot
	liveCopilot.Model = "gpt-5"

	summary := &tokens.AggregateSummary{
		Sessions: []tokens.SessionSummary{
			liveCopilot,
			// The scanner's session.shutdown copy for the same active Copilot
			// session is zeroed once live capture starts; if repo-cost summed
			// scanner and sink totals independently, this test would exceed the
			// hive total below.
			{SessionID: "copilot-shutdown-active", Agent: "scanner", Model: "gpt-5", Backend: tokens.BackendCopilot},
			// Historical Copilot sessions without live per-request capture still
			// contribute once, under backend_unsupported.
			{SessionID: "copilot-before-live", Agent: "scanner", Model: "gpt-5", Backend: tokens.BackendCopilot, InputTokens: 50, OutputTokens: 5, TotalTokens: 55},
		},
	}
	var hiveTotal int64
	for _, s := range summary.Sessions {
		hiveTotal += s.TotalTokens
	}

	entries := []AuditEntry{
		rcEvent(rcTime(base, 10), "scanner", "org/repo-a"),
		rcEvent(rcTime(base, 20), "scanner", "org/repo-b"),
	}
	resp := ComputeRepoCost(summary, entries, now)

	if got := findRepo(t, resp, "org/repo-b").Tokens; got != 330 {
		t.Fatalf("repo-b Copilot tokens = %d, want 330", got)
	}
	if resp.Unattributed.Tokens != 220 {
		t.Fatalf("unattributed Copilot tokens = %d, want trailing live event 220", resp.Unattributed.Tokens)
	}
	if resp.BackendUnsupported.Tokens != 55 {
		t.Fatalf("backend_unsupported tokens = %d, want historical Copilot 55", resp.BackendUnsupported.Tokens)
	}
	sum := resp.Unattributed.Tokens + resp.BackendUnsupported.Tokens
	for _, e := range resp.ByRepo {
		sum += e.Tokens
	}
	if sum != hiveTotal {
		t.Fatalf("partition broken with Copilot live capture: got %d, hive total %d", sum, hiveTotal)
	}
}

// TestRepoCostAttributesToClosingEvent pins the join rule: usage between two
// events belongs to the LATER (closing) one.
func TestRepoCostAttributesToClosingEvent(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-24 * time.Hour)

	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s1", "scanner", rcUsage(rcTime(base, 15), 1000, 100)),
	}}
	entries := []AuditEntry{
		rcEvent(rcTime(base, 10), "scanner", "org/repo-a"),
		rcEvent(rcTime(base, 20), "scanner", "org/repo-b"),
	}

	resp := ComputeRepoCost(summary, entries, now)
	b := findRepo(t, resp, "org/repo-b")
	if b.Tokens != 1100 {
		t.Fatalf("closing repo tokens = %d, want 1100", b.Tokens)
	}
	for _, e := range resp.ByRepo {
		if e.Repo == "org/repo-a" && e.Tokens > 0 {
			t.Fatalf("tokens billed to the OPENING event's repo: %+v", e)
		}
	}
}

// TestRepoCostNeverAttributesLeadingInterval is the epic's explicit "never
// attribute the leading interval to the first repo seen".
func TestRepoCostNeverAttributesLeadingInterval(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-24 * time.Hour)

	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s1", "scanner", rcUsage(rcTime(base, 5), 999, 1)),
	}}
	entries := []AuditEntry{rcEvent(rcTime(base, 10), "scanner", "org/repo-a")}

	resp := ComputeRepoCost(summary, entries, now)
	if len(resp.ByRepo) != 0 {
		t.Fatalf("leading interval was attributed: %+v", resp.ByRepo)
	}
	if resp.Unattributed.Tokens != 1000 {
		t.Fatalf("unattributed = %d, want 1000", resp.Unattributed.Tokens)
	}
}

// TestRepoCostBackendUnsupportedCarriesNonClaude asserts non-Claude spend is
// reported in FULL rather than dropped — it is real money, just unplaceable.
func TestRepoCostBackendUnsupportedCarriesNonClaude(t *testing.T) {
	now := time.Now().UTC()
	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		{SessionID: "c1", Agent: "a", Model: "gpt-5", Backend: tokens.BackendCopilot, InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
		{SessionID: "b1", Agent: "a", Model: "bob", Backend: tokens.BackendBob, InputTokens: 200, OutputTokens: 20, TotalTokens: 220},
	}}
	resp := ComputeRepoCost(summary, nil, now)
	if resp.BackendUnsupported.Tokens != 330 {
		t.Fatalf("backend_unsupported tokens = %d, want 330", resp.BackendUnsupported.Tokens)
	}
	if resp.BackendUnsupported.USD == nil || *resp.BackendUnsupported.USD <= 0 {
		t.Fatal("backend_unsupported must carry a real dollar figure, not nil/zero")
	}
}

// TestRepoCostNilNotZero: hard requirement #3. An untouched bucket reports null
// so the UI renders "—", never "$0.00".
func TestRepoCostNilNotZero(t *testing.T) {
	resp := ComputeRepoCost(&tokens.AggregateSummary{}, nil, time.Now())
	if resp.Unattributed.USD != nil {
		t.Fatalf("unattributed USD = %v, want nil (no cost attributed != $0.00)", *resp.Unattributed.USD)
	}
	if resp.BackendUnsupported.USD != nil {
		t.Fatalf("backend_unsupported USD = %v, want nil", *resp.BackendUnsupported.USD)
	}
	// The mandatory buckets must still be PRESENT even when empty.
	if resp.Unattributed.Repo != RepoCostBucketUnattributed || resp.BackendUnsupported.Repo != RepoCostBucketBackendUnsupported {
		t.Fatalf("mandatory buckets missing: %+v %+v", resp.Unattributed, resp.BackendUnsupported)
	}
}

// TestRepoCostUnknownTimestampIsUnattributed: a usage event that carries no
// parseable timestamp cannot be placed and must not be guessed into a repo.
func TestRepoCostUnknownTimestampIsUnattributed(t *testing.T) {
	now := time.Now().UTC()
	base := now.Add(-24 * time.Hour)
	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s1", "scanner",
			tokens.UsageEvent{TimestampMs: 0, Coalesced: 1, Input: 500},
			rcUsage(rcTime(base, 15), 100, 0),
		),
	}}
	entries := []AuditEntry{
		rcEvent(rcTime(base, 10), "scanner", "org/repo-a"),
		rcEvent(rcTime(base, 20), "scanner", "org/repo-b"),
	}
	resp := ComputeRepoCost(summary, entries, now)
	if resp.Unattributed.Tokens != 500 {
		t.Fatalf("unattributed = %d, want 500 (the zero-timestamp event)", resp.Unattributed.Tokens)
	}
	if findRepo(t, resp, "org/repo-b").Tokens != 100 {
		t.Fatal("timestamped event should still attribute")
	}
}

// TestRepoCostPerAgentIsolation: one agent's repo events must never attribute
// another agent's tokens.
func TestRepoCostPerAgentIsolation(t *testing.T) {
	now := time.Now().UTC()
	base := now.Add(-24 * time.Hour)
	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s1", "other", rcUsage(rcTime(base, 15), 700, 0)),
	}}
	entries := []AuditEntry{
		rcEvent(rcTime(base, 10), "scanner", "org/repo-a"),
		rcEvent(rcTime(base, 20), "scanner", "org/repo-b"),
	}
	resp := ComputeRepoCost(summary, entries, now)
	if len(resp.ByRepo) != 0 {
		t.Fatalf("another agent's events attributed these tokens: %+v", resp.ByRepo)
	}
	if resp.Unattributed.Tokens != 700 {
		t.Fatalf("unattributed = %d, want 700", resp.Unattributed.Tokens)
	}
}

// TestRepoCostReportsWindow: hard requirement #6 — the window travels with the
// number, and the oldest observable event bounds it.
func TestRepoCostReportsWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-24 * time.Hour)
	entries := []AuditEntry{
		rcEvent(rcTime(base, 20), "scanner", "org/repo-b"),
		rcEvent(rcTime(base, 10), "scanner", "org/repo-a"),
	}
	resp := ComputeRepoCost(&tokens.AggregateSummary{}, entries, now)
	if resp.WindowHours != int(RepoCostWindow/time.Hour) {
		t.Fatalf("WindowHours = %d", resp.WindowHours)
	}
	if resp.OldestEventAt != rcTime(base, 10).UTC().Format(time.RFC3339) {
		t.Fatalf("OldestEventAt = %q, want the earliest event", resp.OldestEventAt)
	}
	if len(resp.Limitations) == 0 {
		t.Fatal("limitations must ship with the numbers")
	}
}

// TestRepoCostEventsCountOnlyRealClosers asserts that `events` is evidence, not
// decoration: it counts the DISTINCT audited events that actually closed an
// interval carrying usage. An agent's first event closes nothing (the leading
// interval is never attributed) and events closing empty intervals are not
// evidence of spend — counting either would overstate the support behind a
// dollar figure.
func TestRepoCostEventsCountOnlyRealClosers(t *testing.T) {
	now := time.Now().UTC()
	base := now.Add(-24 * time.Hour)

	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		// Only ONE usage event, closing at the event at +20.
		rcSession("s1", "scanner", rcUsage(rcTime(base, 15), 100, 0)),
	}}
	entries := []AuditEntry{
		rcEvent(rcTime(base, 10), "scanner", "org/repo-a"), // first event: closes nothing
		rcEvent(rcTime(base, 20), "scanner", "org/repo-a"), // closes the interval with usage
		rcEvent(rcTime(base, 30), "scanner", "org/repo-a"), // closes an EMPTY interval
	}

	resp := ComputeRepoCost(summary, entries, now)
	a := findRepo(t, resp, "org/repo-a")
	if a.Events != 1 {
		t.Fatalf("Events = %d, want 1 (only the event that closed a non-empty interval)", a.Events)
	}
	if a.Tokens != 100 {
		t.Fatalf("Tokens = %d, want 100", a.Tokens)
	}
}

// TestRepoCostTierPricingIsTagged: hard requirement #4 — a model priced from the
// coarse tier fallback must not be presented with the same confidence as an
// exact list price.
func TestRepoCostTierPricingIsTagged(t *testing.T) {
	now := time.Now().UTC()
	base := now.Add(-24 * time.Hour)
	u := tokens.UsageEvent{TimestampMs: rcTime(base, 15).UnixMilli(), Model: "totally-made-up-model-xyz", Coalesced: 1, Input: 1000}
	s := rcSession("s1", "scanner", u)
	s.Model = "totally-made-up-model-xyz"
	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{s}}
	entries := []AuditEntry{
		rcEvent(rcTime(base, 10), "scanner", "org/repo-a"),
		rcEvent(rcTime(base, 20), "scanner", "org/repo-b"),
	}
	resp := ComputeRepoCost(summary, entries, now)
	if got := findRepo(t, resp, "org/repo-b").Source; got != "estimated_tier" {
		t.Fatalf("Source = %q, want estimated_tier for an unpriced model", got)
	}
}

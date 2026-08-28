package delegation

import (
	"encoding/json"
	"testing"
	"time"
)

func chainAt(t *testing.T, ts string, action, agent string, build func() (Chain, error)) ObservedChain {
	t.Helper()
	c, err := build()
	if err != nil {
		t.Fatalf("building chain for %s: %v", action, err)
	}
	return ObservedChain{Timestamp: ts, Action: action, Agent: agent, Chain: c}
}

// TestCompareClassifiesEachDisagreement exercises all four verdicts. The
// harness's whole value is that it distinguishes them, so a test that only
// covered agreement would leave the interesting three unproven.
func TestCompareClassifiesEachDisagreement(t *testing.T) {
	const ts = "2026-08-28T10:00:00Z"

	chains := []ObservedChain{
		// AGREE — a human acted and the audit log names the same person.
		chainAt(t, ts, "config_saved", "", func() (Chain, error) {
			return ContributePlaneUserChain("acme", "clubanderson")
		}),
		// CHAIN_ONLY — the audit log recorded "system"; the chain has a real
		// machine principal. This is the information-gain case and covers every
		// cadence-triggered action in hive today.
		chainAt(t, ts, "agent_kicked", "scanner", func() (Chain, error) {
			return ScheduledWorkChain("acme", "scanner", "cadence:scanner")
		}),
		// CONFLICT — both name a human and they differ.
		chainAt(t, ts, "pr_merged", "", func() (Chain, error) {
			return ContributePlaneUserChain("acme", "clubanderson")
		}),
		// CONFLICT — the audit log named a person but the chain's root is a
		// machine. Classified as conflict deliberately: the two records
		// disagree about whether a human authorized this.
		chainAt(t, ts, "agent_paused", "reviewer", func() (Chain, error) {
			return ScheduledWorkChain("acme", "reviewer", "cadence:reviewer")
		}),
	}

	audits := []AuditRecord{
		{Timestamp: ts, User: "clubanderson", Action: "config_saved"},
		{Timestamp: ts, User: "system", Action: "agent_kicked", Agent: "scanner"},
		{Timestamp: ts, User: "someone-else", Action: "pr_merged"},
		{Timestamp: ts, User: "clubanderson", Action: "agent_paused", Agent: "reviewer"},
		// AUDIT_ONLY — a human-attributed action with no chain at all.
		{Timestamp: ts, User: "clubanderson", Action: "acmm_level_changed"},
		// A pseudo-user with no chain is NOT reported: it is the everyday case
		// and reporting it would bury the signal.
		{Timestamp: ts, User: "system", Action: "hook_fired"},
	}

	report := Compare(chains, audits)

	want := map[Verdict]int{
		VerdictAgree:     1,
		VerdictChainOnly: 1,
		VerdictConflict:  2,
		VerdictAuditOnly: 1,
	}
	for v, n := range want {
		if got := report.Counts[string(v)]; got != n {
			t.Errorf("verdict %q count = %d, want %d\nfindings: %+v", v, got, n, report.Findings)
		}
	}
	if report.UnmatchedChains != 0 {
		t.Errorf("UnmatchedChains = %d, want 0", report.UnmatchedChains)
	}
	if !report.HasBlockingFindings() {
		t.Error("a report with conflicts must report blocking findings")
	}

	// The CHAIN_ONLY finding must carry the delegation path — that is the
	// information the audit log structurally cannot hold, and the reason the
	// harness exists.
	found := false
	for _, f := range report.Findings {
		if f.Verdict == VerdictChainOnly && f.Action == "agent_kicked" {
			found = true
			if f.ChainPath == "" {
				t.Error("a chain_only finding carries no chain path")
			}
			if f.RootIsHuman {
				t.Error("cadence work reported a human root")
			}
			// The audit user is preserved verbatim, pseudo-users included —
			// "system" appearing here IS the finding.
			if f.AuditUser != "system" {
				t.Errorf("audit user = %q, want %q preserved verbatim", f.AuditUser, "system")
			}
			if f.ChainRoot == "" {
				t.Error("a chain_only finding carries no chain root")
			}
		}
	}
	if !found {
		t.Error("no chain_only finding for the cadence kick")
	}
}

// TestCompareDoesNotManufactureAgreement pins the strict join.
//
// A loose join that matched on action alone would pair unrelated events and
// report agreement between them. A harness that reports FALSE agreement is
// worse than no harness, because it would be cited as evidence that the chain
// is safe to enforce.
func TestCompareDoesNotManufactureAgreement(t *testing.T) {
	chains := []ObservedChain{
		chainAt(t, "2026-08-28T10:00:00Z", "config_saved", "", func() (Chain, error) {
			return ContributePlaneUserChain("acme", "clubanderson")
		}),
	}
	// Same action and user, but a DIFFERENT second and a different agent.
	audits := []AuditRecord{
		{Timestamp: "2026-08-28T11:30:00Z", User: "clubanderson", Action: "config_saved"},
	}

	report := Compare(chains, audits)
	if report.Counts[string(VerdictAgree)] != 0 {
		t.Errorf("the harness manufactured agreement across a %v gap: %+v",
			time.Hour+30*time.Minute, report.Findings)
	}
	if report.UnmatchedChains != 1 {
		t.Errorf("UnmatchedChains = %d, want 1", report.UnmatchedChains)
	}
	// The unjoined human-attributed audit entry is still surfaced.
	if report.Counts[string(VerdictAuditOnly)] != 1 {
		t.Errorf("audit_only count = %d, want 1", report.Counts[string(VerdictAuditOnly)])
	}
}

// TestCompareHandlesDuplicateJoinKeys pins that repeated (action, agent,
// second) tuples are matched pairwise rather than collapsed, so a busy second
// does not undercount.
func TestCompareHandlesDuplicateJoinKeys(t *testing.T) {
	const ts = "2026-08-28T10:00:00Z"
	build := func() (Chain, error) { return ScheduledWorkChain("acme", "scanner", "cadence:scanner") }

	chains := []ObservedChain{
		chainAt(t, ts, "agent_kicked", "scanner", build),
		chainAt(t, ts, "agent_kicked", "scanner", build),
	}
	audits := []AuditRecord{
		{Timestamp: ts, User: "system", Action: "agent_kicked", Agent: "scanner"},
		{Timestamp: ts, User: "system", Action: "agent_kicked", Agent: "scanner"},
	}

	report := Compare(chains, audits)
	if got := report.Counts[string(VerdictChainOnly)]; got != 2 {
		t.Errorf("chain_only count = %d, want 2 (duplicates must match pairwise)", got)
	}
	if report.UnmatchedChains != 0 {
		t.Errorf("UnmatchedChains = %d, want 0", report.UnmatchedChains)
	}
}

// TestAuditRecordParsesRealAuditLogLine pins that AuditRecord's JSON tags match
// the documented on-disk format (src/docs/audit-log.md), so the harness can run
// over a downloaded audit.jsonl with no hive running. A tag drift here would
// silently produce empty users and a report full of false chain_only findings.
func TestAuditRecordParsesRealAuditLogLine(t *testing.T) {
	line := `{"ts":"2026-08-27T14:31:02Z","user":"clubanderson","action":"config_governor_save","detail":"level=4","agent":""}`
	var rec AuditRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("parsing a documented audit line: %v", err)
	}
	if rec.User != "clubanderson" || rec.Action != "config_governor_save" || rec.Detail != "level=4" {
		t.Errorf("audit line parsed incorrectly: %+v", rec)
	}
	if rec.Timestamp != "2026-08-27T14:31:02Z" {
		t.Errorf("timestamp = %q", rec.Timestamp)
	}
	// Omitted optional fields must read as absent, per the doc's warning that a
	// consumer must treat a missing detail/agent as absent rather than empty.
	var minimal AuditRecord
	if err := json.Unmarshal([]byte(`{"ts":"2026-08-27T14:31:02Z","user":"system","action":"hook_fired"}`), &minimal); err != nil {
		t.Fatalf("parsing a minimal audit line: %v", err)
	}
	if minimal.Detail != "" || minimal.Agent != "" {
		t.Errorf("omitted fields did not read as empty: %+v", minimal)
	}
}

// TestPseudoUserSetMatchesAuditLogContract pins this package's local copy of
// the pseudo-user set against the four values documented in
// src/docs/audit-log.md. The copy exists to keep pkg/delegation near-leaf; this
// test is what stops the duplication drifting.
func TestPseudoUserSetMatchesAuditLogContract(t *testing.T) {
	for _, u := range []string{"", "system", "local", "unknown"} {
		if !isAuditPseudoUser(u) {
			t.Errorf("%q must be treated as a pseudo-user (src/docs/audit-log.md)", u)
		}
	}
	// A machine identity coined post-#4055 is NOT a pseudo-user: it is a real
	// non-human actor, and this package models it with a principal type.
	for _, u := range []string{"clubanderson", "hook:pause-on-red", "kubestellar-hive[bot]"} {
		if isAuditPseudoUser(u) {
			t.Errorf("%q must not be treated as a pseudo-user", u)
		}
	}
}

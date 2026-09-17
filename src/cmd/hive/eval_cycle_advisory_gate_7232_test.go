package main

// Tests for gateAdvisoryFindings, runEvalCycle's ioscan canary gate over newly
// ingested advisory findings, extracted behind a seam for #7232. The gate
// decides which findings survive to PersistAsBeads and when a leak is
// recorded; before the extraction none of it was reachable without a live
// dashboard, bead stores, and the process-global canary registry.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/ioscan"
)

// leakRecorder captures the two injected effects so tests can assert on what
// was recorded without a dashboard or a bead store.
type leakRecorder struct {
	audits []string
	beads  []ioscan.CanaryLeak
}

func (r *leakRecorder) deps(scan func(agent, reportText, source string) (ioscan.CanaryLeak, bool), failClosed bool) advisoryIngestDeps {
	return advisoryIngestDeps{
		scanCanary: scan,
		failClosed: failClosed,
		auditLog: func(actor, action, detail, agent string) {
			r.audits = append(r.audits, strings.Join([]string{actor, action, detail, agent}, "|"))
		},
		recordLeakBead: func(leak ioscan.CanaryLeak) { r.beads = append(r.beads, leak) },
	}
}

func findingTitles(findings []advisory.Finding) []string {
	titles := make([]string, 0, len(findings))
	for _, f := range findings {
		titles = append(titles, f.Title)
	}
	return titles
}

// With canaries disabled the scanner is nil: every finding passes through and
// no effect fires, even with fail-closed set — configuration alone never
// blocks a finding.
func TestGateAdvisoryFindings_NilScannerPassesEverything(t *testing.T) {
	rec := &leakRecorder{}
	in := []advisory.Finding{
		{Agent: "scanner", Title: "one"},
		{Agent: "scanner", Title: "two"},
	}
	out := gateAdvisoryFindings(in, rec.deps(nil, true), discardLogger())
	if got, want := fmt.Sprint(findingTitles(out)), fmt.Sprint([]string{"one", "two"}); got != want {
		t.Fatalf("safe findings = %s, want %s", got, want)
	}
	if len(rec.audits) != 0 || len(rec.beads) != 0 {
		t.Fatalf("effects fired with a nil scanner: audits=%v beads=%v", rec.audits, rec.beads)
	}
}

// A leak under fail-closed withholds ONLY the leaking finding; clean findings
// in the same batch still persist, in order.
func TestGateAdvisoryFindings_FailClosedBlocksOnlyTheLeak(t *testing.T) {
	rec := &leakRecorder{}
	scan := func(agent, reportText, source string) (ioscan.CanaryLeak, bool) {
		if strings.Contains(reportText, "LEAKED-TOKEN") {
			return ioscan.CanaryLeak{Agent: agent, Token: "LEAKED-TOKEN", Source: source}, true
		}
		return ioscan.CanaryLeak{}, false
	}
	in := []advisory.Finding{
		{Agent: "scanner", Title: "clean before"},
		{Agent: "scanner", Title: "carries LEAKED-TOKEN"},
		{Agent: "scanner", Title: "clean after"},
	}
	out := gateAdvisoryFindings(in, rec.deps(scan, true), discardLogger())
	if got, want := fmt.Sprint(findingTitles(out)), fmt.Sprint([]string{"clean before", "clean after"}); got != want {
		t.Fatalf("safe findings = %s, want %s", got, want)
	}
	if len(rec.beads) != 1 || rec.beads[0].Source != "advisory-finding" {
		t.Fatalf("leak bead = %+v, want one with source advisory-finding", rec.beads)
	}
}

// Fail-open (fail_closed unset) records the leak — audit entry and bead — but
// still persists the finding: evidence without suppression.
func TestGateAdvisoryFindings_FailOpenRecordsButKeepsTheFinding(t *testing.T) {
	rec := &leakRecorder{}
	scan := func(agent, reportText, source string) (ioscan.CanaryLeak, bool) {
		return ioscan.CanaryLeak{Agent: agent, Token: "tok", Source: source}, true
	}
	in := []advisory.Finding{{Agent: "reviewer", Title: "leaky"}}
	out := gateAdvisoryFindings(in, rec.deps(scan, false), discardLogger())
	if len(out) != 1 || out[0].Title != "leaky" {
		t.Fatalf("safe findings = %+v, want the leaky finding kept", out)
	}
	if len(rec.audits) != 1 || len(rec.beads) != 1 {
		t.Fatalf("leak not recorded under fail-open: audits=%v beads=%v", rec.audits, rec.beads)
	}
}

// The audit entry names the ioscan rule and attributes actor and agent to the
// LEAK's agent (which the registry resolves; it may differ from the finding's).
func TestGateAdvisoryFindings_AuditEntryNamesRuleAndAgent(t *testing.T) {
	rec := &leakRecorder{}
	scan := func(agent, reportText, source string) (ioscan.CanaryLeak, bool) {
		return ioscan.CanaryLeak{Agent: "owner-agent", Token: "tok", Source: source}, true
	}
	gateAdvisoryFindings([]advisory.Finding{{Agent: "scanner", Title: "x"}}, rec.deps(scan, true), discardLogger())
	if len(rec.audits) != 1 {
		t.Fatalf("audits = %v, want exactly one", rec.audits)
	}
	want := "owner-agent|ioscan_canary_leak|rule=canary.leak, agent=owner-agent, source=advisory-finding|owner-agent"
	if rec.audits[0] != want {
		t.Fatalf("audit entry = %q, want %q", rec.audits[0], want)
	}
}

// The scan text is title, detail, file, type and severity joined by newlines —
// a canary smuggled into ANY of those fields must reach the scanner. Pins the
// exact composition so a field silently dropped from the join fails here.
func TestGateAdvisoryFindings_ScanTextCoversAllReportFields(t *testing.T) {
	var scanned []string
	scan := func(agent, reportText, source string) (ioscan.CanaryLeak, bool) {
		scanned = append(scanned, agent+"\x00"+reportText+"\x00"+source)
		return ioscan.CanaryLeak{}, false
	}
	rec := &leakRecorder{}
	f := advisory.Finding{
		Agent: "scanner", Title: "T", Detail: "D", File: "F", Type: "Y", Severity: "S",
	}
	out := gateAdvisoryFindings([]advisory.Finding{f}, rec.deps(scan, true), discardLogger())
	if len(out) != 1 {
		t.Fatalf("clean finding was withheld: %+v", out)
	}
	if len(scanned) != 1 {
		t.Fatalf("scanner called %d times, want 1", len(scanned))
	}
	if want := "scanner\x00T\nD\nF\nY\nS\x00advisory-finding"; scanned[0] != want {
		t.Fatalf("scan input = %q, want %q", scanned[0], want)
	}
}

// An empty batch returns an empty (non-nil) slice and fires nothing, matching
// the original loop's semantics for PersistAsBeads.
func TestGateAdvisoryFindings_EmptyBatch(t *testing.T) {
	rec := &leakRecorder{}
	out := gateAdvisoryFindings(nil, rec.deps(nil, true), discardLogger())
	if out == nil || len(out) != 0 {
		t.Fatalf("out = %#v, want empty non-nil slice", out)
	}
}

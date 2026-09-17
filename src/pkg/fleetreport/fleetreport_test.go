package fleetreport

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
)

func TestClassifyErrorsFlagsPeriodicBrokenPipeAndIgnoresBenignTimeouts(t *testing.T) {
	now := time.Date(2026, 9, 16, 21, 0, 0, 0, time.UTC)
	var events []ErrorEvent
	for i := 0; i < 366; i++ {
		events = append(events, ErrorEvent{At: now.Add(-9*time.Minute + time.Duration(i)*time.Second), Component: "proxy-read-keepalive", Class: "i/o timeout"})
	}
	start := now.Add(-7 * time.Minute)
	for i := 0; i < 6; i++ {
		events = append(events, ErrorEvent{At: start.Add(time.Duration(i) * 73 * time.Second), Component: "proxy-write", Agent: "scanner", Lane: "pr", Class: "write: broken pipe"})
	}
	got := ClassifyErrors(events, now, 10*time.Minute)
	if len(got) != 1 {
		t.Fatalf("evidence count = %d, want 1: %#v", len(got), got)
	}
	if got[0].ErrorClass != "write: broken pipe" || got[0].Count != 6 || got[0].Periodicity != "1m13s" || !got[0].Attributable {
		t.Fatalf("broken-pipe evidence = %#v", got[0])
	}
}

func TestEvaluateRequiresPersistentUnmetEpochsJustLevelledUpDoesNotFile(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	obs := Observation{
		EpochStart: now,
		HiveID:     "hive-a",
		Version:    "v4.40.0",
		Commit:     "abcdef1234567890",
		Mode:       "BUSY",
		ACMMLevel:  4,
		Unmet:      []acmmadvisor.Criterion{{Name: "Green-CI streak"}},
		Evidence:   []Evidence{{Component: "target-repo", Agent: "quality", ErrorClass: "required check failed", Count: 4, Window: time.Hour, Severity: "high", Attributable: true}},
	}
	got := Evaluate(obs, State{}, true)
	if len(got.Reports) != 0 {
		t.Fatalf("single unmet epoch filed reports: %#v", got.Reports)
	}
	if len(got.State.Criteria["green-ci-streak"].Epochs) != 1 {
		t.Fatalf("state did not record first epoch: %#v", got.State)
	}
}

func TestEvaluateFilesAfterPersistentHiveAttributableShortfall(t *testing.T) {
	first := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	crit := acmmadvisor.Criterion{Name: "Merge success rate"}
	state := Evaluate(Observation{EpochStart: first, HiveID: "hive-a", Version: "v4.40.0", Unmet: []acmmadvisor.Criterion{crit}}, State{}, true).State
	second := first.Add(7 * 24 * time.Hour)
	got := Evaluate(Observation{
		EpochStart: second,
		HiveID:     "hive-a",
		Version:    "v4.40.0",
		Commit:     "abcdef1234567890",
		Mode:       "SURGE",
		ACMMLevel:  5,
		Unmet:      []acmmadvisor.Criterion{crit},
		Evidence:   []Evidence{{Component: "merge", Lane: "pr", ErrorClass: "structurally unmergeable hive PR", Count: 3, Window: time.Hour, Severity: "high", Attributable: true}},
	}, state, true)
	if len(got.Reports) != 1 {
		t.Fatalf("reports = %#v", got.Reports)
	}
	r := got.Reports[0]
	if r.Criterion != "merge-success-rate" || !strings.Contains(r.Body, "unmet criterion") || !strings.Contains(r.Body, "Merge success rate") {
		t.Fatalf("report missing criterion evidence: %#v", r)
	}
	if !contains(r.Labels, "criterion:merge-success-rate") || !contains(r.Labels, "fleet-report") {
		t.Fatalf("labels missing criterion/fleet labels: %v", r.Labels)
	}
}

func TestEvaluateOperatorActionableWithoutEvidenceStaysLocal(t *testing.T) {
	crit := acmmadvisor.Criterion{Name: "Test coverage"}
	got := Evaluate(Observation{EpochStart: time.Now(), Unmet: []acmmadvisor.Criterion{crit}}, State{}, true)
	if len(got.Reports) != 0 {
		t.Fatalf("operator-actionable shortfall filed upstream: %#v", got.Reports)
	}
	if !reflect.DeepEqual(got.OperatorCriteria, []string{"Test coverage"}) {
		t.Fatalf("operator criteria = %v", got.OperatorCriteria)
	}
}

func TestFingerprintStableAndSeparatesClasses(t *testing.T) {
	a := Fingerprint("i/o timeout", "proxy", "v4.40.0", "green-ci-streak")
	b := Fingerprint("i/o timeout", "proxy", "v4.40.0", "green-ci-streak")
	c := Fingerprint("write: broken pipe", "proxy", "v4.40.0", "green-ci-streak")
	if a != b || a == c || len(a) != 24 {
		t.Fatalf("fingerprints unstable or over-merged: a=%s b=%s c=%s", a, b, c)
	}
}

func TestAnonymousInstanceAndScrubbing(t *testing.T) {
	id := AnonymousInstanceID("https://secret-hive.example")
	if strings.Contains(id, "secret") || len(id) != 16 {
		t.Fatalf("anonymous id leaked input: %q", id)
	}
	r := BuildReport(Observation{HiveID: "h", Version: "ghp_abcdefghijklmnopqrstuvwxyz123456", Commit: "abcdef", Mode: "IDLE", ACMMLevel: 3}, "coverage", "Token ghp_abcdefghijklmnopqrstuvwxyz123456", Evidence{Component: "proxy", ErrorClass: "ghp_abcdefghijklmnopqrstuvwxyz123456", Count: 1, Window: time.Minute, Severity: "high", Attributable: true}, "fp", id, "ghp_abcdefghijklmnopqrstuvwxyz123456", "abcdef")
	if strings.Contains(r.Body, "ghp_abcdefghijklmnopqrstuvwxyz123456") || strings.Contains(r.Title, "ghp_abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("report was not scrubbed:\n%s\n%s", r.Title, r.Body)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestClassifyErrorsDropsNonPeriodicSingletonAndOutOfWindow(t *testing.T) {
	now := time.Date(2026, 9, 16, 21, 0, 0, 0, time.UTC)
	events := []ErrorEvent{
		{At: now.Add(-time.Hour), Component: "proxy", Class: "write: broken pipe"},
		{At: now.Add(-time.Minute), Component: "proxy", Class: "transient EOF"},
		{At: now.Add(time.Minute), Component: "proxy", Class: "future"},
		{At: now.Add(-time.Minute), Component: "proxy", Class: ""},
	}
	if got := ClassifyErrors(events, now, 10*time.Minute); len(got) != 0 {
		t.Fatalf("unexpected evidence: %#v", got)
	}
}

func TestClassifyErrorsAcceptsBurstOfRepeatedErrors(t *testing.T) {
	now := time.Date(2026, 9, 16, 21, 0, 0, 0, time.UTC)
	events := []ErrorEvent{
		{At: now.Add(-4 * time.Minute), Component: "backend-auth", Agent: "quality", Class: "401 unauthorized"},
		{At: now.Add(-3 * time.Minute), Component: "backend-auth", Agent: "quality", Class: "401 unauthorized"},
		{At: now.Add(-30 * time.Second), Component: "backend-auth", Agent: "quality", Class: "401 unauthorized"},
	}
	got := ClassifyErrors(events, now, 0)
	if len(got) != 1 || got[0].Severity != "medium" || got[0].Periodicity != "" {
		t.Fatalf("burst evidence = %#v", got)
	}
}

func TestPeriodicityRejectsBadIntervals(t *testing.T) {
	now := time.Date(2026, 9, 16, 21, 0, 0, 0, time.UTC)
	if got := periodicity([]ErrorEvent{{At: now}, {At: now.Add(10 * time.Second)}, {At: now.Add(30 * time.Second)}}); got != "" {
		t.Fatalf("non-periodic intervals got %q", got)
	}
	if got := periodicity([]ErrorEvent{{At: now}, {At: now}}); got != "" {
		t.Fatalf("zero interval got %q", got)
	}
}

func TestEvaluateClearsRecoveredCriterionAndMarksOpenIssue(t *testing.T) {
	state := State{
		Criteria: map[string]CriterionState{"green-ci-streak": {Epochs: []time.Time{time.Now()}}},
		Open:     map[string]OpenIssue{"abc:green-ci-streak:def": {Number: 12, OpenedByHive: true, Criterion: "green-ci-streak"}},
	}
	got := Evaluate(Observation{EpochStart: time.Now(), Unmet: nil}, state, true)
	if _, ok := got.State.Criteria["green-ci-streak"]; ok {
		t.Fatalf("criterion was not cleared: %#v", got.State.Criteria)
	}
	if got.State.Open["abc:green-ci-streak:def"].Recovered {
		t.Fatalf("recovery should not be persisted before upstream write succeeds: %#v", got.State.Open)
	}
	if len(got.Recoveries) != 1 || !got.Recoveries[0].Recovered {
		t.Fatalf("recovery not emitted: %#v", got.Recoveries)
	}
}

func TestRecoveryReportAndShortCommitDefaults(t *testing.T) {
	r := Report{Fingerprint: "fp", InstanceID: "inst", Criterion: "green-ci-streak"}
	got := RecoveryReport(OpenIssue{Number: 1, OpenedByHive: true}, r)
	if !got.Recovered || !strings.Contains(got.Body, "self-recovered: yes") {
		t.Fatalf("recovery report = %#v", got)
	}
	if shortCommit("") != "unknown" || shortCommit("123456789012345") != "123456789012" {
		t.Fatalf("shortCommit defaults/regression")
	}
	if absDuration(-time.Second) != time.Second {
		t.Fatalf("absDuration negative failed")
	}
}

func TestEvaluateSuppressesUnchangedEnabledReport(t *testing.T) {
	now := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	crit := acmmadvisor.Criterion{Name: "green CI streak"}
	ev := Evidence{Component: "agent-runtime", ErrorClass: "agent stall", Count: 4, Window: time.Hour, Severity: "medium", Attributable: true}
	obs := Observation{EpochStart: now, HiveID: "hive-a", Version: "4.0.0", Commit: "abcdef1", Mode: "busy", ACMMLevel: 3, Unmet: []acmmadvisor.Criterion{crit}, Evidence: []Evidence{ev}}
	first := Evaluate(obs, State{Criteria: map[string]CriterionState{"green-ci-streak": {Epochs: []time.Time{now.AddDate(0, 0, -7)}}}}, false)
	if len(first.Reports) != 1 {
		t.Fatalf("first reports=%d, want 1", len(first.Reports))
	}
	fp := first.Reports[0].Fingerprint
	state := first.State
	open := state.Open[fp]
	open.Number = 11
	open.BodyHash = StableBodyHash(first.Reports[0].Body)
	state.Open[fp] = open
	second := Evaluate(obs, state, false)
	if len(second.Reports) != 0 {
		t.Fatalf("unchanged enabled report should be skipped: %#v", second.Reports)
	}
	dryRun := Evaluate(obs, state, true)
	if len(dryRun.Reports) != 1 {
		t.Fatalf("dry-run should still preview would-file report, got %d", len(dryRun.Reports))
	}
}

func TestReportTitleIncludesFingerprintForCreateDedupe(t *testing.T) {
	obs := Observation{HiveID: "hive-a", Version: "4.0.0", Commit: "abcdef1", Mode: "busy", ACMMLevel: 3}
	ev := Evidence{Component: "agent-runtime", ErrorClass: "agent stall", Count: 3, Window: time.Hour, Severity: "medium", Attributable: true}
	fp := Fingerprint(ev.ErrorClass, ev.Component, "4.0.0", "green-ci-streak")
	report := BuildReport(obs, "green-ci-streak", "green CI streak", ev, fp, AnonymousInstanceID(obs.HiveID), "4.0.0", "abcdef1")
	if !strings.Contains(report.Title, fp) {
		t.Fatalf("title %q must include fingerprint %q so CreateIssue title dedupe cannot merge distinct reports", report.Title, fp)
	}
}

func TestEvaluateRetriesPendingRecoveryAfterCriterionPruned(t *testing.T) {
	got := Evaluate(Observation{HiveID: "hive-a"}, State{Open: map[string]OpenIssue{
		"fp": {Number: 12, OpenedByHive: true, Criterion: "green-ci-streak"},
	}}, false)
	if len(got.Recoveries) != 1 || got.Recoveries[0].Fingerprint != "fp" {
		t.Fatalf("pending recovery not retried: %#v", got.Recoveries)
	}
}

func TestEvaluateTriggerBHiveDefectWithoutACMMShortfall(t *testing.T) {
	obs := Observation{
		EpochStart: time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC),
		HiveID:     "hive-a",
		Version:    "v4.40.2",
		Commit:     "abcdef1234567890",
		Mode:       "QUIET",
		ACMMLevel:  3,
		Evidence:   []Evidence{{Component: "proxy-write", Agent: "scanner", Lane: "pr", ErrorClass: "write: broken pipe", Count: 6, Window: 10 * time.Minute, Periodicity: "1m13s", Severity: "high", Attributable: true}},
	}
	got := Evaluate(obs, State{}, true)
	if len(got.Reports) != 1 {
		t.Fatalf("Trigger B reports=%#v, want one", got.Reports)
	}
	r := got.Reports[0]
	if r.Trigger != TriggerHiveDefect || r.Criterion != "" {
		t.Fatalf("trigger/criterion = %q/%q, want Trigger B without criterion", r.Trigger, r.Criterion)
	}
	if !contains(r.Labels, "trigger:hive-code-defect") || containsPrefix(r.Labels, "criterion:") {
		t.Fatalf("Trigger B labels wrong: %v", r.Labels)
	}
	if strings.Contains(r.Body, "unmet criterion") || !strings.Contains(r.Body, "No ACMM shortfall is required") {
		t.Fatalf("Trigger B body should omit unmet criterion and explain defect trigger:\n%s", r.Body)
	}
}

func TestDefectFingerprintStableAndVersionScoped(t *testing.T) {
	a := DefectFingerprint("write: broken pipe", "proxy-write", "v4.40.2")
	b := DefectFingerprint("write: broken pipe", "proxy-write", "v4.40.2")
	c := DefectFingerprint("write: broken pipe", "proxy-write", "v4.40.3")
	d := Fingerprint("write: broken pipe", "proxy-write", "v4.40.2", "green-ci-streak")
	if a != b || a == c || a == d || len(a) != 24 {
		t.Fatalf("bad defect fingerprint stability/scope: a=%s b=%s c=%s d=%s", a, b, c, d)
	}
}

func TestTriggerBRecoveryWhenEvidenceClears(t *testing.T) {
	state := State{Open: map[string]OpenIssue{"fp": {Number: 9, Trigger: TriggerHiveDefect}}}
	got := Evaluate(Observation{HiveID: "hive-a"}, state, false)
	if len(got.Recoveries) != 1 || got.Recoveries[0].Criterion != "" || got.Recoveries[0].Trigger != TriggerHiveDefect {
		t.Fatalf("Trigger B recovery = %#v", got.Recoveries)
	}
	if strings.Contains(got.Recoveries[0].Body, "unmet") {
		t.Fatalf("Trigger B recovery should not mention unmet criterion: %s", got.Recoveries[0].Body)
	}
}

func containsPrefix(xs []string, prefix string) bool {
	for _, x := range xs {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}
	return false
}

func TestTriggerARecoveryWaitsForCriterionToClear(t *testing.T) {
	crit := acmmadvisor.Criterion{Name: "Green CI streak"}
	state := State{Open: map[string]OpenIssue{"fp": {Number: 4, Trigger: TriggerACMMShortfall, Criterion: "green-ci-streak"}}}
	got := Evaluate(Observation{HiveID: "hive-a", Unmet: []acmmadvisor.Criterion{crit}}, state, false)
	if len(got.Recoveries) != 0 {
		t.Fatalf("Trigger A recovered while criterion still unmet: %#v", got.Recoveries)
	}
}

func TestTriggerAFingerprintPreservesLegacyScheme(t *testing.T) {
	got := Fingerprint("write: broken pipe", "proxy", "v4.40.0", "green-ci-streak")
	h := sha256.Sum256([]byte("write: broken pipe|proxy|v4.40.0|green-ci-streak"))
	want := hex.EncodeToString(h[:])[:24]
	if got != want {
		t.Fatalf("Fingerprint changed from legacy scheme: got %s want %s", got, want)
	}
}

func TestRecoveredTriggerBRecurrenceReportsAgain(t *testing.T) {
	ev := Evidence{Component: "proxy-write", ErrorClass: "write: broken pipe", Count: 6, Window: time.Minute, Periodicity: "10s", Severity: "high", Attributable: true}
	fp := DefectFingerprint(ev.ErrorClass, ev.Component, "v4.40.2")
	report := BuildDefectReport(Observation{HiveID: "hive-a", Version: "v4.40.2"}, ev, fp, AnonymousInstanceID("hive-a"), "v4.40.2", "abcdef")
	state := State{Open: map[string]OpenIssue{fp: {Number: 7, Trigger: TriggerHiveDefect, Recovered: true, BodyHash: StableBodyHash(report.Body)}}}
	got := Evaluate(Observation{HiveID: "hive-a", Version: "v4.40.2", Evidence: []Evidence{ev}}, state, false)
	if len(got.Reports) != 1 {
		t.Fatalf("recovered recurrence did not report again: %#v", got.Reports)
	}
	if got.State.Open[fp].Recovered {
		t.Fatalf("active recurrence should clear recovered flag: %#v", got.State.Open[fp])
	}
}

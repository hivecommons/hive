package fleetreport

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
	"github.com/hivecommons/hive/pkg/hivestate"
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

func TestBuildObservationReportContainsHiveMetadataAndSeriesOnly(t *testing.T) {
	r := BuildObservationReport(ObservationFinding{
		Kind:           "queue-growth",
		Title:          "Queue depth is growing",
		Evidence:       "p90 depth 30 versus 15",
		SuspectedCause: "intake is outpacing throughput",
		NextStep:       "review the oldest queue items",
		Fingerprint:    "h1:queue-growth",
		Series:         []float64{15, 18, 30},
	}, ObservationMetadata{
		HiveID:     "h1",
		HiveSlug:   "hosted-h1",
		ProjectOrg: "acme",
		ACMMLevel:  3,
		Version:    "v5.1.0",
		ClusterID:  "oke-11",
		Now:        time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
	})
	for _, want := range []string{"h1", "hosted-h1", "acme", "L3", "v5.1.0", "oke-11", "[15,18,30]"} {
		if !strings.Contains(r.Body, want) {
			t.Fatalf("observation body missing %q:\n%s", want, r.Body)
		}
	}
	if strings.Contains(r.Body, "acme/widgets") || strings.Contains(r.Body, "foo.go") {
		t.Fatalf("observation body leaked project repository identity:\n%s", r.Body)
	}
	if r.Trigger != TriggerObservation || r.Class != TriggerObservation || r.Observation == nil || r.Observation.Kind != "queue-growth" {
		t.Fatalf("observation report = %#v", r)
	}
	if !contains(r.Labels, LabelObservation) {
		t.Fatalf("observation labels = %v", r.Labels)
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

func TestCountWindowOmitsUnclaimedWindow(t *testing.T) {
	if got := countWindow(43, 0); got != "43 (cumulative)" {
		t.Fatalf("countWindow with no window = %q", got)
	}
	if got := countWindow(6, 10*time.Minute); got != "6 in 10m0s" {
		t.Fatalf("countWindow with window = %q", got)
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

func TestRecoveryBodyNamesACMMCriterion(t *testing.T) {
	got := RecoveryReport(OpenIssue{Trigger: TriggerACMMShortfall}, Report{Fingerprint: "abc123", InstanceID: "inst", Criterion: "green-ci-streak", Trigger: TriggerACMMShortfall})
	if !strings.Contains(got.Body, "`green-ci-streak`") {
		t.Fatalf("recovery body dropped the criterion: %q", got.Body)
	}
}

func TestReportsCarryHiveStateOutsideBody(t *testing.T) {
	trends := &hivestate.Trends{Last24h: hivestate.Window{Samples: 3, QueueIssues: hivestate.Quantiles{Median: 4, P90: 8}}}
	obs := Observation{Mode: "busy", ACMMLevel: 2, HiveState: trends}
	ev := Evidence{Component: "merge", ErrorClass: "conflict", Severity: "high", Class: "unclassified_error_repeated", Message: "boom"}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	reports := []Report{
		BuildReport(obs, "ci", "CI", ev, "fp", "inst", "v", "c"),
		BuildRuntimeReport(obs, ev, "fp", "inst", "hive", "v", "c", now),
	}
	obs.HiveState = nil
	bare := []Report{
		BuildReport(obs, "ci", "CI", ev, "fp", "inst", "v", "c"),
		BuildRuntimeReport(obs, ev, "fp", "inst", "hive", "v", "c", now),
	}
	for i, r := range reports {
		if r.HiveState != trends {
			t.Errorf("report %d hive_state = %+v, want the observation's trends", i, r.HiveState)
		}
		if bare[i].HiveState != nil {
			t.Errorf("report %d without trends = %+v, want nil", i, bare[i].HiveState)
		}
		if r.Body != bare[i].Body {
			t.Errorf("report %d body changed with hive state; trends must not alter the body hash", i)
		}
	}
}

// Selfreports keep the deprecated hive/selfreport marker for operators'
// existing filters and ALSO carry origin/selfreport, the universal
// provenance axis every hive-created issue records.
func TestRuntimeReportCarriesBothSelfReportLabels(t *testing.T) {
	ev := Evidence{Component: "merge", ErrorClass: "conflict", Severity: "high", Class: "unclassified_error_repeated", Message: "boom"}
	r := BuildRuntimeReport(Observation{Mode: "busy"}, ev, "fp", "inst", "hive", "v", "c", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	has := map[string]bool{}
	for _, l := range r.Labels {
		has[l] = true
	}
	if !has[LabelSelfReport] || !has[LabelOriginSelfReport] || LabelOriginSelfReport != "origin/selfreport" {
		t.Fatalf("labels = %v, want both %s and origin/selfreport", r.Labels, LabelSelfReport)
	}
}

// The standalone hive-code-defect trigger is retired (#13470): evidence
// without a runtime class and without a persistent ACMM shortfall files
// nothing, and a persisted entry for the retired trigger is forgotten
// rather than recovered under a trigger that no longer exists.
func TestEvaluateNoLongerFilesStandaloneHiveCodeDefects(t *testing.T) {
	obs := Observation{HiveID: "hive-a", Version: "v4.40.2", Evidence: []Evidence{
		{Component: "agent-runtime", Agent: "scanner", Lane: "scanner", ErrorClass: "agent stall", Count: 3, Severity: "medium", Attributable: true},
		{Component: "proxy-write", Agent: "scanner", Lane: "pr", ErrorClass: "write: broken pipe", Count: 6, Window: 10 * time.Minute, Periodicity: "1m13s", Severity: "high", Attributable: true},
	}}
	state := State{Open: map[string]OpenIssue{
		"legacy-defect":   {Number: 9, Trigger: "hive-code-defect"},
		"legacy-untagged": {Number: 10},
	}}
	got := Evaluate(obs, state, true)
	if len(got.Reports) != 0 || len(got.Recoveries) != 0 {
		t.Fatalf("retired trigger still produced reports=%#v recoveries=%#v", got.Reports, got.Recoveries)
	}
	if _, ok := got.State.Open["legacy-defect"]; ok {
		t.Fatal("legacy hive-code-defect entry was not pruned from state")
	}
	if _, ok := got.State.Open["legacy-untagged"]; ok {
		t.Fatal("legacy untagged (defect) entry was not pruned from state")
	}
	for _, r := range got.Reports {
		if strings.Contains(r.Title, "hive-code defect") {
			t.Fatalf("legacy title rendered: %q", r.Title)
		}
	}
}

// An ACMM-shortfall report carries the same identity rows, label set and
// Affected-hives block as a self-report (#13470), so the join path and the
// hub can enrich it like any other hive-filed issue.
func TestACMMReportCarriesIdentityOwnersReposAndAffectedBlock(t *testing.T) {
	obs := Observation{HiveID: "hive-a", HiveSlug: "hosted-aslom", HiveName: "Aslom", SpokeURL: "https://aslom.hive.example", Version: "v5.1.0", Commit: "abcdef1234", Mode: "surge", ACMMLevel: 2, Cluster: "oke-11",
		Repos: []string{"acme/widgets", "acme/gadgets"}, Owners: []string{"alice", "bob"}}
	ev := Evidence{Component: "merge", Lane: "pr", ErrorClass: "structurally unmergeable hive PR", Count: 3, Window: time.Hour, Severity: "high", Attributable: true}
	r := BuildReport(obs, "merge-success-rate", "Merge success rate", ev, "fp1", AnonymousInstanceID("hive-a"), "v5.1.0", "abcdef1")
	for _, want := range []string{"| hive | hosted-aslom |", "| " + HiveNameField + " | Aslom |", "aslom.hive.example", "@alice", "@bob", "acme/widgets", "acme/gadgets", AffectedBlockStart, "owner(s): @alice, @bob", "| unmet criterion | Merge success rate (merge-success-rate) |", SelfReportMarker("fp1", "hosted-aslom")} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("body missing %q:\n%s", want, r.Body)
		}
	}
	if strings.Contains(r.Body, "last 20") || strings.Contains(r.Body, ", last ") {
		t.Fatalf("ACMM body must be timestamp-free so it hashes stably:\n%s", r.Body)
	}
	for _, l := range []string{LabelFleetReport, LabelSelfReport, LabelOriginSelfReport, LabelKindBug, LabelNeedsTriage, "severity:high", "trigger:acmm-shortfall", "criterion:merge-success-rate"} {
		if !contains(r.Labels, l) {
			t.Errorf("labels %v missing %q", r.Labels, l)
		}
	}
	if containsPrefix(r.Labels, "class:") || containsPrefix(r.Labels, "instance:") || containsPrefix(r.Labels, "version:") || containsPrefix(r.Labels, "component:") {
		t.Fatalf("legacy per-instance labels (which the report repo rejects) are back: %v", r.Labels)
	}
	if r.Hive != "hosted-aslom" || r.HiveName != "Aslom" || r.SpokeURL != "https://aslom.hive.example" || !reflect.DeepEqual(r.Owners, []string{"alice", "bob"}) || len(r.Repos) != 2 || !strings.HasPrefix(r.Title, "Fleet report: merge blocks Merge success rate [") {
		t.Fatalf("report identity = %+v", r)
	}
	hidden := BuildReport(Observation{HiveID: "hive-a", Owners: []string{"alice"}, OwnersHidden: true}, "k", "K", ev, "fp2", "inst", "v", "c")
	if strings.Contains(hidden.Body, "@alice") || hidden.Owners != nil || hidden.Hive != "inst" {
		t.Fatalf("hidden owners leaked or anonymous hive not used: %+v", hidden)
	}
}

// With an open self-report about the same component (and this version), a
// persistent ACMM shortfall rolls up onto it instead of filing its own
// "Fleet report:" issue; once that self-report is gone it files standalone.
func TestEvaluateRollsUpACMMShortfallOntoOpenSelfReport(t *testing.T) {
	first := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	crit := acmmadvisor.Criterion{Name: "Green CI streak"}
	ev := Evidence{Component: "agent-runtime", Agent: "scanner", Lane: "scanner", ErrorClass: "agent stall", Count: 3, Severity: "medium", Attributable: true}
	mkObs := func(epoch time.Time) Observation {
		return Observation{EpochStart: epoch, HiveID: "hive-a", HiveSlug: "hosted-aslom", Version: "v5.1.0", Unmet: []acmmadvisor.Criterion{crit}, Evidence: []Evidence{ev}}
	}
	state := Evaluate(mkObs(first), State{}, true).State
	state.Open["fp-self"] = OpenIssue{Number: 21, URL: "u", Trigger: TriggerRuntimeError, Class: ClassAgentStall, Component: "agent-runtime", PostedAt: first}
	state.Open["fp-other"] = OpenIssue{Number: 22, Trigger: TriggerRuntimeError, Class: ClassPanicRecovered, Component: "dashboard", PostedAt: first}
	state.Open["fp-closed"] = OpenIssue{Number: 23, Trigger: TriggerRuntimeError, Component: "agent-runtime", ClosedAt: first}
	got := Evaluate(mkObs(first.Add(7*24*time.Hour)), state, true)
	if len(got.Reports) != 1 {
		t.Fatalf("reports = %#v", got.Reports)
	}
	r := got.Reports[0]
	fp := Fingerprint(ev.ErrorClass, ev.Component, "v5.1.0", "green-ci-streak")
	if r.RollupOf != "fp-self" || r.Fingerprint != fp || r.Trigger != TriggerACMMShortfall || r.Criterion != "green-ci-streak" {
		t.Fatalf("roll-up = %+v", r)
	}
	for _, want := range []string{SeenMarker(fp, "hosted-aslom"), "rolled up from **hosted-aslom**", "`agent-runtime`", "`green-ci-streak`", "3 (cumulative)", "no standalone `Fleet report:` issue"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("roll-up body missing %q:\n%s", want, r.Body)
		}
	}
	if strings.Contains(r.Body, "## Evidence") {
		t.Fatalf("roll-up should be a short comment, not the full report:\n%s", r.Body)
	}
	if got.State.Open[fp].Component != "agent-runtime" {
		t.Fatalf("component not recorded on the shortfall entry: %+v", got.State.Open[fp])
	}

	// Posted as a roll-up (state points at the self-report): it keeps
	// rolling up while the target is open rather than filing standalone.
	state = got.State
	state.Open[fp] = OpenIssue{Number: 21, Trigger: TriggerACMMShortfall, Criterion: "green-ci-streak", Component: "agent-runtime", RollupOf: "fp-self", BodyHash: StableBodyHash(r.Body), PostedAt: first}
	again := Evaluate(mkObs(first.Add(14*24*time.Hour)), state, false)
	if len(again.Reports) != 0 {
		t.Fatalf("unchanged roll-up re-emitted: %#v", again.Reports)
	}

	// Target closed: the shortfall starts over as a standalone report.
	state.Open["fp-self"] = OpenIssue{Number: 21, Trigger: TriggerRuntimeError, Component: "agent-runtime", ClosedAt: first}
	fresh := Evaluate(mkObs(first.Add(21*24*time.Hour)), state, false)
	if len(fresh.Reports) != 1 || fresh.Reports[0].RollupOf != "" || !strings.Contains(fresh.Reports[0].Body, "## Evidence") {
		t.Fatalf("standalone fallback = %#v", fresh.Reports)
	}
	if o := fresh.State.Open[fp]; o.Number != 0 || o.RollupOf != "" || o.Criterion != "green-ci-streak" {
		t.Fatalf("roll-up entry not reset for standalone filing: %+v", o)
	}

	// No self-report at all: standalone from the start.
	plain := Evaluate(mkObs(first.Add(7*24*time.Hour)), Evaluate(mkObs(first), State{}, true).State, true)
	if len(plain.Reports) != 1 || plain.Reports[0].RollupOf != "" {
		t.Fatalf("standalone without a self-report = %#v", plain.Reports)
	}
}

func TestOpenSelfReportForPicksLowestOpenMatch(t *testing.T) {
	st := State{Open: map[string]OpenIssue{
		"b": {Number: 2, Trigger: TriggerRuntimeError, Component: "Agent-Runtime"},
		"a": {Number: 1, Trigger: TriggerRuntimeError, Component: "agent-runtime"},
		"c": {Number: 3, Trigger: TriggerRuntimeError, Component: "agent-runtime", Recovered: true},
		"d": {Trigger: TriggerRuntimeError, Component: "agent-runtime"},
		"e": {Number: 5, Trigger: TriggerACMMShortfall, Component: "agent-runtime"},
	}}
	if got, ok := OpenSelfReportFor(st, " agent-runtime "); !ok || got != "a" {
		t.Fatalf("got %q/%v, want a", got, ok)
	}
	if _, ok := OpenSelfReportFor(st, "merge"); ok {
		t.Fatal("matched a component with no self-report")
	}
	if _, ok := OpenSelfReportFor(st, ""); ok {
		t.Fatal("matched an empty component")
	}
}

func TestSelfReportLabelsAndCoreSet(t *testing.T) {
	got := SelfReportLabels(TriggerRuntimeError, "High", ClassAgentStall)
	if !sort.StringsAreSorted(got) || !contains(got, "class:agent-stall") || !contains(got, "severity:high") || !contains(got, "trigger:runtime-error") {
		t.Fatalf("labels = %v", got)
	}
	if containsPrefix(SelfReportLabels(TriggerACMMShortfall, "low", ""), "class:") {
		t.Fatal("empty class rendered a class label")
	}
	core := CoreSelfReportLabels()
	for _, l := range core {
		if !contains(got, l) {
			t.Fatalf("core label %q not in the full set %v", l, got)
		}
	}
	if len(core) != 5 {
		t.Fatalf("core set = %v", core)
	}
}

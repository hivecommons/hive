package fleetreport

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/logscrub"
)

func fakeClock(start time.Time) (func() time.Time, func(time.Duration)) {
	now := start
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }
}

func TestRuntimeTrackerCountThreshold(t *testing.T) {
	now, _ := fakeClock(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	tr := NewRuntimeTracker(now)
	tr.Observe(ClassKickFailedRepeatedly, "agent-a", "kick failed: tmux send-keys exit 1", "diag")
	tr.Observe(ClassKickFailedRepeatedly, "agent-a", "kick failed: tmux send-keys exit 1", "diag")
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("2 kicks produced evidence: %#v", ev)
	}
	tr.Observe(ClassKickFailedRepeatedly, "agent-a", "kick failed: tmux send-keys exit 1", "diag")
	ev := tr.Evidence()
	if len(ev) != 1 || ev[0].Class != ClassKickFailedRepeatedly || ev[0].Count != 3 || !ev[0].Attributable {
		t.Fatalf("evidence = %#v", ev)
	}
	if ev[0].Severity != "medium" || ev[0].Component != "agent-runtime" {
		t.Fatalf("spec not applied: %#v", ev[0])
	}
	tr.Clear(ClassKickFailedRepeatedly, "agent-a")
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("cleared condition still reported: %#v", ev)
	}
	cleared := tr.TakeCleared()
	if len(cleared) != 1 || cleared[0].Key != "agent-a" {
		t.Fatalf("TakeCleared = %#v", cleared)
	}
	if again := tr.TakeCleared(); len(again) != 0 {
		t.Fatalf("TakeCleared not drained: %#v", again)
	}
}

func TestRuntimeTrackerDurationThresholdAndTTL(t *testing.T) {
	now, advance := fakeClock(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	tr := NewRuntimeTracker(now)
	tr.Observe(ClassModelDiscoveryDegraded, "copilot", "model discovery failed: 502", "")
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("degraded <1h produced evidence: %#v", ev)
	}
	advance(59 * time.Minute)
	tr.Observe(ClassModelDiscoveryDegraded, "copilot", "model discovery failed: 502", "")
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("degraded at 59m produced evidence: %#v", ev)
	}
	advance(2 * time.Minute)
	tr.Observe(ClassModelDiscoveryDegraded, "copilot", "model discovery failed: 502", "")
	if ev := tr.Evidence(); len(ev) != 1 {
		t.Fatalf("degraded >1h did not produce evidence: %#v", ev)
	}
	advance(runtimeEventTTL + time.Minute)
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("stale condition survived TTL: %#v", ev)
	}
}

func TestRuntimeTrackerIgnoresUnknownClass(t *testing.T) {
	tr := NewRuntimeTracker(nil)
	tr.Observe("made_up", "k", "boom", "")
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("unknown class produced evidence: %#v", ev)
	}
}

func TestNormalizeMessageAndFingerprintStable(t *testing.T) {
	a := NormalizeMessage(`kick failed for agent "quality" after 3 tries: /data/agents/quality/log-20260924.txt sha 9f86d081884c7d659a2feaa0c55ad015`)
	b := NormalizeMessage(`kick failed for agent "security" after 7 tries: /data/agents/security/log-20260923.txt sha e3b0c44298fc1c149afbf4c8996fb924`)
	if a != b {
		t.Fatalf("normalisation differs:\n%s\n%s", a, b)
	}
	if strings.Contains(a, "quality") || strings.Contains(a, "9f86d081") || strings.Contains(a, "/data/agents") {
		t.Fatalf("normalised message leaks specifics: %s", a)
	}
	fp1 := RuntimeFingerprint(ClassKickFailedRepeatedly, a, "v5.1.0")
	fp2 := RuntimeFingerprint(ClassKickFailedRepeatedly, b, "v5.1.0")
	fp3 := RuntimeFingerprint(ClassKickFailedRepeatedly, a, "v5.2.0")
	if fp1 != fp2 || fp1 == fp3 || len(fp1) == 0 {
		t.Fatalf("fingerprints: %s %s %s", fp1, fp2, fp3)
	}
}

func TestBuildRuntimeReportCarriesMarkersAndLabels(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	ev := Evidence{Class: ClassRestartStorm, Component: "agent-runtime", Severity: "high", Count: 4, Window: 10 * time.Minute, Message: "restart storm: agent quality restarted 4 times in 10m", Diagnostics: "line1\ntoken=ghp_abcdefghijklmnopqrstuvwxyz0123456789\nline3", Attributable: true}
	fp := RuntimeFingerprint(ev.Class, NormalizeMessage(ev.Message), "v5.1.0")
	r := BuildRuntimeReport(Observation{HiveID: "hive-a", Version: "v5.1.0", HiveSlug: "hosted-aslom", Cluster: "oke-11"}, ev, fp, "inst", "hosted-aslom", "v5.1.0", "abcdef1", now)
	if !strings.HasPrefix(r.Title, "[selfreport] restart_storm: ") {
		t.Fatalf("title = %q", r.Title)
	}
	for _, want := range []string{"hive-fleet-fingerprint:" + fp, SelfReportMarker(fp, "hosted-aslom"), AffectedBlockStart, "**Affected hives (1):** hosted-aslom", "<details>"} {
		if !strings.Contains(r.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, r.Body)
		}
	}
	if strings.Contains(r.Body, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("body leaks token:\n%s", r.Body)
	}
	hasLabel := func(name string) bool {
		for _, l := range r.Labels {
			if l == name {
				return true
			}
		}
		return false
	}
	for _, l := range []string{LabelSelfReport, LabelKindBug, LabelNeedsTriage, LabelFleetReport, "class:restart-storm", "severity:high", "trigger:" + TriggerRuntimeError} {
		if !hasLabel(l) {
			t.Fatalf("labels missing %q: %v", l, r.Labels)
		}
	}
	if r.Class != ClassRestartStorm || r.Trigger != TriggerRuntimeError || r.Hive != "hosted-aslom" {
		t.Fatalf("report meta = %#v", r)
	}
}

func TestAffectedBlockRoundTripAndIdempotentUpdate(t *testing.T) {
	body := "intro\n\n" + RenderAffectedBlock([]string{"hive-b", "hive-a"}, 2) + "\n\ntrailer"
	hives, seen, ok := ParseAffectedBlock(body)
	if !ok || seen != 2 || strings.Join(hives, ",") != "hive-a,hive-b" {
		t.Fatalf("parse = %v %d %v", hives, seen, ok)
	}
	updated, changed := UpdateAffectedBlock(body, "hive-c", true)
	if !changed || !strings.Contains(updated, "**Affected hives (3):** hive-a, hive-b, hive-c") || !strings.Contains(updated, "seen_count: 3") {
		t.Fatalf("update:\n%s", updated)
	}
	if !strings.HasPrefix(updated, "intro\n\n") || !strings.HasSuffix(updated, "\n\ntrailer") {
		t.Fatalf("update clobbered surrounding body:\n%s", updated)
	}
	same, changed := UpdateAffectedBlock(updated, "hive-c", false)
	if changed || same != updated {
		t.Fatalf("re-adding an existing hive without bump changed the body")
	}
	appended, changed := UpdateAffectedBlock("human filed body", "hive-z", true)
	if !changed || !strings.Contains(appended, "human filed body") || !strings.Contains(appended, "**Affected hives (1):** hive-z") {
		t.Fatalf("append to body without block:\n%s", appended)
	}
}

func TestSeenAndClearedComments(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	c := SeenComment("fp1", "hosted-aslom", "v5.1.0", "oke-11", Evidence{Class: ClassInferenceAuthFailed, Count: 3, Message: "litellm 401", Diagnostics: "d"}, now)
	for _, want := range []string{SeenMarker("fp1", "hosted-aslom"), "Also seen on **hosted-aslom** (v5.1.0, oke-11) at 2026-09-24T12:00:00Z"} {
		if !strings.Contains(c, want) {
			t.Fatalf("seen comment missing %q:\n%s", want, c)
		}
	}
	cl := ClearedComment("fp1", "hosted-aslom", "v5.1.0", now)
	if !strings.Contains(cl, "Cleared on **hosted-aslom**") || !strings.Contains(cl, "2026-09-24T12:00:00Z") || strings.Contains(cl, "self-recovered: yes") {
		t.Fatalf("cleared comment:\n%s", cl)
	}
}

func TestFilingLimits(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	limits := DefaultFilingLimits.WithMaxPerDay(2)
	var st State
	if ok, _ := AllowNewFiling(st, now, limits); !ok {
		t.Fatal("fresh state must allow filing")
	}
	RecordFiling(&st, now)
	if ok, reason := AllowNewFiling(st, now.Add(30*time.Minute), limits); ok || !strings.Contains(reason, "min gap") {
		t.Fatalf("min gap not enforced: %v %q", ok, reason)
	}
	if ok, _ := AllowNewFiling(st, now.Add(time.Hour), limits); !ok {
		t.Fatal("1h after first filing must allow")
	}
	RecordFiling(&st, now.Add(time.Hour))
	if ok, reason := AllowNewFiling(st, now.Add(3*time.Hour), limits); ok || !strings.Contains(reason, "daily cap") {
		t.Fatalf("daily cap not enforced: %v %q", ok, reason)
	}
	if ok, _ := AllowNewFiling(st, now.Add(25*time.Hour), limits); !ok {
		t.Fatal("cap must roll off after 24h")
	}
	RecordFiling(&st, now.Add(25*time.Hour))
	if len(st.Filings) != 1 {
		t.Fatalf("RecordFiling did not prune: %v", st.Filings)
	}

	open := OpenIssue{LastSeenCommentAt: now}
	if AllowSeenComment(open, now.Add(23*time.Hour), limits) || !AllowSeenComment(open, now.Add(24*time.Hour), limits) || !AllowSeenComment(OpenIssue{}, now, limits) {
		t.Fatal("seen-comment gap wrong")
	}
	if !ClosedRecently(now, now.Add(6*24*time.Hour), limits) || ClosedRecently(now, now.Add(8*24*time.Hour), limits) || ClosedRecently(time.Time{}, now, limits) {
		t.Fatal("closed cooldown wrong")
	}
}

func TestEvaluateRuntimeEvidenceReEmitsUntilPostedAndRecovers(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	ev := Evidence{Class: ClassPanicRecovered, Component: "dashboard", Severity: "high", Count: 1, Message: "panic in handler: nil deref", Attributable: true}
	obs := Observation{EpochStart: now, HiveID: "hive-a", Version: "v5.1.0", Commit: "abcdef1", HiveSlug: "hosted-x", Evidence: []Evidence{ev}, Now: now}
	first := Evaluate(obs, State{}, false)
	if len(first.Reports) != 1 || first.Reports[0].Trigger != TriggerRuntimeError || first.Reports[0].Class != ClassPanicRecovered {
		t.Fatalf("first = %#v", first.Reports)
	}
	fp := first.Reports[0].Fingerprint
	// Not yet posted → re-emitted.
	second := Evaluate(obs, first.State, false)
	if len(second.Reports) != 1 {
		t.Fatalf("unposted report not re-emitted: %#v", second.Reports)
	}
	st := second.State
	o := st.Open[fp]
	o.Number, o.PostedAt = 42, now
	st.Open[fp] = o
	third := Evaluate(obs, st, false)
	if len(third.Reports) != 0 {
		t.Fatalf("posted report re-emitted: %#v", third.Reports)
	}
	// Condition gone → recovery for the runtime trigger, carrying class+hive.
	gone := obs
	gone.Evidence = nil
	rec := Evaluate(gone, third.State, false)
	if len(rec.Recoveries) != 1 || rec.Recoveries[0].Fingerprint != fp || rec.Recoveries[0].Trigger != TriggerRuntimeError || rec.Recoveries[0].Class != ClassPanicRecovered {
		t.Fatalf("recoveries = %#v", rec.Recoveries)
	}
	if !strings.Contains(rec.Recoveries[0].Body, "panic_recovered") {
		t.Fatalf("recovery body = %q", rec.Recoveries[0].Body)
	}
	// Runtime evidence must not leak into the hive-code-defect trigger.
	for _, r := range first.Reports {
		if r.Trigger == TriggerHiveDefect {
			t.Fatalf("runtime evidence produced a defect report: %#v", r)
		}
	}
}

func TestDefaultTrackerPackageHelpersAndEviction(t *testing.T) {
	now, advance := fakeClock(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	tr := NewRuntimeTracker(now)
	SetDefaultTrackerForTest(t, tr)
	if DefaultTracker() != tr {
		t.Fatal("SetDefaultTrackerForTest did not swap the default")
	}
	Observe(ClassRestartStorm, "a", "restart storm", "")
	if ev := DefaultTracker().Evidence(); len(ev) != 1 {
		t.Fatalf("package Observe did not reach the default tracker: %#v", ev)
	}
	Clear(ClassRestartStorm, "a")
	if ev := DefaultTracker().Evidence(); len(ev) != 0 {
		t.Fatalf("package Clear did not reach the default tracker: %#v", ev)
	}
	// Eviction keeps the map bounded: the oldest condition goes first.
	for i := 0; i < maxTrackedConditions+5; i++ {
		tr.Observe(ClassPanicRecovered, fmt.Sprintf("k%03d", i), "panic", "")
		advance(time.Second)
	}
	tr.mu.Lock()
	n := len(tr.conds)
	_, oldestKept := tr.conds[condKey(ClassPanicRecovered, "k000")]
	tr.mu.Unlock()
	if n != maxTrackedConditions || oldestKept {
		t.Fatalf("conds=%d oldestKept=%v", n, oldestKept)
	}
}

func TestUnclassifiedRepeatedUsesSlidingWindow(t *testing.T) {
	now, advance := fakeClock(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	tr := NewRuntimeTracker(now)
	for i := 0; i < unclassifiedRepeatCount-1; i++ {
		tr.Observe(ClassUnclassifiedRepeated, "x", "weird error", "")
		advance(time.Minute)
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("below window count produced evidence: %#v", ev)
	}
	tr.Observe(ClassUnclassifiedRepeated, "x", "weird error", "")
	if ev := tr.Evidence(); len(ev) != 1 {
		t.Fatalf("window count met but no evidence: %#v", ev)
	}
	advance(unclassifiedRepeatWindow + time.Minute)
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("events outside the window still counted: %#v", ev)
	}
}

func TestShortMessageDiagnosticsAndLabelHelpers(t *testing.T) {
	if got := ShortMessage(""); got != "unusual error" {
		t.Fatalf("empty short message = %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := ShortMessage(long); len([]rune(got)) != 80 || !strings.HasSuffix(got, "…") {
		t.Fatalf("long short message = %q", got)
	}
	big := strings.Repeat("line\n", maxDiagnosticsBytes)
	if got := boundDiagnostics(big); len(got) > maxDiagnosticsBytes+4 || !strings.HasPrefix(got, "…\n") {
		t.Fatalf("diagnostics not bounded: len=%d prefix=%q", len(got), got[:4])
	}
	if displayVersion("") != "vunknown" || displayVersion("v5.1.0") != "v5.1.0" || displayVersion("5.1.0") != "v5.1.0" {
		t.Fatalf("displayVersion: %q %q %q", displayVersion(""), displayVersion("v5.1.0"), displayVersion("5.1.0"))
	}
	// The hive/<slug> provenance label is retired (hivecommons/hive#12539):
	// neither a named nor an anonymous report carries one; the body marker
	// names the hive instead.
	for _, hive := range []string{"", "Hosted X"} {
		r := BuildRuntimeReport(Observation{}, Evidence{Class: ClassRestartStorm, Severity: "high"}, "fp", "inst", hive, "v5", "c", time.Now())
		for _, l := range r.Labels {
			if strings.HasPrefix(l, "hive/") && l != LabelSelfReport {
				t.Fatalf("report for %q carries %q", hive, l)
			}
		}
	}
	if r := BuildRuntimeReport(Observation{}, Evidence{Class: ClassRestartStorm, Severity: "high"}, "fp", "inst", "Hosted X", "v5", "c", time.Now()); !strings.Contains(r.Body, "hive=hosted-x") {
		t.Fatalf("report body does not name the hive: %q", r.Body)
	}
}

// TestEveryRuntimeClassTriggerAndNonTriggerThresholds walks the whole
// allow-list (#11881): each class is observed one step short of its
// threshold (no evidence) and then across it (exactly one evidence row).
func TestEveryRuntimeClassTriggerAndNonTriggerThresholds(t *testing.T) {
	cases := []struct {
		class   string
		message string
		// below drives the condition up to (but not across) its threshold;
		// cross then makes it qualify.
		below func(tr *RuntimeTracker, advance func(time.Duration))
		cross func(tr *RuntimeTracker, advance func(time.Duration))
	}{
		{class: ClassRestartStorm, message: "restart storm: agent quality restarted 4 times in 10m"},
		{class: ClassKickFailedRepeatedly, message: "kick undeliverable 3 times in a row (busy)"},
		{class: ClassPanicRecovered, message: "panic in kickBrainstorm: runtime error: invalid memory address"},
		{class: ClassModelDiscoveryDegraded, message: "model discovery failed: 502 bad gateway"},
		{class: ClassInferenceAuthFailed, message: "litellm returned HTTP 401 unauthorized"},
		{class: ClassAdvisoryPostFailed, message: "advisory digest post failed: HTTP 422"},
		{class: ClassUpgradeStalled, message: "standalone upgrade helper did not finish within 10m0s"},
		{class: ClassDataDiskPressure, message: "data disk pressure: disk-full: /data is 97% used (threshold 95%)"},
		{class: ClassGitHubAppTokenMintFailed, message: "github app token mint failed: POST /app/installations/123/access_tokens: 401"},
		{class: ClassUnexpectedHub5xx, message: "hub heartbeat returned HTTP 503"},
		{class: ClassUnclassifiedRepeated, message: "weird error from somewhere"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		seen[tc.class] = true
		t.Run(tc.class, func(t *testing.T) {
			now, advance := fakeClock(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
			tr := NewRuntimeTracker(now)
			spec := Classes[tc.class]
			observe := func() { tr.Observe(tc.class, "scope", tc.message, "") }
			min := spec.MinCount
			if min < 1 {
				min = 1
			}
			// Non-trigger: one observation short of MinCount, or (for
			// duration classes) every observation but before MinDuration.
			if spec.MinDuration > 0 {
				for i := 0; i < min; i++ {
					observe()
				}
				advance(spec.MinDuration - time.Minute)
				observe()
			} else {
				for i := 0; i < min-1; i++ {
					observe()
					advance(time.Second)
				}
			}
			if ev := tr.Evidence(); len(ev) != 0 {
				t.Fatalf("%s below threshold produced evidence: %#v", tc.class, ev)
			}
			// Trigger.
			if spec.MinDuration > 0 {
				advance(2 * time.Minute)
			}
			observe()
			ev := tr.Evidence()
			if len(ev) != 1 || ev[0].Class != tc.class || !ev[0].Attributable || ev[0].Component != spec.Component || ev[0].Severity != spec.Severity {
				t.Fatalf("%s at threshold: evidence = %#v", tc.class, ev)
			}
			// A cleared condition stops producing evidence.
			tr.Clear(tc.class, "scope")
			if ev := tr.Evidence(); len(ev) != 0 {
				t.Fatalf("%s cleared but still reported: %#v", tc.class, ev)
			}
		})
	}
	for class := range Classes {
		if !seen[class] {
			t.Errorf("class %s has no threshold case", class)
		}
	}
}

func TestRuntimeClassThresholdValues(t *testing.T) {
	if got := Classes[ClassKickFailedRepeatedly].MinCount; got != 3 {
		t.Fatalf("kick_failed_repeatedly MinCount = %d, want 3 consecutive", got)
	}
	if got := Classes[ClassModelDiscoveryDegraded].MinDuration; got != time.Hour {
		t.Fatalf("model_discovery_degraded MinDuration = %s, want 1h", got)
	}
	if s := Classes[ClassUnclassifiedRepeated]; s.MinCount != 5 || s.Window != 10*time.Minute {
		t.Fatalf("unclassified_error_repeated = %d in %s, want 5 in 10m", s.MinCount, s.Window)
	}
	if got := Classes[ClassUnexpectedHub5xx].MinCount; got != hub5xxStreak {
		t.Fatalf("unexpected_http_5xx_from_hub MinCount = %d, want %d", got, hub5xxStreak)
	}
}

func TestRuntimeFingerprintIgnoresIDsTimestampsAndPorts(t *testing.T) {
	pairs := [][2]string{
		{"hub heartbeat to 10.0.3.17:8443 failed at 2026-09-24T12:00:00Z", "hub heartbeat to 10.0.9.2:9443 failed at 2026-10-01T03:14:15Z"},
		{"dial tcp 127.0.0.1:41235: connect: connection refused", "dial tcp 127.0.0.1:50001: connect: connection refused"},
		{"request 3f2b9c1e-8d4a-4b7e-9c2d-1a2b3c4d5e6f failed after 1234ms", "request 9a8b7c6d-5e4f-4a3b-8c2d-0f1e2d3c4b5a failed after 87ms"},
		{"installation 123456 token mint failed (pid 4242)", "installation 987 token mint failed (pid 17)"},
	}
	for _, p := range pairs {
		if NormalizeMessage(p[0]) != NormalizeMessage(p[1]) {
			t.Fatalf("volatile parts split the message:\n%q\n%q", NormalizeMessage(p[0]), NormalizeMessage(p[1]))
		}
		if RuntimeFingerprint(ClassUnexpectedHub5xx, p[0], "v5.1.0") != RuntimeFingerprint(ClassUnexpectedHub5xx, p[1], "v5.1.0") {
			t.Fatalf("volatile parts changed the fingerprint: %q vs %q", p[0], p[1])
		}
		if RuntimeFingerprint(ClassUnexpectedHub5xx, p[0], "v5.1.0") == RuntimeFingerprint(ClassUnexpectedHub5xx, p[0], "v5.1.1") {
			t.Fatalf("version change did not change the fingerprint for %q", p[0])
		}
		if RuntimeFingerprint(ClassUnexpectedHub5xx, p[0], "v5.1.0") == RuntimeFingerprint(ClassUpgradeStalled, p[0], "v5.1.0") {
			t.Fatalf("class change did not change the fingerprint for %q", p[0])
		}
	}
	if RuntimeFingerprint(ClassPanicRecovered, "panic in handler: nil map", "v5") == RuntimeFingerprint(ClassPanicRecovered, "panic in handler: index out of range", "v5") {
		t.Fatal("different messages collapsed to one fingerprint")
	}
}

func TestRuntimeDiagnosticsNeverCarryTokens(t *testing.T) {
	secrets := []string{
		"ghp_" + strings.Repeat("a", 36),
		"ghs_" + strings.Repeat("B", 36),
		"github_pat_" + strings.Repeat("c", 40),
		"sk-hive-" + strings.Repeat("d", 24),
		"sk-ant-" + strings.Repeat("e", 30),
		"eyJ" + strings.Repeat("f", 24) + "." + strings.Repeat("g", 24) + "." + strings.Repeat("h", 24),
		"Bearer " + strings.Repeat("i", 40),
	}
	// Secrets sit at the tail so they survive the size bound unless scrubbed.
	diag := strings.Repeat("x", maxDiagnosticsBytes) + "\nhelper output:\n" + strings.Join(secrets, "\nAuthorization: ")
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	tr := NewRuntimeTracker(func() time.Time { return now })
	for class := range Classes {
		tr.Observe(class, "scope", "failed with "+strings.Join(secrets, " "), diag)
	}
	// Past every MinDuration, then enough observations to meet every
	// MinCount inside every Window.
	now = now.Add(2 * time.Hour)
	for class := range Classes {
		for i := 0; i < unclassifiedRepeatCount; i++ {
			tr.Observe(class, "scope", "failed with "+strings.Join(secrets, " "), diag)
		}
	}
	evs := tr.Evidence()
	if len(evs) != len(Classes) {
		t.Fatalf("expected one evidence per class, got %d", len(evs))
	}
	for _, ev := range evs {
		if len(ev.Diagnostics) > maxDiagnosticsBytes+4 {
			t.Fatalf("%s diagnostics not size-bounded: %d bytes", ev.Class, len(ev.Diagnostics))
		}
		fp := RuntimeFingerprint(ev.Class, ev.Message, "v5.1.0")
		r := BuildRuntimeReport(Observation{}, ev, fp, "inst", "hosted-x", "v5.1.0", "abc", now)
		comment := SeenComment(fp, "hosted-x", "v5.1.0", "oke", ev, now)
		for _, s := range secrets {
			for name, text := range map[string]string{"diagnostics": ev.Diagnostics, "message": ev.Message, "title": r.Title, "body": r.Body, "seen comment": comment} {
				if strings.Contains(text, s) {
					t.Fatalf("%s %s leaks %q:\n%s", ev.Class, name, s[:6], text)
				}
			}
		}
		if logscrubTokenLike(ev.Diagnostics) || logscrubTokenLike(r.Body) {
			t.Fatalf("%s carries a token-like string:\n%s", ev.Class, r.Body)
		}
	}
}

// tokenLikeRE adds the sk- API-key shape to logscrub's GitHub/JWT pattern.
var tokenLikeRE = regexp.MustCompile(`sk-[A-Za-z0-9]{6,}`)

func logscrubTokenLike(s string) bool {
	return logscrub.TokenPattern.MatchString(s) || tokenLikeRE.MatchString(s)
}

func TestBuildRuntimeReportCarriesReposOwnersAndItem(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ev := Evidence{Class: ClassAdvisoryPostFailed, Component: "github-client", Severity: "medium", Count: 1, Message: "advisory digest post failed: 403", Repo: "Acme/Widgets", Ref: "#42"}
	fp := RuntimeFingerprint(ev.Class, ev.Message, "v5.1.0")
	obs := Observation{HiveID: "hive-a", Version: "v5.1.0", HiveSlug: "hosted-aslom", Repos: []string{"hivecommons/hive", "HiveCommons/pluk", "hivecommons/hive", "not a repo", "https://github.com/acme/tools.git"}}
	r := BuildRuntimeReport(obs, ev, fp, "inst", "hosted-aslom", "v5.1.0", "abcdef1", now)
	for _, want := range []string{
		"| repos | 3: acme/tools, hivecommons/hive, hivecommons/pluk |",
		"| owners | 2: acme, hivecommons |",
		"| repo | acme/widgets |",
		"| ref | acme/widgets#42 |",
		"**Affected hives (1):** hosted-aslom\n**Affected repos (4):** acme/tools, acme/widgets, hivecommons/hive, hivecommons/pluk\n**Affected owners (2):** acme, hivecommons\nseen_count: 1",
	} {
		if !strings.Contains(r.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, r.Body)
		}
	}
	if strings.Join(r.Repos, ",") != "acme/tools,acme/widgets,hivecommons/hive,hivecommons/pluk" {
		t.Fatalf("report repos = %v", r.Repos)
	}
	if strings.Contains(r.Body, "not a repo") {
		t.Fatalf("malformed repo reached the body:\n%s", r.Body)
	}
}

func TestBuildRuntimeReportWithoutReposRendersDashesAndLegacyBlock(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ev := Evidence{Class: ClassRestartStorm, Component: "agent-runtime", Severity: "high", Count: 4, Message: "restart storm: agent quality restarted 4 times in 10m"}
	r := BuildRuntimeReport(Observation{HiveID: "hive-a", Version: "v5.1.0"}, ev, "fp", "inst", "", "v5.1.0", "abcdef1", now)
	for _, want := range []string{"| repos | — |", "| owners | — |", "| repo | — |", "| ref | — |", AffectedBlockStart + "\n**Affected hives (1):** inst\nseen_count: 1\n" + AffectedBlockEnd} {
		if !strings.Contains(r.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, r.Body)
		}
	}
	if strings.Contains(r.Body, "Affected repos") || strings.Contains(r.Body, "Affected owners") || r.Repos != nil {
		t.Fatalf("opted-out hive still named repos:\n%s", r.Body)
	}
}

func TestAffectedBlockReposUnionAcrossHivesIsIdempotent(t *testing.T) {
	body := "intro\n\n" + RenderAffectedBlockWithRepos([]string{"hive-a"}, []string{"hivecommons/hive"}, 1) + "\n\ntrailer"
	hives, repos, seen, ok := ParseAffectedBlockWithRepos(body)
	if !ok || seen != 1 || strings.Join(hives, ",") != "hive-a" || strings.Join(repos, ",") != "hivecommons/hive" {
		t.Fatalf("parse = %v %v %d %v", hives, repos, seen, ok)
	}

	updated, changed := UpdateAffectedBlockWithRepos(body, "hive-b", []string{"acme/widgets", "hivecommons/hive", "hivecommons/pluk"}, true)
	wantBlock := AffectedBlockStart + "\n**Affected hives (2):** hive-a, hive-b\n**Affected repos (3):** acme/widgets, hivecommons/hive, hivecommons/pluk\n**Affected owners (2):** acme, hivecommons\nseen_count: 2\n" + AffectedBlockEnd
	if !changed || !strings.Contains(updated, wantBlock) {
		t.Fatalf("union update:\n%s\nwant block:\n%s", updated, wantBlock)
	}
	if !strings.HasPrefix(updated, "intro\n\n") || !strings.HasSuffix(updated, "\n\ntrailer") {
		t.Fatalf("update clobbered surrounding body:\n%s", updated)
	}

	// Re-reporting the same hive with the same repos and no bump is a no-op.
	same, changed := UpdateAffectedBlockWithRepos(updated, "hive-b", []string{"hivecommons/pluk", "acme/widgets"}, false)
	if changed || same != updated {
		t.Fatalf("idempotent re-report changed the body:\n%s", same)
	}
	// A known hive bringing one new repo edits the body even without a bump.
	grown, changed := UpdateAffectedBlockWithRepos(updated, "hive-a", []string{"acme/tools"}, false)
	if !changed || !strings.Contains(grown, "**Affected repos (4):** acme/tools, acme/widgets, hivecommons/hive, hivecommons/pluk") || !strings.Contains(grown, "**Affected hives (2):** hive-a, hive-b") || !strings.Contains(grown, "seen_count: 2") {
		t.Fatalf("new repo from a known hive:\n%s", grown)
	}

	// Legacy blocks (hives only) keep their shape until a hive brings repos.
	legacy := RenderAffectedBlock([]string{"hive-a"}, 1)
	kept, changed := UpdateAffectedBlockWithRepos(legacy, "hive-c", nil, true)
	if !changed || strings.Contains(kept, "Affected repos") || !strings.Contains(kept, "**Affected hives (2):** hive-a, hive-c") {
		t.Fatalf("legacy block grew repo lines without repos:\n%s", kept)
	}
	// A human-filed body with no block gets one appended, repos included.
	appended, changed := UpdateAffectedBlockWithRepos("human filed body", "hive-z", []string{"acme/widgets"}, true)
	if !changed || !strings.HasPrefix(appended, "human filed body") || !strings.Contains(appended, "**Affected owners (1):** acme") {
		t.Fatalf("append to body without block:\n%s", appended)
	}
}

func TestRuntimeFingerprintIgnoresReposAndItem(t *testing.T) {
	// Repos and the triggering item live outside the fingerprint, so two hives
	// watching different repos still land on one issue.
	fp := RuntimeFingerprint(ClassAdvisoryPostFailed, "advisory digest post failed: 403", "v5.1.0")
	a := BuildRuntimeReport(Observation{Repos: []string{"acme/widgets"}}, Evidence{Class: ClassAdvisoryPostFailed, Message: "advisory digest post failed: 403", Repo: "acme/widgets", Ref: "#1"}, fp, "inst-a", "hive-a", "v5.1.0", "c", time.Time{})
	b := BuildRuntimeReport(Observation{Repos: []string{"hivecommons/hive", "hivecommons/pluk"}}, Evidence{Class: ClassAdvisoryPostFailed, Message: "advisory digest post failed: 403", Repo: "hivecommons/hive"}, fp, "inst-b", "hive-b", "v5.1.0", "c", time.Time{})
	if a.Fingerprint != b.Fingerprint || a.Fingerprint != fp {
		t.Fatalf("fingerprints differ: %s vs %s (want %s)", a.Fingerprint, b.Fingerprint, fp)
	}
	if !strings.Contains(a.Body, "hive-fleet-fingerprint:"+fp) || !strings.Contains(b.Body, "hive-fleet-fingerprint:"+fp) {
		t.Fatal("body fingerprint marker drifted")
	}
}

func TestObserveItemCarriesRepoAndRefIntoEvidence(t *testing.T) {
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tr := NewRuntimeTracker(func() time.Time { return clock })
	tr.ObserveItem(ClassAdvisoryPostFailed, "acme/widgets", "advisory digest post failed: 403", "", "Acme/Widgets", "#7")
	tr.Observe(ClassAdvisoryPostFailed, "acme/widgets", "advisory digest post failed: 403 again", "")
	evs := tr.Evidence()
	if len(evs) != 1 || evs[0].Repo != "acme/widgets" || evs[0].Ref != "#7" {
		t.Fatalf("evidence = %+v", evs)
	}
	if got := NormalizeRepo("ghp_abcdefghijklmnopqrstuvwxyz0123456789/x"); got != "" {
		t.Fatalf("token-shaped repo survived normalisation: %q", got)
	}
	if got := strings.Join(OwnersOf([]string{"B/x", "a/y", "a/z", "bad"}), ","); got != "a,b" {
		t.Fatalf("owners = %q", got)
	}
}

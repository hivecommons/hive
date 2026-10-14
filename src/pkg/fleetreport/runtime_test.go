package fleetreport

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
	for _, l := range []string{LabelSelfReport, LabelKindBug, LabelNeedsTriage, LabelFleetReport, "class:restart-storm", "severity:high", "trigger:" + TriggerRuntimeError, "hive/hosted-aslom"} {
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
	if HiveLabel("") != "" || HiveLabel("Hosted X") != "hive/hosted-x" {
		t.Fatalf("HiveLabel = %q %q", HiveLabel(""), HiveLabel("Hosted X"))
	}
	if displayVersion("") != "vunknown" || displayVersion("v5.1.0") != "v5.1.0" || displayVersion("5.1.0") != "v5.1.0" {
		t.Fatalf("displayVersion: %q %q %q", displayVersion(""), displayVersion("v5.1.0"), displayVersion("5.1.0"))
	}
	// Anonymous hives (no slug) carry no hive/<slug> label.
	r := BuildRuntimeReport(Observation{}, Evidence{Class: ClassRestartStorm, Severity: "high"}, "fp", "inst", "", "v5", "c", time.Now())
	for _, l := range r.Labels {
		if strings.HasPrefix(l, "hive/") && l != LabelSelfReport {
			t.Fatalf("anonymous report carries %q", l)
		}
	}
}

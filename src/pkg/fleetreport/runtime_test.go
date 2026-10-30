package fleetreport

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/logscrub"
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
	fp1 := RuntimeFingerprint(ClassKickFailedRepeatedly, a)
	fp2 := RuntimeFingerprint(ClassKickFailedRepeatedly, b)
	if fp1 != fp2 || len(fp1) == 0 {
		t.Fatalf("fingerprints: %s %s", fp1, fp2)
	}
}

func TestBuildRuntimeReportCarriesMarkersAndLabels(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	ev := Evidence{Class: ClassRestartStorm, Component: "agent-runtime", Severity: "high", Count: 4, Window: 10 * time.Minute, Message: "restart storm: agent quality restarted 4 times in 10m", Diagnostics: "line1\ntoken=ghp_abcdefghijklmnopqrstuvwxyz0123456789\nline3", Attributable: true}
	fp := RuntimeFingerprint(ev.Class, NormalizeMessage(ev.Message))
	r := BuildRuntimeReport(Observation{HiveID: "hive-a", Version: "v5.1.0", HiveSlug: "hosted-aslom", Cluster: "oke-11"}, ev, fp, "inst", "hosted-aslom", "v5.1.0", "abcdef1", now)
	if !strings.HasPrefix(r.Title, "[selfreport] restart_storm: ") {
		t.Fatalf("title = %q", r.Title)
	}
	for _, want := range []string{"hive-fleet-fingerprint:" + fp, SelfReportMarker(fp, "hosted-aslom"), AffectedBlockStart, "**Affected hives (1):**\n- hosted-aslom (instance inst) — owner(s): unknown — seen 1, last 2026-09-24T12:00:00Z", "<details>"} {
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
	if !changed || !strings.Contains(updated, "**Affected hives (3):**\n- hive-a — owner(s): unknown — seen 1\n- hive-b — owner(s): unknown — seen 1\n- hive-c — owner(s): unknown — seen 1\n") || !strings.Contains(updated, "seen_count: 3") {
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
	if !changed || !strings.Contains(appended, "human filed body") || !strings.Contains(appended, "**Affected hives (1):**\n- hive-z — owner(s): unknown — seen 1\n") {
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
	// Runtime evidence only ever files under the runtime-error trigger.
	for _, r := range first.Reports {
		if r.Trigger != TriggerRuntimeError {
			t.Fatalf("runtime evidence produced a non-runtime report: %#v", r)
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
		{class: ClassAgentStall, message: "agent stalled after kick: 3 continue nudges sent without the pane moving"},
		{class: ClassAgentCrashLoop, message: "agent crash loop: 5 failures within the backoff window"},
		{class: ClassCIPollNudge, message: "ci-poll nudge: agent polling ci 4 times since last kick"},
		{class: ClassIssueRequestStuck, message: "issue-request stuck: issue request failed 3 attempts over 7m"},
		{class: ClassTmuxSessionCheckFailed, message: "tmux has-session failed"},
		{class: ClassAgentPaneTimeout, message: "pane content at timeout"},
		{class: ClassStartupKickInterrupted, message: "startup kick dropped: agent restarted or stopped while waiting"},
		{class: ClassProxyTLSHandshakeFailed, message: "proxy client TLS handshake failed"},
		{class: ClassAgentDirSetupFailed, message: "failed to create per-agent XDG dir"},
		{class: ClassReviewEvidenceWriteFail, message: "review evidence: could not write bundle"},
		{class: ClassFeedbackIssueRefreshFailed, message: "feedback: issue refresh failed"},
		{class: ClassSharedCIRerunForbidden, message: "shared-ci rerun forbidden: the App installation lacks Actions write permission for rerun-failed-jobs"},
		{class: ClassTranscriptScanSlow, message: "copilot transcript activity scan outlived its interval; agent liveness reads may lag"},
		{class: ClassBeadFactIngestFailed, message: "failed to ingest bead fact"},
		{class: ClassSelfReportClearedFailed, message: "fleet report: cleared comment failed"},
		{class: ClassGitHubAppRepoNotCovered, message: "github app recheck: installation does not cover every configured repo"},
		{class: ClassGitHubAppNotInstalled, message: "github app installation id is unset — per-agent scoped token NOT delivered"},
		{class: ClassProxyConnectDialFailed, message: "proxy: CONNECT dial failed"},
		{class: ClassProxyPlainForwardRefused, message: "proxy: plain HTTP forward refused — destination is not globally routable"},
		{class: ClassInferenceCallerUnknown, message: "inference request from unidentified caller refused"},
		{class: ClassInferenceUpstreamError, message: "inference upstream error"},
		{class: ClassSHAPollCommitMsgUnavail, message: "SHA poll: commit message fetch non-200"},
		{class: ClassKickFreshnessWithheld, message: "kick withheld: every ref failed the freshness check"},
		{class: ClassAgentLoginRequired, message: "login required detected"},
		{class: ClassPRRequestHeadUnpushed, message: "pr-request watcher: head branch is not on the remote — the agent's push did not authenticate"},
		{class: ClassIOScanClassifierUnavail, message: "ioscan classifier failed closed; text withheld"},
		{class: ClassIOScanClassifierUnavail, message: "ioscan classifier failed open; text passes on unclassified"},
		{class: ClassIOScanClassifierUnavail, message: "ioscan fail-closed blocked kick"},
		{class: ClassIOScanClassifierUnavail, message: "ioscan fail-closed blocked held-PR coordination kick"},
		{class: ClassGitHubFetchTransient, message: "channel commit date: fetch failed — the row will render without a timestamp"},
		{class: ClassLabelProvisioningFailed, message: "issue stage labels: provisioning failed"},
		{class: ClassAgentCLIRestartLoop, message: "agent CLI crashed or stuck on consent screen, restarting before kick"},
		{class: ClassEffectLedgerOpStuck, message: "effect ledger op stuck: issue_create has been Unknown for 31m; retries refused pending reconciliation"},
		{class: ClassCruxRuntimeUnavailable, message: "crux runtime unavailable: crux server unreachable at /run/hive/crux.sock"},
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
	// The folded hive-code-defect triggers (#13470) keep the thresholds the
	// dashboard synthesis used: three unrecovered stall nudges, one watchdog
	// crash-loop latch, one CI-poll nudge per 24h window.
	if s := Classes[ClassAgentStall]; s.MinCount != AgentStallNudgeStreak || AgentStallNudgeStreak != 3 || s.Severity != "medium" || s.Component != "agent-runtime" {
		t.Fatalf("agent_stall = %+v", s)
	}
	if s := Classes[ClassAgentCrashLoop]; s.MinCount != 1 || s.Severity != "high" || s.Component != "agent-runtime" {
		t.Fatalf("agent_crash_loop = %+v", s)
	}
	if s := Classes[ClassCIPollNudge]; s.MinCount != 1 || s.Window != 24*time.Hour || s.Severity != "low" {
		t.Fatalf("ci_poll_nudge = %+v", s)
	}
	// Queue-health classes: the owning component applies the stuck
	// threshold (attempts/age), so the class fires on the first observation
	// and clears the moment the work completes or is quarantined.
	if s := Classes[ClassIssueRequestStuck]; s.MinCount != 1 || s.Severity != "medium" || s.Component != "github-relay" || s.MinDuration != 0 {
		t.Fatalf("issue_request_stuck = %+v", s)
	}
	if s := Classes[ClassEffectLedgerOpStuck]; s.MinCount != 1 || s.Severity != "medium" || s.Component != "mutation-ledger" || s.MinDuration != 0 {
		t.Fatalf("effect_ledger_op_stuck = %+v", s)
	}
	if s := Classes[ClassUnclassifiedRepeated]; s.IdleClear != 2*s.Window {
		t.Fatalf("unclassified_error_repeated IdleClear = %s, want 2× window", s.IdleClear)
	}
}

// The three folded classes behave like every other self-report: the stall
// streak files once at the threshold, a recovery clears it so the next
// streak starts from zero, the fingerprint ignores the per-event numbers and
// the diagnostics are scrubbed.
func TestFoldedDefectClassesThresholdDedupAndScrubbing(t *testing.T) {
	now, advance := fakeClock(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	tr := NewRuntimeTracker(now)
	msg := func(n int) string {
		return fmt.Sprintf("agent stalled after kick: %d continue nudge(s) sent without the pane moving (last kick 2026-10-21T11:58:00Z, 2m0s ago; pane frozen at hash deadbeefcafe)", n)
	}
	for i := 1; i < AgentStallNudgeStreak; i++ {
		tr.Observe(ClassAgentStall, "scanner", msg(i), "agent=scanner lane=scanner token ghp_abcdefghijklmnopqrstuvwxyz123456")
		advance(time.Minute)
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("stall below the streak produced evidence: %#v", ev)
	}
	tr.Observe(ClassAgentStall, "scanner", msg(3), "agent=scanner lane=scanner token ghp_abcdefghijklmnopqrstuvwxyz123456")
	ev := tr.Evidence()
	if len(ev) != 1 || ev[0].Count != 3 || ev[0].Agent != "scanner" || ev[0].Class != ClassAgentStall {
		t.Fatalf("stall evidence = %#v", ev)
	}
	if strings.Contains(ev[0].Diagnostics, "ghp_abcdefghijklmnopqrstuvwxyz123456") || !strings.Contains(ev[0].Diagnostics, "lane=scanner") {
		t.Fatalf("stall diagnostics not scrubbed / lost context: %q", ev[0].Diagnostics)
	}
	if !strings.Contains(ev[0].Message, "continue nudge") {
		t.Fatalf("stall message lost its context: %q", ev[0].Message)
	}
	// Per-event numbers (count, kick time, pane hash) never fork the issue.
	if RuntimeFingerprint(ClassAgentStall, NormalizeMessage(msg(3))) != RuntimeFingerprint(ClassAgentStall, NormalizeMessage(msg(7))) {
		t.Fatal("stall fingerprint varies with the nudge count")
	}
	// Two agents stalling are two conditions on one fingerprint, not two issues.
	tr.Observe(ClassAgentStall, "quality", msg(3), "")
	tr.Observe(ClassAgentStall, "quality", msg(4), "")
	tr.Observe(ClassAgentStall, "quality", msg(5), "")
	if ev := tr.Evidence(); len(ev) != 2 {
		t.Fatalf("expected one condition per agent, got %#v", ev)
	}
	// Recovery clears the streak; a single new nudge is not a report.
	tr.Clear(ClassAgentStall, "scanner")
	tr.Observe(ClassAgentStall, "scanner", msg(1), "")
	for _, e := range tr.Evidence() {
		if e.Agent == "scanner" {
			t.Fatalf("stall streak survived a recovery: %#v", e)
		}
	}

	// Crash loop: the watchdog latch is the threshold; one observation files.
	tr.Observe(ClassAgentCrashLoop, "quality", "agent crash loop: 5 failures within the backoff window", "agent=quality class=crash failures=5 cap=5")
	var crash []Evidence
	for _, e := range tr.Evidence() {
		if e.Class == ClassAgentCrashLoop {
			crash = append(crash, e)
		}
	}
	if len(crash) != 1 || crash[0].Severity != "high" || crash[0].Count != 1 {
		t.Fatalf("crash-loop evidence = %#v", crash)
	}
	tr.Clear(ClassAgentCrashLoop, "quality")

	// CI-poll nudge: one per window; the 24h window keeps a stale nudge from
	// resurfacing after the evidence TTL.
	tr.Observe(ClassCIPollNudge, "scanner", "ci-poll nudge: agent polling ci 4 times since last kick", "")
	found := false
	for _, e := range tr.Evidence() {
		if e.Class == ClassCIPollNudge {
			found = true
			if e.Window != 24*time.Hour || e.Severity != "low" {
				t.Fatalf("ci-poll evidence = %#v", e)
			}
		}
	}
	if !found {
		t.Fatal("ci-poll nudge did not qualify on first observation")
	}
	advance(25 * time.Hour)
	for _, e := range tr.Evidence() {
		if e.Class == ClassCIPollNudge {
			t.Fatalf("stale ci-poll nudge still reported: %#v", e)
		}
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
		if RuntimeFingerprint(ClassUnexpectedHub5xx, p[0]) != RuntimeFingerprint(ClassUnexpectedHub5xx, p[1]) {
			t.Fatalf("volatile parts changed the fingerprint: %q vs %q", p[0], p[1])
		}
		if RuntimeFingerprint(ClassUnexpectedHub5xx, p[0]) == RuntimeFingerprint(ClassUpgradeStalled, p[0]) {
			t.Fatalf("class change did not change the fingerprint for %q", p[0])
		}
	}
	if RuntimeFingerprint(ClassPanicRecovered, "panic in handler: nil map") == RuntimeFingerprint(ClassPanicRecovered, "panic in handler: index out of range") {
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
		fp := RuntimeFingerprint(ev.Class, ev.Message)
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
	fp := RuntimeFingerprint(ev.Class, ev.Message)
	obs := Observation{HiveID: "hive-a", Version: "v5.1.0", HiveSlug: "hosted-aslom", Repos: []string{"hivecommons/hive", "HiveCommons/pluk", "hivecommons/hive", "not a repo", "https://github.com/acme/tools.git"}}
	r := BuildRuntimeReport(obs, ev, fp, "inst", "hosted-aslom", "v5.1.0", "abcdef1", now)
	for _, want := range []string{
		"| repos | 3: acme/tools, hivecommons/hive, hivecommons/pluk |",
		"| owners | 2: acme, hivecommons |",
		"| repo | acme/widgets |",
		"| ref | acme/widgets#42 |",
		"**Affected hives (1):**\n- hosted-aslom (instance inst) — owner(s): unknown — seen 1, last 2026-10-16T12:00:00Z\n**Affected repos (4):** acme/tools, acme/widgets, hivecommons/hive, hivecommons/pluk\n**Affected repo owners (2):** acme, hivecommons\nseen_count: 1",
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
	for _, want := range []string{"| repos | — |", "| owners | — |", "| repo | — |", "| ref | — |", AffectedBlockStart + "\n**Affected hives (1):**\n- inst — owner(s): unknown — seen 1, last 2026-10-16T12:00:00Z\nseen_count: 1\n" + AffectedBlockEnd} {
		if !strings.Contains(r.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, r.Body)
		}
	}
	if strings.Contains(r.Body, "Affected repos") || strings.Contains(r.Body, "Affected repo owners") || r.Repos != nil {
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
	wantBlock := AffectedBlockStart + "\n**Affected hives (2):**\n- hive-a — owner(s): unknown — seen 1\n- hive-b — owner(s): unknown — seen 1\n**Affected repos (3):** acme/widgets, hivecommons/hive, hivecommons/pluk\n**Affected repo owners (2):** acme, hivecommons\nseen_count: 2\n" + AffectedBlockEnd
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
	if !changed || !strings.Contains(grown, "**Affected repos (4):** acme/tools, acme/widgets, hivecommons/hive, hivecommons/pluk") || !strings.Contains(grown, "**Affected hives (2):**\n- hive-a — owner(s): unknown — seen 1\n- hive-b") || !strings.Contains(grown, "seen_count: 2") {
		t.Fatalf("new repo from a known hive:\n%s", grown)
	}

	// Legacy blocks (hives only) keep their shape until a hive brings repos.
	legacy := RenderAffectedBlock([]string{"hive-a"}, 1)
	kept, changed := UpdateAffectedBlockWithRepos(legacy, "hive-c", nil, true)
	if !changed || strings.Contains(kept, "Affected repos") || !strings.Contains(kept, "**Affected hives (2):**\n- hive-a — owner(s): unknown — seen 1\n- hive-c — owner(s): unknown — seen 1\n") {
		t.Fatalf("legacy block grew repo lines without repos:\n%s", kept)
	}
	// A human-filed body with no block gets one appended, repos included.
	appended, changed := UpdateAffectedBlockWithRepos("human filed body", "hive-z", []string{"acme/widgets"}, true)
	if !changed || !strings.HasPrefix(appended, "human filed body") || !strings.Contains(appended, "**Affected repo owners (1):** acme") {
		t.Fatalf("append to body without block:\n%s", appended)
	}
}

func TestRuntimeFingerprintIgnoresReposAndItem(t *testing.T) {
	// Repos and the triggering item live outside the fingerprint, so two hives
	// watching different repos still land on one issue.
	fp := RuntimeFingerprint(ClassAdvisoryPostFailed, "advisory digest post failed: 403")
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

func TestMatchesExplicitClass(t *testing.T) {
	if !MatchesExplicitClass("Kick FAILED for agent quality") {
		t.Fatal("kick failed not matched")
	}
	if !MatchesExplicitClass("continuous re-kick backed off after failed kick") {
		t.Fatal("self-healing re-kick backoff not matched")
	}
	if !MatchesExplicitClass("issue-request watcher: request exceeded retry horizon") {
		t.Fatal("issue-request watcher keyword not matched")
	}
	if MatchesExplicitClass("some brand new failure nobody classified") {
		t.Fatal("unclassified message matched an explicit class")
	}
}

func TestIsBenignMessage(t *testing.T) {
	for _, m := range []string{"read tcp 1.2.3.4: i/o timeout", "keepalive ping: i/o timeout", "fetch: context canceled", "permissions watcher: fixed file permissions path=/x old_mode=-rw-r----- new_mode=-rw-rw-----", "permissions watcher: fixed ownership path=/x uid=1000", "shadow fence would have denied external mutation"} {
		if !IsBenignMessage(m) {
			t.Fatalf("%q not benign", m)
		}
	}
	for _, m := range []string{"write failed: i/o timeout", "request failed: 500", "permissions watcher: chmod failed path=/x"} {
		if IsBenignMessage(m) {
			t.Fatalf("%q wrongly benign", m)
		}
	}
}

func TestIsGitHubRateLimited(t *testing.T) {
	for _, m := range []string{
		"GET https://api.github.com/repos/o/r/pulls/798: 403 API secondary rate limit exceeded until 2026-10-28 17:42:28 -0400 EDT, not making remote request.",
		"GET https://api.github.com/repos/o/r: 403 API rate limit of 5000 still exceeded until 2026-10-28 17:42:28 -0400 EDT, not making remote request.",
		"GET https://api.github.com/repos/o/r: 403 API rate limit exceeded for installation ID 1.",
		"POST https://api.github.com/repos/o/r/issues: 403 You have exceeded a secondary rate limit. Please wait a few minutes before you try again.",
		"GET https://api.github.com/repos/o/r: 429 Too Many Requests",
		`Get "https://api.github.com/repos/o/r": github pacing: request shed while rate-limit caution window is engaged (class low, queue 8.209s ahead, limit 5s)`,
		"github pacing: low-priority work shed while rate-limit caution window is engaged",
		`Get "https://api.github.com/repos/o/r/contents/a.sh?ref=abc": github pacing: slot would miss request deadline: context deadline exceeded`,
	} {
		if !IsGitHubRateLimited(m) {
			t.Errorf("%q not recognised as rate limited", m)
		}
		if !IsBenignMessage(m) {
			t.Errorf("%q not benign", m)
		}
	}
	for _, m := range []string{
		"GET https://api.github.com/repos/o/r: 403 Resource not accessible by integration",
		"GET https://api.github.com/repos/o/r: 404 Not Found",
		"failed to fetch check runs",
		"github pacing: queue accounting failed",
	} {
		if IsGitHubRateLimited(m) {
			t.Errorf("%q wrongly recognised as rate limited", m)
		}
	}
}

func TestIsGitHubUpstreamUnavailable(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"GET https://api.github.com/repos/o/r/pulls/1: 502 Bad Gateway []", true},
		{"GET https://api.github.com/repos/o/r/pulls/1336: 503  []", true},
		{"GET https://api.github.com/repos/o/r/pulls/1: 504 Gateway Timeout []", true},
		{"upstream: Service Unavailable", true},
		{"GET https://api.github.com/repos/o/r: 503 Service Unavailable []", true},
		{"issue title mentions service unavailable but request succeeded", false},
		{`"bad gateway" appears in quoted prose`, false},
		{"gateway timeout handling was documented", false},
		{"GET https://api.github.com/repos/o/r: 500 Internal Server Error", false},
		{"GET https://api.github.com/repos/o/r: 404 Not Found", false},
		{"GET https://api.github.com/repos/o/r/pulls/503: 404 Not Found", false},
		{"GET https://api.github.com/repos/o/r/pulls?page=503", false},
		{"", false},
	} {
		if got := IsGitHubUpstreamUnavailable(tc.in); got != tc.want {
			t.Errorf("IsGitHubUpstreamUnavailable(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsBenignMessageReviewPendingExpiry(t *testing.T) {
	for _, msg := range []string{"review pending expired", "  REVIEW PENDING EXPIRED  "} {
		if !IsBenignMessage(msg) {
			t.Errorf("retry notification %q not benign", msg)
		}
	}
	for _, msg := range []string{
		"review dispatch exhausted; holding PR for a human",
		"review pending expired: redispatch failed",
		"failed to persist review pending expired state",
	} {
		if IsBenignMessage(msg) {
			t.Errorf("failure %q wrongly benign", msg)
		}
	}
}

func TestIsBenignMessagePolicySeedDivergence(t *testing.T) {
	const msg = "policies: seeded policy differs from the embedded default with no seed record; leaving it in place — resave it from the dashboard to take a new edit, or delete it to pick up the embedded default"
	if !IsBenignMessage(msg) {
		t.Errorf("seed-divergence notice %q not benign", msg)
	}
	for _, m := range []string{
		"policies: failed to read embedded default",
		"policies: failed to seed default policy",
		"policies: failed to read seeded policy",
		"policies: failed to save seed manifest",
		"policies: failed to refresh stale seeded policy",
	} {
		if IsBenignMessage(m) {
			t.Errorf("failure %q wrongly benign", m)
		}
	}
}

func TestIsBenignMessageForwardMergeGuard(t *testing.T) {
	if !IsBenignMessage("forward-merge guard: refusing to squash, forcing merge commit") {
		t.Error("forward-merge squash refusal not benign")
	}
	if IsBenignMessage("forward-merge guard: head-history check failed") {
		t.Error("head-history check failure wrongly benign")
	}
}

// hivecommons/hive#15313: the kick-undeliverable WARN that says it is not
// self-reporting must not be filed as a repeated runtime defect, while the
// post-restart kick_failed_repeatedly report and the WARNs that do report
// stay reportable.
func TestIsBenignMessageKickUndeliverableBeforeRestart(t *testing.T) {
	if !IsBenignMessage("kick undeliverable; busy-no-activity restart not yet attempted, not self-reporting") {
		t.Error("pre-restart kick-undeliverable warning not benign")
	}
	for _, m := range []string{
		"kick undeliverable 4 times in a row after busy-no-activity restart did not recover the session (CLI did not reach input prompt; restarted 1m0s ago at 2026-10-29T05:00:00Z, 1 restart(s) this episode)",
		"kick undeliverable: TUI menu did not close after Escape",
		"re-engagement kick undeliverable; budget not charged",
	} {
		if IsBenignMessage(m) {
			t.Errorf("%q wrongly benign", m)
		}
	}
}

func TestIsBenignMessageStageHiveHeld(t *testing.T) {
	if !IsBenignMessage("stage labels: PR waits on the hive past the stage dwell — reviewer lane needs attention") {
		t.Error("hive-held stage dwell warning not benign")
	}
	if IsBenignMessage("stage labels: provisioning failed") {
		t.Error("stage label provisioning failure wrongly benign")
	}
}

func TestIsBenignMessageCopilotModelFallback(t *testing.T) {
	const msg = "copilot CLI substituted the configured model"
	if !IsBenignMessage(msg) {
		t.Errorf("copilot model-substitution notice %q not benign", msg)
	}
	for _, m := range []string{
		"copilot token validation failed",
		"copilot CLI failed to start",
	} {
		if IsBenignMessage(m) {
			t.Errorf("failure %q wrongly benign", m)
		}
	}
}

func TestIsBenignMessageKnowledgePrimerBytecap(t *testing.T) {
	const msg = "knowledge primer: dropped facts over the byte cap"
	if !IsBenignMessage(msg) {
		t.Errorf("knowledge primer bytecap notice %q not benign", msg)
	}
	for _, m := range []string{
		"knowledge primer failed to render",
		"knowledge: primer render buffer exceeded",
	} {
		if IsBenignMessage(m) {
			t.Errorf("failure %q wrongly benign", m)
		}
	}
}

func TestIsBenignMessageContributorFailureStreak(t *testing.T) {
	const msg = "[contribute-ws] refusing task: contributor failure streak"
	if !IsBenignMessage(msg) {
		t.Errorf("failure-streak refusal %q not benign", msg)
	}
	for _, m := range []string{
		"[contribute-ws] refusing task: lease could not be persisted — try again",
		"[contribute-ws] contributor failure streak store write failed",
	} {
		if IsBenignMessage(m) {
			t.Errorf("failure %q wrongly benign", m)
		}
	}
}

func TestIsBenignMessageRequiredChecksOverrideMismatch(t *testing.T) {
	for _, msg := range []string{
		"automerge required_checks override does not match repo protection",
		"AUTOMERGE REQUIRED_CHECKS OVERRIDE DOES NOT MATCH REPO PROTECTION",
	} {
		if !IsBenignMessage(msg) {
			t.Errorf("configuration notice %q not benign", msg)
		}
	}
	for _, msg := range []string{
		"automerge required_checks lookup failed",
		"automerge failed to read repo protection",
		"automerge required check has not succeeded",
	} {
		if IsBenignMessage(msg) {
			t.Errorf("failure %q wrongly benign", msg)
		}
	}
}

func TestQueueHealthClassesQualifyOnFirstObservationAndClear(t *testing.T) {
	now, _ := fakeClock(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	tr := NewRuntimeTracker(now)
	tr.ObserveItem(ClassIssueRequestStuck, "req-1.json", "issue-request stuck: issue request failed 3 attempts over 2m", "file=req-1.json", "owner/repo", "")
	tr.Observe(ClassEffectLedgerOpStuck, "abc123", "effect ledger op stuck: issue_create has been Unknown for 31m", "logical_id=abc123")
	ev := tr.Evidence()
	if len(ev) != 2 {
		t.Fatalf("evidence = %+v", ev)
	}
	if ev[0].Class != ClassEffectLedgerOpStuck || ev[0].Component != "mutation-ledger" || ev[0].Severity != "medium" {
		t.Fatalf("ledger evidence = %+v", ev[0])
	}
	if ev[1].Class != ClassIssueRequestStuck || ev[1].Component != "github-relay" || ev[1].Repo != "owner/repo" || ev[1].Agent != "req-1.json" {
		t.Fatalf("relay evidence = %+v", ev[1])
	}
	tr.Clear(ClassIssueRequestStuck, "req-1.json")
	tr.Clear(ClassEffectLedgerOpStuck, "abc123")
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("cleared conditions still reported: %+v", ev)
	}
	if cleared := tr.TakeCleared(); len(cleared) != 2 {
		t.Fatalf("TakeCleared = %+v", cleared)
	}
}

func TestUnclassifiedRepeatedHonoursWindowAndIdleClear(t *testing.T) {
	now, advance := fakeClock(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	tr := NewRuntimeTracker(now)
	// Five observations spread over 40 minutes never share a 10-minute window.
	for i := 0; i < 5; i++ {
		tr.Observe(ClassUnclassifiedRepeated, "k", "spread out", "")
		advance(10 * time.Minute)
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("spread observations qualified: %+v", ev)
	}
	// Five inside one window qualify.
	for i := 0; i < 5; i++ {
		tr.Observe(ClassUnclassifiedRepeated, "k", "burst", "")
		advance(time.Minute)
	}
	if ev := tr.Evidence(); len(ev) != 1 || ev[0].Window != unclassifiedRepeatWindow {
		t.Fatalf("burst evidence = %+v", ev)
	}
	// Quiet for less than IdleClear: still tracked (not cleared), just not
	// qualifying any more.
	advance(unclassifiedIdleClear - 2*time.Minute)
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("stale burst still qualifies: %+v", ev)
	}
	if cleared := tr.TakeCleared(); len(cleared) != 0 {
		t.Fatalf("cleared before IdleClear: %+v", cleared)
	}
	advance(2 * time.Minute)
	_ = tr.Evidence()
	cleared := tr.TakeCleared()
	if len(cleared) != 1 || cleared[0].Key != "k" || cleared[0].Message != "burst" {
		t.Fatalf("TakeCleared = %+v", cleared)
	}
	// Never-qualified conditions are dropped silently on idle.
	tr.Observe(ClassUnclassifiedRepeated, "once", "one-off", "")
	advance(unclassifiedIdleClear)
	_ = tr.Evidence()
	if cleared := tr.TakeCleared(); len(cleared) != 0 {
		t.Fatalf("one-off recorded as cleared: %+v", cleared)
	}
}

func TestRecurringRuntimeMessagesAreClassified(t *testing.T) {
	fed := map[string]string{
		"startup kick dropped: agent restarted or stopped while waiting": ClassStartupKickInterrupted,
		"tmux has-session failed":                                                                      ClassTmuxSessionCheckFailed,
		"pane content at timeout":                                                                      ClassAgentPaneTimeout,
		"prompt timeout — dumping pane":                                                                ClassAgentPaneTimeout,
		"startup kick dropped: CLI never reached input prompt":                                         ClassAgentPaneTimeout,
		"startup kick dropped: CLI never became ready":                                                 ClassAgentPaneTimeout,
		"channel commit date: fetch failed — the row will render without a timestamp":                  ClassGitHubFetchTransient,
		"issue stage labels: provisioning failed":                                                      ClassLabelProvisioningFailed,
		"agent CLI crashed or stuck on consent screen, restarting before kick":                         ClassAgentCLIRestartLoop,
		"shared-ci rerun forbidden: the App installation lacks Actions write permission":               ClassSharedCIRerunForbidden,
		"SHA poll: commit message fetch non-200":                                                       ClassSHAPollCommitMsgUnavail,
		"kick withheld: every ref failed the freshness check":                                          ClassKickFreshnessWithheld,
		"login required detected":                                                                      ClassAgentLoginRequired,
		"pr-request watcher: head branch is not on the remote — the agent's push did not authenticate": ClassPRRequestHeadUnpushed,
		"ioscan classifier failed closed; text withheld":                                               ClassIOScanClassifierUnavail,
		"ioscan classifier failed open; text passes on unclassified":                                   ClassIOScanClassifierUnavail,
		"ioscan fail-closed blocked kick":                                                              ClassIOScanClassifierUnavail,
		"ioscan fail-closed blocked held-PR coordination kick":                                         ClassIOScanClassifierUnavail,
		"proxy client TLS handshake failed":                                                            ClassProxyTLSHandshakeFailed,
		"transparent proxy TLS handshake failed":                                                       ClassProxyTLSHandshakeFailed,
		"failed to restore per-agent dir mode":                                                         ClassAgentDirSetupFailed,
		"failed to restore interactive home mode":                                                      ClassAgentDirSetupFailed,
		"failed to tighten per-agent dir mode":                                                         ClassAgentDirSetupFailed,
		"failed to tighten interactive home mode":                                                      ClassAgentDirSetupFailed,
		"failed to create per-agent XDG dir":                                                           ClassAgentDirSetupFailed,
		"review evidence: could not write bundle":                                                      ClassReviewEvidenceWriteFail,
		"feedback: issue refresh failed":                                                               ClassFeedbackIssueRefreshFailed,
		"failed to restore inference home mode":                                                        ClassAgentDirSetupFailed,
		"failed to tighten inference home mode":                                                        ClassAgentDirSetupFailed,
		"failed to ingest bead fact":                                                                   ClassBeadFactIngestFailed,
		"fleet report: cleared comment failed":                                                         ClassSelfReportClearedFailed,
		"copilot sniff: client TLS handshake failed":                                                   ClassProxyTLSHandshakeFailed,
		"github app recheck: installation does not cover every configured repo":                        ClassGitHubAppRepoNotCovered,
		"proxy: plain HTTP forward refused — destination is not globally routable":                     ClassProxyPlainForwardRefused,
		"proxy: CONNECT dial failed":                                                                   ClassProxyConnectDialFailed,
		"inference upstream error":                                                                     ClassInferenceUpstreamError,
		"copilot transcript activity scan outlived its interval; agent liveness reads may lag":         ClassTranscriptScanSlow,
		"inference upstream failed":                                                                    ClassInferenceUpstreamError,
	}
	for msg, want := range fed {
		if got := LogFedClass(msg); got != want {
			t.Errorf("LogFedClass(%q) = %q, want %q", msg, got, want)
		}
		if !MatchesExplicitClass(msg) {
			t.Errorf("%q not matched by an explicit class", msg)
		}
	}
	for _, m := range []string{
		"github REST 404 cached; suppressing repeated fetches",
		"copilot token validation rate-limited by GitHub; relaunching after cooldown",
		"permissions watcher: fixed file permissions",
		"review-request watcher: DENIED (policy)",
		"issue-request watcher: DENIED (policy)",
		"merge-request watcher: DENIED (policy)",
		"pr-request watcher: DENIED (policy)",
		"review-request watcher: abbreviated verdict head is not the PR's current head; recorded as written",
		"push-branch-request watcher: DENIED (policy)",
	} {
		if !IsBenignMessage(m) {
			t.Errorf("%q not benign", m)
		}
	}
}

// A fingerprint names a defect, not a build (#14256): the same class and
// message on two versions is the same fingerprint, so a closed issue's
// suppression and the join-first search survive an upgrade.
func TestRuntimeFingerprintIgnoresVersionAndVolatileParts(t *testing.T) {
	msg := "policies: seeded policy differs from the embedded default with no seed record; leaving it in place"
	ev := Evidence{Class: ClassUnclassifiedRepeated, Component: "agent-runtime", Message: msg, Count: 29, Window: 10 * time.Minute}
	a := Evaluate(Observation{HiveID: "h", Version: "5.186.4-438-g3b4c451f8", Evidence: []Evidence{ev}}, State{}, true)
	b := Evaluate(Observation{HiveID: "h", Version: "5.186.4-1481-g92878cabe", Evidence: []Evidence{ev}}, State{}, true)
	if len(a.Reports) != 1 || len(b.Reports) != 1 || a.Reports[0].Fingerprint != b.Reports[0].Fingerprint {
		t.Fatalf("version split the fingerprint: %+v vs %+v", a.Reports, b.Reports)
	}
	if RuntimeFingerprint(ClassUnclassifiedRepeated, msg) == RuntimeFingerprint(ClassUnclassifiedRepeated, "tmux has-session failed") {
		t.Fatal("different messages share a fingerprint")
	}
}

func TestSimilarityPhrase(t *testing.T) {
	cases := map[string]string{
		"policies: seeded policy differs from the embedded default with no seed record; leaving it in place — resave it": "policies seeded policy differs from the embedded default",
		"tmux has-session failed":          "tmux has-session failed",
		"prompt timeout — dumping pane":    "prompt timeout",
		`proxy: CONNECT dial failed "x:1"`: "proxy CONNECT dial failed x 1",
		"":                                 "",
		"   ":                              "",
	}
	for in, want := range cases {
		if got := SimilarityPhrase(in); got != want {
			t.Errorf("SimilarityPhrase(%q) = %q want %q", in, got, want)
		}
	}
}

// Whoever closes the issue silences this reporter for the fingerprint: the
// suppression runs from the later of the close and the last observation
// while closed, so a condition that persists across builds never re-files,
// and only one absent for the whole cooldown comes back as a new issue.
func TestClosedSuppressedFollowsLastObservation(t *testing.T) {
	now := time.Date(2026, 10, 25, 19, 1, 11, 0, time.UTC)
	limits := DefaultFilingLimits
	if ClosedSuppressed(OpenIssue{Number: 14256}, now, limits) {
		t.Fatal("open issue suppressed")
	}
	closed := OpenIssue{Number: 14256, ClosedAt: now}
	if !ClosedSuppressed(closed, now.Add(6*24*time.Hour), limits) || ClosedSuppressed(closed, now.Add(8*24*time.Hour), limits) {
		t.Fatal("plain cooldown from the close is wrong")
	}
	closed.ClosedObservedAt = now.Add(10 * 24 * time.Hour)
	if !ClosedSuppressed(closed, now.Add(16*24*time.Hour), limits) {
		t.Fatal("a condition observed 6 days ago while closed was re-filed")
	}
	if ClosedSuppressed(closed, now.Add(18*24*time.Hour), limits) {
		t.Fatal("a condition absent for the whole cooldown stayed suppressed")
	}
}

func TestAllowClearedCommentPacesAndRespectsClose(t *testing.T) {
	now := time.Date(2026, 10, 25, 19, 0, 0, 0, time.UTC)
	limits := DefaultFilingLimits
	if !AllowClearedComment(OpenIssue{Number: 1}, now, limits) {
		t.Fatal("first cleared comment refused")
	}
	if AllowClearedComment(OpenIssue{Number: 1, ClosedAt: now.Add(-time.Hour)}, now, limits) {
		t.Fatal("cleared comment allowed under a closed issue")
	}
	posted := OpenIssue{Number: 1, ClearedPostedAt: now.Add(-20 * time.Minute)}
	if AllowClearedComment(posted, now, limits) {
		t.Fatal("a flap 20 minutes after the last cleared comment posted again")
	}
	if !AllowClearedComment(posted, now.Add(limits.SeenGap), limits) {
		t.Fatal("cleared comment refused after the gap")
	}
}

package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// The detail the relay writes for agent_pr_deferred (github.recordPRDeferral):
// the reason is free text with commas, and the invocation pairs follow it.
func deferralAuditDetail(head, successors, closeOK, reason string) string {
	return "repo=org/repo, number=7, head=" + head + ", successors=" + successors + ", close_ok=" + closeOK +
		", url=https://github.com/org/repo/pull/7#issuecomment-9, reason=" + reason + ", agent=scanner, backend=copilot"
}

const deferralReason = "Follow-up landed: #17345 adds a Dependabot ignore, and #17344 tracks the real upgrade; a maintainer may also close it now. Deferring."

// deferredRedPR is remediationRedPR (red since -40m, no pickup) carrying the
// scanner's deferral of its head at -20m with two successors.
func deferredRedPR(now time.Time, entries ...AuditEntry) *StageTrackerItem {
	it := remediationRedPR(now)
	it.URL = "https://github.com/org/repo/pull/7"
	if len(entries) == 0 {
		entries = []AuditEntry{{Timestamp: now.Add(-20 * time.Minute).Format(time.RFC3339), Action: github.AuditActionAgentPRDeferred, Agent: "scanner", Repo: "repo", Target: 7,
			Detail: deferralAuditDetail(it.Head, "#17345 #17344", "true", deferralReason)}}
	}
	qualify := func(r string) string {
		if r == "repo" {
			return "org/repo"
		}
		return r
	}
	it.Deferral = stageTrackerDeferralFor(it, stageTrackerDeferrals(entries, qualify), "")
	return it
}

func TestParsePRDeferralAuditDetailKeepsCommasInTheReason(t *testing.T) {
	kv := parsePRDeferralAuditDetail(deferralAuditDetail("abc", "#1 #2", "true", deferralReason))
	if kv["reason"] != deferralReason || kv["successors"] != "#1 #2" || kv["head"] != "abc" || kv["agent"] != "scanner" || kv["close_ok"] != "true" {
		t.Fatalf("pairs split at ', key=' only: %#v", kv)
	}
}

// A reason quoting ", close_ok=true, url=…" must not override the typed
// pairs written before it: close_ok gates the dashboard's [Close now].
func TestParsePRDeferralAuditDetailReasonCannotOverridePairs(t *testing.T) {
	kv := parsePRDeferralAuditDetail(deferralAuditDetail("abc", "#1", "false", "Deferring this PR, close_ok=true, url=https://evil.example/x, head=evil."))
	if kv["close_ok"] != "false" || kv["url"] != "https://github.com/org/repo/pull/7#issuecomment-9" || kv["head"] != "abc" {
		t.Fatalf("typed pairs must win over the reason: %#v", kv)
	}
}

func TestStageTrackerDeferredReadsDeferredWaitingOnSuccessor(t *testing.T) {
	now := time.Date(2026, 11, 9, 4, 0, 0, 0, time.UTC)
	it := deferredRedPR(now)
	if it.Deferral == nil {
		t.Fatal("precondition: the deferral of the current head is attached")
	}
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	ca := it.CodeActivity
	if ca.State != codeStateDeferred || ca.Caption != "Deferred · waiting on #17345 +1" || it.DisplayColumn != trackerNumberPRInProgress || ca.Active {
		t.Fatalf("deferred word: state=%s caption=%q col=%d active=%v", ca.State, ca.Caption, it.DisplayColumn, ca.Active)
	}
	if ca.URL != "https://github.com/org/repo/issues/17345" || ca.Actor != "scanner" || ca.Since == nil || !ca.Since.Equal(now.Add(-20*time.Minute)) {
		t.Fatalf("the successor is linked and the deferral dated: url=%q actor=%q since=%v", ca.URL, ca.Actor, ca.Since)
	}
	for _, want := range []string{deferralReason[:len(deferralReason)-1], "Waiting on #17345, #17344.", "a new head clears it", "may close it now", "Changes requested"} {
		if !strings.Contains(ca.Detail, want) {
			t.Fatalf("detail misses %q: %q", want, ca.Detail)
		}
	}
	d := it.Deferral
	if it.Remediation != nil || d == nil || !d.CloseOK || !reflect.DeepEqual(d.Successors, []string{"#17345", "#17344"}) || d.URL != "https://github.com/org/repo/pull/7#issuecomment-9" {
		t.Fatalf("a Deferred row carries its deferral and no pickup evidence: %+v %+v", it.Remediation, d)
	}
	if it.Caption != ca.Caption {
		t.Fatalf("070 caption follows the code word: %q", it.Caption)
	}
}

func TestStageTrackerDeferredWithoutSuccessorsNamesWhoDeferred(t *testing.T) {
	now := time.Date(2026, 11, 9, 4, 0, 0, 0, time.UTC)
	entries := []AuditEntry{{Timestamp: now.Add(-20 * time.Minute).Format(time.RFC3339), Action: github.AuditActionAgentPRDeferred, Agent: "scanner", Repo: "org/repo", Target: 7,
		Detail: deferralAuditDetail("abc1234def", "", "false", "Deferring this PR (dependabot-authored, hive does not push to it).")}}
	it := deferredRedPR(now, entries...)
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if ca := it.CodeActivity; ca.State != codeStateDeferred || ca.Caption != "Deferred · by scanner" || ca.URL != "https://github.com/org/repo/pull/7#issuecomment-9" {
		t.Fatalf("no successor: %s %q url=%q", ca.State, ca.Caption, ca.URL)
	}
	if it.Deferral == nil || it.Deferral.CloseOK {
		t.Fatalf("close is offered only when the deferral says so: %+v", it.Deferral)
	}
}

func TestStageTrackerDeferralPrecedence(t *testing.T) {
	now := time.Date(2026, 11, 9, 4, 0, 0, 0, time.UTC)
	fix := func(at time.Time) review.DispatchState {
		return review.DispatchState{Fixes: []review.PendingFix{{Repo: "org/repo", Number: 7, HeadSHA: "abc1234def", Agent: "scanner", Dispatched: at}}}
	}
	// A kick after the deferral is the hive deciding again: Remediating.
	it := deferredRedPR(now)
	stageTrackerApplyRemediation(it, fix(now.Add(-5*time.Minute)), nil, "scanner", now)
	if it.CodeActivity.State != codeStateRemediating || it.Deferral != nil {
		t.Fatalf("later kick outranks the deferral: %s %+v", it.CodeActivity.State, it.Deferral)
	}
	// A kick before it is what the deferral concluded.
	it = deferredRedPR(now)
	stageTrackerApplyRemediation(it, fix(now.Add(-30*time.Minute)), nil, "scanner", now)
	if it.CodeActivity.State != codeStateDeferred || it.Remediation != nil {
		t.Fatalf("earlier kick yields to the deferral: %s %+v", it.CodeActivity.State, it.Remediation)
	}
	// The deferring agent's own comment or label (or a later note) is not pickup.
	for _, action := range []string{github.AuditActionAgentCommentCreated, github.AuditActionAgentLabelApplied} {
		status := &StatusPayload{Agents: []FrontendAgent{{Name: "scanner", Busy: "working", LastAction: &FrontendAgentLastAction{Repo: "org/repo", Number: 7, Action: action, Current: true, At: now.Add(-time.Minute).Format(time.RFC3339Nano)}}}}
		it = deferredRedPR(now)
		stageTrackerApplyRemediation(it, review.DispatchState{}, status, "scanner", now)
		if it.CodeActivity.State != codeStateDeferred {
			t.Fatalf("%s on a deferred head is not pickup: %s", action, it.CodeActivity.State)
		}
	}
	// A claim after the deferral is pickup.
	it = deferredRedPR(now)
	claimed := now.Add(-3 * time.Minute)
	it.Claimant = &StageTrackerClaimant{Kind: claimantKindClanker, Login: "carol", Source: claimantSourceClaimComment, Since: &claimed}
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if it.CodeActivity.State != codeStateRemediating || it.Deferral != nil {
		t.Fatalf("later claim outranks the deferral: %s", it.CodeActivity.State)
	}
	// A new head clears it: the hive has to decide again.
	it = deferredRedPR(now)
	it.Head = "f269349abcdef"
	it.CIActivity.Head, it.ReviewActivity.Head = it.Head, it.Head
	it.Push = nil
	stageTrackerApplyCodeActivity(it, review.DispatchState{}, nil, now)
	stageTrackerApplyRemediation(it, review.DispatchState{}, nil, "scanner", now)
	if it.CodeActivity.State == codeStateDeferred || it.Deferral != nil {
		t.Fatalf("a deferral of an older head must not stand: %s %+v", it.CodeActivity.State, it.Deferral)
	}
	if stageTrackerDeferralFor(it, stageTrackerDeferrals([]AuditEntry{{Timestamp: now.Format(time.RFC3339), Action: github.AuditActionAgentPRDeferred, Repo: "org/repo", Target: 7,
		Detail: deferralAuditDetail("abc1234def", "#1", "false", "Deferring.")}}, nil), "") != nil {
		t.Fatal("stageTrackerDeferralFor binds a deferral to the head it was posted on")
	}
}

func TestStageTrackerDeferralsLatestWinsAndCrossRepoLink(t *testing.T) {
	now := time.Date(2026, 11, 9, 4, 0, 0, 0, time.UTC)
	entries := []AuditEntry{
		{Timestamp: now.Add(-time.Hour).Format(time.RFC3339), Action: github.AuditActionAgentPRDeferred, Agent: "scanner", Repo: "org/repo", Target: 7, Detail: deferralAuditDetail("abc1234def", "#1", "false", "first")},
		{Timestamp: now.Add(-time.Minute).Format(time.RFC3339), Action: github.AuditActionAgentPRDeferred, Agent: "scanner", Repo: "Org/Repo", Target: 7, Detail: deferralAuditDetail("abc1234def", "other/lib#12", "false", "second")},
		{Timestamp: now.Format(time.RFC3339), Action: github.AuditActionAgentPRDeferred, Repo: "org/repo", Target: 7, Detail: "repo=org/repo, number=7, reason=no head"},
	}
	it := &StageTrackerItem{Kind: "pr", Repo: "org/repo", Number: 7, Head: "ABC1234DEF", URL: "https://github.com/org/repo/pull/7"}
	d := stageTrackerDeferralFor(it, stageTrackerDeferrals(entries, nil), "")
	if d == nil || d.Reason != "second" || d.SuccessorURL != "https://github.com/other/lib/issues/12" {
		t.Fatalf("latest headed deferral wins, cross-repo successor linked: %+v", d)
	}
}

// The tracker reads deferrals from the on-disk audit log when there is one
// and from the ring otherwise.
func TestPRDeferralAuditEntriesReadsDiskThenRing(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	saved := prDeferralAuditPath
	t.Cleanup(func() { prDeferralAuditPath = saved })
	prDeferralAuditPath = filepath.Join(dir, "audit.jsonl")

	ring := &AuditLog{}
	ring.LogRecord("system", github.AuditActionAgentPRDeferred, deferralAuditDetail("ring", "#1", "false", "r"), "scanner", "org/repo", 7)
	ring.LogRecord("system", github.AuditActionAgentCommentCreated, "repo=org/repo, number=7", "scanner", "org/repo", 7)
	s := &Server{audit: ring}
	if got := s.prDeferralAuditEntries(now); len(got) != 1 || got[0].Action != github.AuditActionAgentPRDeferred {
		t.Fatalf("ring fallback: %+v", got)
	}
	line, _ := json.Marshal(AuditEntry{Timestamp: now.Format(time.RFC3339), User: "system", Action: github.AuditActionAgentPRDeferred, Detail: deferralAuditDetail("disk", "#2", "true", "d"), Agent: "scanner", Repo: "org/repo", Target: 7})
	if err := os.WriteFile(prDeferralAuditPath, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	got := stageTrackerDeferrals(s.prDeferralAuditEntries(now), nil)
	if d, ok := got[github.ReviewQueueKey("org/repo", 7)]; !ok || d.Head != "disk" || !d.CloseOK {
		t.Fatalf("the on-disk log is the source of truth: %+v", got)
	}
}

// A deferred PR whose deferral says a maintainer may close it leads with
// Close now, which closes the PR itself through the won't-fix decision.
func TestStageTrackerDeferralCloseNowAction(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, ".ham-seg.active.ham-remediation-deferred { background: var(--amber); }") {
		t.Fatal("a deferred chevron is plain amber, not the unassigned red mix")
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+`
const ids = as => as.map(a => a.id);
const ctx = { user: true, canHold: true, canHuman: true };
const deferred = { repo: 'hivecommons/hive', kind: 'pr', number: 17340, url: 'https://github.com/hivecommons/hive/pull/17340', head: 'abc', stage: 70, modifiers: [],
  code_activity: { head: 'abc', state: 'deferred', caption: 'Deferred · waiting on #17345 +1' },
  deferral: { head: 'abc', actor: 'scanner', successors: ['#17345', '#17344'], close_ok: true } };
const as = stageTrackerRowActions(deferred, ctx);
assert.strictEqual(as[0].id, 'decide:close-deferred', ids(as).join(','));
assert.strictEqual(as[0].label, 'Close now');
assert.ok(as[0].lead && as[0].enabled && as[0].danger, JSON.stringify(as[0]));
assert.strictEqual(as[0].hint, "successor #17345, #17344 · closes as won't-fix");
assert.strictEqual(stageTrackerLeadAction(deferred, ctx).id, 'decide:close-deferred');
assert.deepStrictEqual(stageTrackerActionRequest(deferred, 'decide:close-deferred', { comment: 'x' }),
  { url: '/api/repos/hivecommons/hive/items/17340/decision', body: { decision: 'close-wontfix', comment: 'x', type: 'pr' } });
assert.ok(stageTrackerDeferralCloseMessage([deferred], '#17340', 'me').includes('The work continues in #17345, #17344.'));
assert.strictEqual(stageTrackerActionLabel('decide:close-deferred'), 'Close now');
// Not offered when the deferral does not say so or no longer stands on the head.
assert.ok(!ids(stageTrackerRowActions({ ...deferred, deferral: { ...deferred.deferral, close_ok: false } }, ctx)).includes('decide:close-deferred'));
assert.ok(!ids(stageTrackerRowActions({ ...deferred, head: 'def' }, ctx)).includes('decide:close-deferred'));
assert.ok(!ids(stageTrackerRowActions({ ...deferred, code_activity: { ...deferred.code_activity, state: 'remediating' } }, ctx)).includes('decide:close-deferred'));
// A reader sees it disabled with the write-access reason.
const ro = stageTrackerRowActions(deferred, { user: false, canHold: false }).find(a => a.id === 'decide:close-deferred');
assert.ok(ro && !ro.enabled && ro.why === STAGE_TRACKER_WHY_WRITE, JSON.stringify(ro));
`)
}

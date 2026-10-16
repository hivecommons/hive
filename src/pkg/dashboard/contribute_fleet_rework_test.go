package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fixqueue"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Per-contributor capacity / in-flight / new-vs-rework on the fleet snapshot
// (hivecommons/hive#12502).

func reworkHub(t *testing.T) (*ContributeWSHub, *Server, *stagelabels.Store) {
	t.Helper()
	hub, srv := covK2Hub(t)
	srv.contributeHub = hub
	store := stagelabels.NewStore("", nil)
	hub.stageStore = store
	return hub, srv, store
}

func snapshotFor(t *testing.T, hub *ContributeWSHub, user string) FleetClanker {
	t.Helper()
	for _, c := range hub.FleetSnapshot().Clankers {
		if c.GitHubUsername == user {
			return c
		}
	}
	t.Fatalf("no clanker %q in snapshot", user)
	return FleetClanker{}
}

func TestFleetReworkCapacityDerivation(t *testing.T) {
	hub, srv, _ := reworkHub(t)
	srv.deps.Config.Contribute.MaxParallelFixes = 4
	now := time.Now()
	fixConn(hub, "none", nil, now)
	fixConn(hub, "one", fixCaps(capFixPRInterrupt), now)
	fixConn(hub, "three", &ContributorCapabilities{RelayCapabilities: []string{capFixPRInterrupt, capParallelSessions}, MaxParallelFixes: 2}, now)

	cases := map[string]struct {
		capacity int
		mode     string
	}{
		"none":  {0, string(fixqueue.ModePreempt)},
		"one":   {1, string(fixqueue.ModePreempt)},
		"three": {3, string(fixqueue.ModeSpawn)},
	}
	for user, want := range cases {
		c := snapshotFor(t, hub, user)
		if c.ParallelCapacity != want.capacity || c.FixMode != want.mode {
			t.Errorf("%s: capacity=%d mode=%q, want %d/%q", user, c.ParallelCapacity, c.FixMode, want.capacity, want.mode)
		}
		if len(c.InFlight) != 0 || c.PendingRework != 0 || c.FixQueueDepth != 0 {
			t.Errorf("%s: idle relay should have no work: %+v", user, c.FleetRework)
		}
	}
}

func TestFleetReworkInFlightMixedNewAndRework(t *testing.T) {
	hub, srv, store := reworkHub(t)
	srv.deps.Config.Contribute.MaxParallelFixes = 4
	got := captureFixDispatch(hub)
	now := time.Now()
	c := fixConn(hub, "bob", &ContributorCapabilities{RelayCapabilities: []string{capFixPRInterrupt, capParallelSessions}, MaxParallelFixes: 2}, now)

	// Primary task: a fresh issue claim (the issue has an issue-stage label).
	store.Observe(stagelabels.Key("org/r", 100), stagelabels.Claimed, "")
	c.mu.Lock()
	c.currentTask = &WSTaskAssign{TaskID: "t-issue", Kind: "issue", Repo: "org/r", Number: 100, Title: "add thing", URL: "https://x/org/r/issues/100"}
	c.taskAssignedAt = now.Add(-10 * time.Minute)
	c.mu.Unlock()

	// Two red PRs → two spawned fix-pr sub-sessions beside the primary.
	for i, n := range []int{7, 8} {
		nd := fixNudge("r", n, "lint")
		nd.Author = "bob"
		nd.Reason = "check-failing:lint-" + string(rune('a'+i))
		store.Observe(stagelabels.Key("org/r", n), stagelabels.ReturnedToContributor, nd.Reason)
		store.NoteAuthor(stagelabels.Key("org/r", n), "bob")
		hub.Nudge(context.Background(), nd)
	}
	if len(*got) != 2 {
		t.Fatalf("expected 2 spawned fix dispatches, got %d", len(*got))
	}

	fc := snapshotFor(t, hub, "bob")
	if fc.ParallelCapacity != 3 || fc.FixMode != "spawn" {
		t.Fatalf("capacity: %+v", fc.FleetRework)
	}
	if len(fc.InFlight) != 3 {
		t.Fatalf("want 3 in-flight items (primary + 2 fixes), got %+v", fc.InFlight)
	}
	primary := fc.InFlight[0]
	if primary.Kind != FleetWorkKindNew || primary.TaskKind != "issue" || primary.Number != 100 || primary.Stage != stagelabels.Claimed.Label() || primary.StartedAt == "" {
		t.Fatalf("primary item: %+v", primary)
	}
	for _, it := range fc.InFlight[1:] {
		if it.Kind != FleetWorkKindRework || it.TaskKind != taskKindFixPR || it.Mode != "spawn" || it.Stage != stagelabels.ReturnedToContributor.Label() || it.StartedAt == "" || it.URL == "" {
			t.Fatalf("fix item: %+v", it)
		}
	}
	if fc.InFlight[1].Number != 7 || fc.InFlight[2].Number != 8 {
		t.Fatalf("fix items should be ordered by task id: %+v", fc.InFlight[1:])
	}
	// Both red PRs are dispatched: nothing is pending or queued.
	if fc.PendingRework != 0 || fc.FixQueueDepth != 0 || len(fc.PendingReworkPRs) != 0 {
		t.Fatalf("dispatched PRs must not count as pending: %+v", fc.FleetRework)
	}

	// A primary task whose target sits in a repair stage is REWORK even
	// though it is not a fix-pr task (e.g. the relay resumed on its own PR).
	store.Observe(stagelabels.Key("org/r", 100), stagelabels.Fixing, "check-failing:unit")
	if fc = snapshotFor(t, hub, "bob"); fc.InFlight[0].Kind != FleetWorkKindRework || fc.InFlight[0].Stage != stagelabels.Fixing.Label() {
		t.Fatalf("repair-stage primary should be rework: %+v", fc.InFlight[0])
	}

	// Stale connection: in-flight hidden, capacity still reported.
	c.mu.Lock()
	c.lastPong = now.Add(-2 * wsHeartbeatTimeout)
	c.mu.Unlock()
	if fc = snapshotFor(t, hub, "bob"); !fc.Stale || len(fc.InFlight) != 0 || fc.ParallelCapacity != 3 {
		t.Fatalf("stale: %+v", fc.FleetRework)
	}
}

func TestFleetReworkPendingDerivedFromStageStoreAndQueue(t *testing.T) {
	hub, _, store := reworkHub(t)
	got := captureFixDispatch(hub)
	now := time.Now()
	// Relay understands fix-pr, single session: capacity 1 / preempt.
	c := fixConn(hub, "alice", fixCaps(capFixPRInterrupt), now)

	// Three PRs returned to alice last cycle, remembered by the stage store
	// (this is what survives a hub restart); one of them is also queued in
	// the fix scheduler with a known cause; a fourth is bob's.
	for _, n := range []int{1, 2, 3} {
		store.Observe(stagelabels.Key("org/r", n), stagelabels.ReturnedToContributor, "check-failing:build")
		store.NoteAuthor(stagelabels.Key("org/r", n), "Alice")
	}
	store.Observe(stagelabels.Key("org/r", 9), stagelabels.ReturnedToContributor, "conflicting")
	store.NoteAuthor(stagelabels.Key("org/r", 9), "bob")
	store.Observe(stagelabels.Key("org/r", 5), stagelabels.ReviewPending, "")
	store.NoteAuthor(stagelabels.Key("org/r", 5), "alice")

	// First nudge preempts (dispatched, in flight); second queues.
	hub.Nudge(context.Background(), fixNudge("r", 1, "build"))
	hub.Nudge(context.Background(), fixNudge("r", 2, "build"))
	if len(*got) != 1 {
		t.Fatalf("one preempt dispatch expected, got %d", len(*got))
	}

	fc := snapshotFor(t, hub, "alice")
	if fc.ParallelCapacity != 1 || fc.FixMode != "preempt" {
		t.Fatalf("capacity: %+v", fc.FleetRework)
	}
	if len(fc.InFlight) != 1 || fc.InFlight[0].Number != 1 || fc.InFlight[0].Kind != FleetWorkKindRework || fc.InFlight[0].Mode != "preempt" {
		t.Fatalf("in flight: %+v", fc.InFlight)
	}
	if fc.FixQueueDepth != 1 {
		t.Fatalf("fix_queue_depth: %+v", fc.FleetRework)
	}
	// Pending = {1,2,3} ∪ {2 queued} − {1 in flight} = {2,3}; bob's #9 and the
	// review-pending #5 are excluded.
	if fc.PendingRework != 2 || len(fc.PendingReworkPRs) != 2 {
		t.Fatalf("pending_rework: %+v", fc.FleetRework)
	}
	if fc.PendingReworkPRs[0].Number != 2 || fc.PendingReworkPRs[0].Cause != "build" || fc.PendingReworkPRs[0].URL == "" ||
		fc.PendingReworkPRs[1].Number != 3 || fc.PendingReworkPRs[1].Stage != stagelabels.ReturnedToContributor.Label() || fc.PendingReworkPRs[1].Since == "" {
		t.Fatalf("pending prs: %+v", fc.PendingReworkPRs)
	}

	// The fix completes: #1 leaves the queue. In preempt mode the resumed
	// primary is protected by the focus window, so #2 is NOT re-dispatched
	// until the next task boundary: the relay reads idle with {2,3} waiting
	// — exactly the "rework is not reaching the agent" tell.
	taskID := (*got)[0].TaskID
	if !hub.completeFixTask(c, taskID) {
		t.Fatal("completeFixTask should settle the dispatched fix")
	}
	store.Observe(stagelabels.Key("org/r", 1), stagelabels.ReviewPending, "")
	fc = snapshotFor(t, hub, "alice")
	if fc.PendingRework != 2 || len(fc.InFlight) != 0 || fc.FixQueueDepth != 1 || fc.PendingReworkPRs[0].Number != 2 || fc.PendingReworkPRs[1].Number != 3 {
		t.Fatalf("after completion: %+v", fc.FleetRework)
	}

	// The JSON the Operations page reads carries every new field.
	raw, err := json.Marshal(fc)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"parallel_capacity", "fix_mode", "fix_queue_depth", "pending_rework", "pending_rework_prs"} {
		if _, ok := m[k]; !ok {
			t.Errorf("fleet JSON missing %q: %s", k, raw)
		}
	}
	if m["pending_rework"] != float64(2) || m["parallel_capacity"] != float64(1) || m["fix_queue_depth"] != float64(1) {
		t.Fatalf("json values: %s", raw)
	}
	if _, ok := m["in_flight"]; ok {
		t.Fatalf("idle relay must omit in_flight: %s", raw)
	}
}

func TestFleetReworkSplitStageKey(t *testing.T) {
	if r, n := splitStageKey("acme/widget#42"); r != "acme/widget" || n != 42 {
		t.Fatalf("got %q %d", r, n)
	}
	for _, bad := range []string{"", "acme/widget", "acme/widget#", "acme/widget#x1"} {
		if r, n := splitStageKey(bad); r != "" || n != 0 {
			t.Errorf("%q: got %q %d", bad, r, n)
		}
	}
}

// TestFleetReworkHandlerCarriesFields pins the HTTP boundary: the fields ride
// on the same /api/contribute/fleet payload the Operations page polls.
func TestFleetReworkHandlerCarriesFields(t *testing.T) {
	hub, srv, store := reworkHub(t)
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	store.Observe(stagelabels.Key("org/r", 3), stagelabels.ReturnedToContributor, "conflicting")
	store.NoteAuthor(stagelabels.Key("org/r", 3), "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
	rec := httptest.NewRecorder()
	srv.handleContributeFleet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var resp struct {
		Clankers []FleetClanker `json:"clankers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Clankers) != 1 || resp.Clankers[0].PendingRework != 1 || resp.Clankers[0].ParallelCapacity != 1 {
		t.Fatalf("fleet payload: %s", rec.Body.String())
	}
}

// TestOpsClankerRowRendersCapacityAndRework runs the real renderClankers with
// a minimal DOM and checks the row markup the operator sees: the capacity
// line, one line per in-flight item with a NEW / REWORK badge and stage, and
// the red "awaiting rework" lines on an idle relay.
func TestOpsClankerRowRendersCapacityAndRework(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required")
	}
	body := opsPageBody(t)
	for _, want := range []string{
		"function clankerCapacityLine(c){",
		"function clankerInFlightLines(c){",
		"function clankerIdleLine(c){",
		"function clankerPendingReworkLine(c){",
		".work-kind.new{",
		".work-kind.rework{",
		".clanker-idle-rework,.clanker-rework-pending{color:var(--status-error)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("operations page missing %q", want)
		}
	}
	script := `
const assert = require('node:assert/strict');
var ccDeferredClankers = null, ccKnownClankers = {}, adminEnabled = false;
const fleet = { querySelector() { return null; }, set innerHTML(v) { this.html = v; } };
const document = { getElementById(id) { return id === 'clanker-list' ? fleet : {}; } };
function ccUpdateArmy() {}
function ccRenderInterestRoster() {}
function esc(s) { return String(s == null ? '' : s); }
function rel() { return '5m ago'; }
function tierBadge() { return ''; }
function ccAdvisorLabel() { return ''; }
function capabilityLine() { return ''; }
function protocolLine() { return ''; }
function knowledgeLine() { return ''; }
function clankerInterestsLine() { return ''; }
function clankerFailureLine() { return ''; }
function clankerPaneBlock() { return ''; }
function ccIssueURL(item) { return item && item.url ? item.url : (item && item.repo && item.number ? 'https://github.com/' + item.repo + '/issues/' + item.number : ''); }
function ccIssueLinkHTML(item, label, extra) { var u = ccIssueURL(item); return u ? '<a class="cc-issue-link ' + (extra||'') + '" href="' + u + '">' + label + '</a>' : '<span>' + label + '</span>'; }
` + jsFunc(t, body, "idleReasonLabel") + "\n" +
		jsFunc(t, body, "clankerCapacityLine") + "\n" +
		jsFunc(t, body, "clankerWorkRef") + "\n" +
		jsFunc(t, body, "clankerInFlightLines") + "\n" +
		jsFunc(t, body, "clankerIdleLine") + "\n" +
		jsFunc(t, body, "clankerPendingReworkLine") + "\n" +
		jsFunc(t, body, "renderClankers") + `
// Capacity 3, primary issue + two spawned fixes, nothing pending.
renderClankers([{github_username: 'bob', trust_tier: 'contributor', parallel_capacity: 3, fix_mode: 'spawn',
  current_task: {task_id: 't1', kind: 'issue', repo: 'org/r', number: 100},
  in_flight: [
    {task_id: 't1', kind: 'new', task_kind: 'issue', repo: 'org/r', number: 100, title: 'add thing', url: 'https://x/100', stage: 'stage/020-claimed', started_at: '2026-10-16T12:00:00Z'},
    {task_id: 'f1', kind: 'rework', task_kind: 'fix-pr', repo: 'org/r', number: 7, url: 'https://x/7', stage: 'stage/072-returned-to-contributor', mode: 'spawn'},
    {task_id: 'f2', kind: 'rework', task_kind: 'fix-pr', repo: 'org/r', number: 8, url: 'https://x/8', stage: 'stage/072-returned-to-contributor', mode: 'spawn'}
  ], fix_queue_depth: 0, pending_rework: 0}]);
let html = fleet.html;
assert.match(html, /clanker-capacity[^>]*>capacity 3 &middot; spawn</, 'capacity line');
assert.equal((html.match(/class="clanker-sub clanker-inflight"/g) || []).length, 3, 'one line per in-flight item');
assert.equal((html.match(/class="work-kind new"/g) || []).length, 1, 'one NEW badge');
assert.equal((html.match(/class="work-kind rework"/g) || []).length, 2, 'two REWORK badges');
assert.match(html, />NEW<\/span> <a class="cc-issue-link work-ref" href="https:\/\/x\/100">org\/r#100<\/a> <span class="work-title"[^>]*>add thing<\/span> &middot; <span class="work-stage">stage\/020-claimed<\/span> &middot; 5m ago/, 'primary row');
assert.match(html, />REWORK<\/span> <a class="cc-issue-link work-ref" href="https:\/\/x\/7">org\/r#7<\/a> &middot; <span class="work-stage">stage\/072-returned-to-contributor<\/span> &middot; spawn/, 'fix row');
assert.doesNotMatch(html, /awaiting rework/, 'nothing pending');
assert.doesNotMatch(html, /idle/, 'busy relay is not idle');
assert.match(html, /clanker-status working/, 'status pill is working');

// Idle relay, capacity not advertised, two PRs returned and not dispatched.
renderClankers([{github_username: 'alice', parallel_capacity: 0, fix_mode: 'preempt', idle_reason: 'no_matching_work',
  fix_queue_depth: 1, pending_rework: 2,
  pending_rework_prs: [{repo: 'org/r', number: 2, cause: 'build', url: 'https://x/2'}, {repo: 'org/r', number: 3, stage: 'stage/072-returned-to-contributor'}]}]);
html = fleet.html;
assert.match(html, /clanker-capacity[^>]*>capacity: not advertised</, 'not advertised');
assert.match(html, /class="clanker-sub clanker-idle-rework">idle &middot; 2 PRs awaiting rework</, 'idle line names the rework');
assert.doesNotMatch(html, /idle: no matching work/, 'idle reason replaced while rework waits');
assert.match(html, /class="clanker-sub clanker-rework-pending" title="org\/r#2 \(build\), org\/r#3">2 PRs awaiting rework \(not yet dispatched\) &middot; fix queue 1: <a class="cc-issue-link work-ref" href="https:\/\/x\/2">org\/r#2<\/a>, <a class="cc-issue-link work-ref" href="https:\/\/github.com\/org\/r\/issues\/3">org\/r#3<\/a></, 'awaiting-rework line');
assert.match(html, /clanker-status idle/, 'status pill is idle');

// Idle, nothing pending: the pre-#12502 idle reason line is unchanged.
renderClankers([{github_username: 'carol', idle_reason: 'no_matching_work', pending_rework: 0}]);
html = fleet.html;
assert.match(html, /class="clanker-sub">idle: no matching work</, 'plain idle reason');
assert.doesNotMatch(html, /awaiting rework/);

// Older hub without in_flight: the single current_task line still renders.
renderClankers([{github_username: 'dan', current_task: {repo: 'org/r', number: 5}}]);
assert.match(fleet.html, /class="clanker-sub">on org\/r#5</, 'legacy current_task line');
`
	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("clanker row render: %v\n%s", err, out)
	}
}

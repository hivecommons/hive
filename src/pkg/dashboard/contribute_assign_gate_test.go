package dashboard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
)

const (
	gateRepo      = "myorg/repo1"
	gateHeldIssue = 19788
	gateFreeIssue = 2
)

// fakeAssignGate answers the live issue read from a per-issue table and
// records which issues were read.
type fakeAssignGate struct {
	mu    sync.Mutex
	gates map[int]ghpkg.IssueAssignGate
	errs  map[int]error
	reads []int
}

func (f *fakeAssignGate) read(_ context.Context, _ string, number int) (ghpkg.IssueAssignGate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, number)
	if err := f.errs[number]; err != nil {
		return ghpkg.IssueAssignGate{}, err
	}
	return f.gates[number], nil
}

func (f *fakeAssignGate) readCount(number int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reads {
		if r == number {
			n++
		}
	}
	return n
}

func gateConn(hub *ContributeWSHub, id string) *ContributorConnection {
	c := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: id, ContributorID: "c-" + id, TrustTier: "contributor"},
		session:  id,
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-"+id] = c
	hub.mu.Unlock()
	return c
}

func heldGate() ghpkg.IssueAssignGate {
	return ghpkg.IssueAssignGate{Reason: ghpkg.ClaimBlockReasonHold, Held: true, Labels: []string{"hold"}}
}

// The incident: the cached snapshot still lists the issue as actionable with
// no hold label (the hold went on seconds ago), but the forge says it is
// held. It is never assigned, nothing is leased for it, and the next issue
// is offered instead.
func TestAssignGate_HeldIssueNeverAssignedFromStaleCache(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s,
		intgIssue(gateHeldIssue, "held seconds ago", "someone", nil),
		intgIssue(gateFreeIssue, "free", "someone", nil),
	)
	fake := &fakeAssignGate{gates: map[int]ghpkg.IssueAssignGate{gateHeldIssue: heldGate()}}
	hub.assignGate = fake.read
	c := gateConn(hub, "worker")

	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_assign" || msg.Number != gateFreeIssue {
		t.Fatalf("selectTask = %+v, want task_assign for #%d", msg, gateFreeIssue)
	}
	if fake.readCount(gateHeldIssue) != 1 {
		t.Fatalf("held issue read %d times, want exactly 1 before assignment", fake.readCount(gateHeldIssue))
	}
	for key := range hub.leasedIssueKeys("someone-else", time.Now()) {
		if key == gateRepo+"#19788" {
			t.Fatal("a lease was recorded for the held issue")
		}
	}

	// A second contributor is not offered it either, and the remembered
	// refusal spares another read.
	hub.rollbackAssignment(c, msg.TaskID)
	other := gateConn(hub, "other")
	setStatusIssues(s, intgIssue(gateHeldIssue, "held seconds ago", "someone", nil))
	if msg := hub.selectTask(other); msg == nil || msg.Type != "task_unavailable" {
		t.Fatalf("selectTask = %+v, want task_unavailable while the only issue is held", msg)
	}
	if fake.readCount(gateHeldIssue) != 1 {
		t.Fatalf("held issue re-read %d times inside the refusal TTL", fake.readCount(gateHeldIssue))
	}
}

// Only a held issue in the queue: the contributor is told there is no
// matching work, with the live-gate count in the filtered map.
func TestAssignGate_OnlyHeldIssueYieldsNoMatchingWork(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, intgIssue(gateHeldIssue, "held", "someone", nil))
	hub.assignGate = (&fakeAssignGate{gates: map[int]ghpkg.IssueAssignGate{gateHeldIssue: heldGate()}}).read
	c := gateConn(hub, "worker")

	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("selectTask = %+v, want no_matching_work", msg)
	}
	if msg.Filtered[filterLiveGate] != 1 {
		t.Fatalf("filtered = %v, want %s=1", msg.Filtered, filterLiveGate)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentTask != nil {
		t.Fatalf("connection holds %+v after a refused selection", c.currentTask)
	}
}

// An untriaged issue from an untrusted (CONTRIBUTOR) reporter is refused by
// the live gate.
func TestAssignGate_UntriagedContributorIssueNeverAssigned(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, intgIssue(gateHeldIssue, "stranger's request", "someone", nil))
	hub.assignGate = (&fakeAssignGate{gates: map[int]ghpkg.IssueAssignGate{gateHeldIssue: {
		Reason: ghpkg.ClaimBlockReasonAwaitingTriagePrefix + "triage/accepted", AwaitingTriage: true,
	}}}).read
	if msg := hub.selectTask(gateConn(hub, "worker")); msg == nil || msg.Type != "task_unavailable" {
		t.Fatalf("selectTask = %+v, want task_unavailable", msg)
	}
}

// The cached filter: with reporter trust on, a CONTRIBUTOR's issue whose
// cached labels carry no triage label never reaches the Ready queue or
// selection — even with no live gate wired.
func TestAssignGate_CachedTriageFilterWithholdsUntriagedIssue(t *testing.T) {
	hub, s := covK2Hub(t)
	on := true
	s.deps.Config.Project.IssueFilter = config.IssueFilterConfig{ReporterTrust: config.ReporterTrustConfig{Enabled: &on}}
	untriaged := intgIssue(gateHeldIssue, "stranger's request", "someone", nil)
	untriaged["author_is_human"] = true
	untriaged["author_association"] = config.AuthorAssociationContributor
	triaged := intgIssue(gateFreeIssue, "triaged request", "someone", nil)
	triaged["author_is_human"] = true
	triaged["author_association"] = config.AuthorAssociationContributor
	triaged["labels"] = []any{"triage/accepted"}
	setStatusIssues(s, untriaged, triaged)

	for _, item := range hub.ReadyQueue(readyQueueDefaultLimit) {
		if item.Number == gateHeldIssue {
			t.Fatalf("Ready queue lists the untriaged issue: %+v", item)
		}
	}
	msg := hub.selectTask(gateConn(hub, "worker"))
	if msg == nil || msg.Type != "task_assign" || msg.Number != gateFreeIssue {
		t.Fatalf("selectTask = %+v, want the triaged #%d", msg, gateFreeIssue)
	}
}

// The cached filter: a hold label the webhook patched onto the cached row
// keeps it out of the Ready queue without any live read.
func TestAssignGate_CachedHoldLabelWithheld(t *testing.T) {
	hub, s := covK2Hub(t)
	held := intgIssue(gateHeldIssue, "held", "someone", nil)
	held["labels"] = []any{"do-not-merge/hold"}
	setStatusIssues(s, held, intgIssue(gateFreeIssue, "free", "someone", nil))
	fake := &fakeAssignGate{}
	hub.assignGate = fake.read

	for _, item := range hub.ReadyQueue(readyQueueDefaultLimit) {
		if item.Number == gateHeldIssue {
			t.Fatalf("Ready queue lists the held issue: %+v", item)
		}
	}
	msg := hub.selectTask(gateConn(hub, "worker"))
	if msg == nil || msg.Number != gateFreeIssue {
		t.Fatalf("selectTask = %+v, want #%d", msg, gateFreeIssue)
	}
	if fake.readCount(gateHeldIssue) != 0 {
		t.Fatal("the cached hold should have refused the issue before any live read")
	}
}

// A failed live read fails closed: the issue is not assigned this round.
func TestAssignGate_APIErrorFailsClosed(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, intgIssue(gateHeldIssue, "unknown", "someone", nil))
	hub.assignGate = (&fakeAssignGate{errs: map[int]error{gateHeldIssue: errors.New("502 bad gateway")}}).read
	c := gateConn(hub, "worker")
	if msg := hub.selectTask(c); msg == nil || msg.Type != "task_unavailable" {
		t.Fatalf("selectTask = %+v, want task_unavailable on a failed read", msg)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentTask != nil {
		t.Fatalf("assigned %+v on an unknown verdict", c.currentTask)
	}
}

// A skip label added since the snapshot (live labels) refuses as well.
func TestAssignGate_LiveSkipLabelRefuses(t *testing.T) {
	hub, s := covK2Hub(t)
	s.deps.Config.Hub.ContributeSkipLabels = []string{"not-for-contributors"}
	setStatusIssues(s, intgIssue(gateHeldIssue, "skip", "someone", nil))
	hub.assignGate = (&fakeAssignGate{gates: map[int]ghpkg.IssueAssignGate{gateHeldIssue: {Labels: []string{"not-for-contributors"}}}}).read
	if msg := hub.selectTask(gateConn(hub, "worker")); msg == nil || msg.Type != "task_unavailable" {
		t.Fatalf("selectTask = %+v, want task_unavailable", msg)
	}
}

// The queue-hold and the cached skip-label behaviour are unchanged: both
// still refuse before any live read.
func TestAssignGate_QueueHoldAndSkipLabelUnchanged(t *testing.T) {
	hub, s := covK2Hub(t)
	s.deps.Config.Hub.ContributeQueueHold = []string{gateRepo + "#19788"}
	s.deps.Config.Hub.ContributeSkipLabels = []string{"not-for-contributors"}
	skipped := intgIssue(3, "skip", "someone", nil)
	skipped["labels"] = []any{"not-for-contributors"}
	setStatusIssues(s, intgIssue(gateHeldIssue, "queue-held", "someone", nil), skipped,
		intgIssue(gateFreeIssue, "free", "someone", nil))
	fake := &fakeAssignGate{}
	hub.assignGate = fake.read

	msg := hub.selectTask(gateConn(hub, "worker"))
	if msg == nil || msg.Type != "task_assign" || msg.Number != gateFreeIssue {
		t.Fatalf("selectTask = %+v, want #%d", msg, gateFreeIssue)
	}
	if fake.readCount(gateHeldIssue) != 0 || fake.readCount(3) != 0 {
		t.Fatalf("queue-held or skip-labelled issue reached the live read: %v", fake.reads)
	}
	if fake.readCount(gateFreeIssue) != 1 {
		t.Fatalf("assigned issue read %d times, want 1", fake.readCount(gateFreeIssue))
	}
}

func assignedConn(hub *ContributeWSHub, id string, number int) *ContributorConnection {
	c := gateConn(hub, id)
	c.mu.Lock()
	c.currentTask = &WSTaskAssign{TaskID: "ct-" + id, Repo: gateRepo, Number: number}
	c.currentTaskGen = 7
	c.pendingToken = "ghs_pending"
	c.lastLeaseRenew = time.Now()
	c.mu.Unlock()
	return c
}

func requireRevoked(t *testing.T, c *ContributorConnection, number int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentTask != nil && c.currentTask.Number == number {
		t.Fatalf("task on #%d not revoked", number)
	}
	if c.currentTaskGen == 7 {
		t.Fatal("generation not bumped: a stale completion would not be fenced")
	}
	if c.pendingToken == "ghs_pending" {
		t.Fatal("pending credential survived the revocation")
	}
}

// Adding a hold to an assigned issue revokes the task; a session on other
// work is untouched.
func TestPreemptIssueHeld_RevokesAssignedTask(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, intgIssue(gateHeldIssue, "now held", "someone", nil))
	hub.assignGate = (&fakeAssignGate{gates: map[int]ghpkg.IssueAssignGate{gateHeldIssue: heldGate()}}).read
	worker := assignedConn(hub, "worker", gateHeldIssue)
	sibling := assignedConn(hub, "sibling", gateFreeIssue)

	if n := hub.PreemptIssueHeld("MyOrg/Repo1", gateHeldIssue); n != 1 {
		t.Fatalf("released %d sessions, want 1", n)
	}
	requireRevoked(t, worker, gateHeldIssue)
	sibling.mu.Lock()
	defer sibling.mu.Unlock()
	if sibling.currentTask == nil || sibling.currentTask.Number != gateFreeIssue {
		t.Fatalf("sibling disturbed: %+v", sibling.currentTask)
	}
}

// The webhook path: an `issues` re-read classified held revokes the task;
// one classified as an untrusted reporter's untriaged issue does too.
func TestPreemptBlockedSnapshots_WebhookRevokes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch ghpkg.WebhookItemPatch
	}{
		{"hold added", ghpkg.WebhookItemPatch{Bucket: ghpkg.WebhookBucketHeld}},
		{"triage removed", ghpkg.WebhookItemPatch{WorkIssue: &ghpkg.RepoWorkIssue{Bucket: string(stageengine.BucketReporterTriage)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, s := covK2Hub(t)
			s.contributeHub = hub
			setStatusIssues(s)
			worker := assignedConn(hub, "worker", gateHeldIssue)
			s.preemptBlockedSnapshots([]ghpkg.WebhookItemSnapshot{{
				Repo: "repo1", Number: gateHeldIssue, State: "open", Patch: tc.patch,
			}})
			requireRevoked(t, worker, gateHeldIssue)
		})
	}
}

// An actionable re-read revokes nothing.
func TestPreemptBlockedSnapshots_ActionableLeavesTask(t *testing.T) {
	hub, s := covK2Hub(t)
	s.contributeHub = hub
	worker := assignedConn(hub, "worker", gateHeldIssue)
	s.preemptBlockedSnapshots([]ghpkg.WebhookItemSnapshot{{
		Repo: gateRepo, Number: gateHeldIssue, State: "open", Patch: ghpkg.WebhookItemPatch{Bucket: ghpkg.WebhookBucketActionable},
	}})
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.currentTask == nil || worker.currentTask.Number != gateHeldIssue {
		t.Fatalf("actionable re-read revoked the task: %+v", worker.currentTask)
	}
}

// The periodic reconcile catches a hold whose webhook was missed, and a
// failed read leaves the task running (revocation needs a verdict).
func TestReconcileHeldTasks_MissedWebhook(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s)
	const failing = 77
	hub.assignGate = (&fakeAssignGate{
		gates: map[int]ghpkg.IssueAssignGate{gateHeldIssue: heldGate()},
		errs:  map[int]error{failing: errors.New("timeout")},
	}).read
	held := assignedConn(hub, "held", gateHeldIssue)
	unknown := assignedConn(hub, "unknown", failing)

	if n := hub.reconcileHeldTasks(context.Background()); n != 1 {
		t.Fatalf("reconcile released %d sessions, want 1", n)
	}
	requireRevoked(t, held, gateHeldIssue)
	unknown.mu.Lock()
	defer unknown.mu.Unlock()
	if unknown.currentTask == nil || unknown.currentTask.Number != failing {
		t.Fatalf("a failed read revoked the task: %+v", unknown.currentTask)
	}
}

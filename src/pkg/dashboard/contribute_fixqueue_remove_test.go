package dashboard

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fixqueue"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A contributor's brand-new PR was returned to its author (072) and its fix
// was queued; four seconds later the fix left the queue with a bare
// "[contribute-ws] fix queue remove" line and was never dispatched again.
// The Operations card had removed it: the status snapshot's open-PR list
// vouched for the repo, but the enumeration behind it ran before the PR was
// listed, so the PR looked closed.

// fqrLogs is a slog handler that keeps every record's message and attributes.
type fqrLogs struct {
	mu   sync.Mutex
	recs []map[string]string
}

func (l *fqrLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l *fqrLogs) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *fqrLogs) WithGroup(string) slog.Handler            { return l }
func (l *fqrLogs) Handle(_ context.Context, r slog.Record) error {
	m := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	l.mu.Lock()
	l.recs = append(l.recs, m)
	l.mu.Unlock()
	return nil
}

// removeReason returns the reason on the "fix queue remove" line for pr.
func (l *fqrLogs) removeReason(t *testing.T, pr string) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.recs {
		if m["msg"] == "[contribute-ws] fix queue remove" && strings.Contains(m["prs"], pr) {
			return m["reason"]
		}
	}
	t.Fatalf("no fix queue remove line for %s", pr)
	return ""
}

// fqrClock is the stage store's clock: real time unless a test pins it.
type fqrClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fqrClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() {
		return time.Now()
	}
	return c.at
}

func (c *fqrClock) set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

// fqrHub is reworkHub with the contribute hub logging into a capture and a
// stage store whose clock fqrObserve can pin.
func fqrHub(t *testing.T) (*ContributeWSHub, *Server, *fqrLogs, *fqrClock) {
	t.Helper()
	dir := os.Getenv("HIVE_CONTRIBUTORS_DIR")
	if dir == "" {
		dir = t.TempDir()
		t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	}
	redirectContributeWSDisk(t, dir)
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := NewServer(0, quiet)
	srv.RegisterAPI(testDeps(t))
	t.Cleanup(srv.CloseContributeHub)
	logs := &fqrLogs{}
	hub := NewContributeWSHub(slog.New(logs), srv)
	t.Cleanup(hub.Close)
	srv.contributeHub = hub
	clk := &fqrClock{}
	hub.stageStore = stagelabels.NewStore("", clk.now)
	return hub, srv, logs, clk
}

// fqrListing publishes a status whose complete open-PR list for org/r
// carries only prs, built from an enumeration that ran at listedAt.
func fqrListing(srv *Server, listedAt time.Time, prs ...int) {
	repo := FrontendRepo{Name: "r", Full: "org/r"}
	for _, n := range prs {
		repo.OpenPrs = append(repo.OpenPrs, FrontendPR{PullRequest: github.PullRequest{Repo: "org/r", Number: n}})
	}
	srv.statusMu.Lock()
	srv.status = &StatusPayload{Repos: []FrontendRepo{repo}, ListedAt: listedAt}
	srv.statusMu.Unlock()
}

// fqrObserve records org/r#n at stage as a stage pass saw it at `at`.
func fqrObserve(t *testing.T, hub *ContributeWSHub, clk *fqrClock, n int, stage stagelabels.Stage, at time.Time) {
	t.Helper()
	clk.set(at)
	defer clk.set(time.Time{})
	hub.stageStore.Observe(stagelabels.Key("org/r", n), stage, "check-failing:build")
	if rec, ok := hub.stageStore.Get(stagelabels.Key("org/r", n)); !ok || rec.Stage != stage || !rec.LastSeen.Equal(at) {
		t.Fatalf("org/r#%d record = %+v, want %s seen at %s", n, rec, stage.Label(), at)
	}
}

func queuedNumbers(hub *ContributeWSHub, user string) map[int]bool {
	out := map[int]bool{}
	for _, it := range hub.fixQueue().Queued(user) {
		out[it.Number] = true
	}
	return out
}

func TestFleetReworkKeepsFixWhoseStageRecordIsNewerThanTheListing(t *testing.T) {
	hub, srv, logs, clk := fqrHub(t)
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	for _, n := range []int{7, 8, 9} {
		hub.fixQueue().Enqueue("alice", fixqueue.Item{Repo: "org/r", Number: n, Title: "red"})
		hub.rememberFixOwner("alice", fixqueue.Key("org/r", n))
	}
	// The listing ran after every fix was queued, so only the stage
	// records can say whether it is older than the evidence.
	listedAt := time.Now().Add(time.Millisecond)
	fqrListing(srv, listedAt)                                                                 // none of #7-#9 listed
	fqrObserve(t, hub, clk, 7, stagelabels.ReturnedToContributor, listedAt.Add(time.Minute))  // 072, seen after the listing
	fqrObserve(t, hub, clk, 8, stagelabels.ReturnedToContributor, listedAt.Add(-time.Minute)) // 072, last seen before it
	fqrObserve(t, hub, clk, 9, stagelabels.Fixing, listedAt.Add(time.Minute))                 // 070: not waiting on the author

	fc := snapshotFor(t, hub, "alice")
	if q := queuedNumbers(hub, "alice"); len(q) != 1 || !q[7] {
		t.Fatalf("queued = %v, want only #7: a listing older than the 072 record cannot say the PR left", q)
	}
	if fc.FixQueueDepth != 1 || fc.PendingRework != 1 || fc.PendingReworkPRs[0].Number != 7 {
		t.Fatalf("card = %+v, want #7 pending", fc.FleetRework)
	}
	for _, n := range []string{"org/r#8", "org/r#9"} {
		if got := logs.removeReason(t, n); got != fixRemoveFleetCardPrefix+fleetReworkGoneNotListed {
			t.Fatalf("%s removed with reason %q", n, got)
		}
	}
}

func TestFleetReworkKeepsFixEnqueuedAfterTheListing(t *testing.T) {
	hub, srv, logs, clk := fqrHub(t)
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	listedAt := time.Now().Add(-10 * time.Minute)
	fqrListing(srv, listedAt)
	// #11 has no stage record yet; #12 was adopted by the hive (074).
	fqrObserve(t, hub, clk, 12, stagelabels.AdoptedByHive, time.Now())
	for _, n := range []int{11, 12} {
		hub.fixQueue().Enqueue("alice", fixqueue.Item{Repo: "org/r", Number: n, Title: "red"})
		hub.rememberFixOwner("alice", fixqueue.Key("org/r", n))
	}

	snapshotFor(t, hub, "alice")
	if q := queuedNumbers(hub, "alice"); len(q) != 1 || !q[11] {
		t.Fatalf("queued = %v, want only #11: a fix queued after the listing ran is not dropped on its say-so", q)
	}
	if got := logs.removeReason(t, "org/r#12"); got != fixRemoveFleetCardPrefix+fleetReworkGoneAdopted {
		t.Fatalf("074 removal reason = %q", got)
	}
}

func TestFleetReworkListingNewerThanEvidenceStillDropsAClosedPR(t *testing.T) {
	// hivecommons/hive#14708 stands: a PR the newer listing no longer
	// carries, last seen by a stage pass before it, leaves the queue.
	hub, srv, logs, clk := fqrHub(t)
	fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	fqrObserve(t, hub, clk, 21, stagelabels.ReturnedToContributor, time.Now().Add(-time.Hour))
	hub.fixQueue().Enqueue("alice", fixqueue.Item{Repo: "org/r", Number: 21, Title: "merged"})
	hub.fixQueue().Enqueue("alice", fixqueue.Item{Repo: "org/r", Number: 22, Title: "open"})
	fqrListing(srv, time.Now().Add(time.Millisecond), 22)

	snapshotFor(t, hub, "alice")
	if q := queuedNumbers(hub, "alice"); len(q) != 1 || !q[22] {
		t.Fatalf("queued = %v, want only the listed #22", q)
	}
	if got := logs.removeReason(t, "org/r#21"); got != fixRemoveFleetCardPrefix+fleetReworkGoneNotListed {
		t.Fatalf("reason = %q", got)
	}
}

func TestFixQueueRemoveLogsEachCallersCause(t *testing.T) {
	hub, _, logs, _ := fqrHub(t)
	c := fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	sched := hub.fixQueue()
	queue := func(n, attempts int) {
		sched.Enqueue("alice", fixqueue.Item{Repo: "org/r", Number: n, Title: "red", Attempts: attempts})
		hub.rememberFixOwner("alice", fixqueue.Key("org/r", n))
	}

	// The PR left stage 072.
	queue(1, 0)
	hub.Resolve(context.Background(), "org", "r", 1)

	// The dispatch check read the PR merged.
	queue(2, 0)
	hub.fixPRStateHook = func(_ context.Context, _ string, n int) (fixPRState, string, error) {
		if n == 2 {
			return fixPRStateGone, "merged", nil
		}
		return fixPRStateOpen, "", nil
	}
	hub.dispatchFixes("alice", []*ContributorConnection{c}, false)

	// The relay failed the fix for the last allowed time.
	queue(3, fixTaskMaxAttempts-1)
	got := captureFixDispatch(hub)
	hub.dispatchFixes("alice", []*ContributorConnection{c}, false)
	if len(*got) != 1 {
		t.Fatalf("dispatches = %d, want #3 handed out", len(*got))
	}
	hub.failFixTask(c, WSMessage{Type: "task_failed", TaskID: (*got)[0].TaskID, Message: "session crashed"})

	// An operator's reroute lapsed.
	queue(4, 0)
	hub.CancelReroute("alice", "org/r", 4)

	want := map[string]string{
		"org/r#1": fixRemoveLeftStage072,
		"org/r#2": fixRemovePRClosed,
		"org/r#3": fixRemoveRelayGaveUp,
		"org/r#4": fixRemoveRerouteCancelled,
	}
	for pr, reason := range want {
		if got := logs.removeReason(t, pr); got != reason {
			t.Errorf("%s: remove reason = %q, want %q", pr, got, reason)
		}
	}
}

// An older relay polling another hive hands a fix back unstarted ("Hub is
// not the active polling slot"). The fix waits out the backoff in the queue
// and is offered again when it ends; before, only the relay's next ready
// here or a re-nudge (72h for the same head) offered it again.
func TestFixQueueReturnedFixIsOfferedAgainWhenBackoffEnds(t *testing.T) {
	hub, _ := covK2Hub(t)
	clock := time.Now()
	hub.fixSched = fixqueue.New(fixqueue.Options{Now: func() time.Time { return clock }})
	hub.fixPRStateHook = func(context.Context, string, int) (fixPRState, string, error) {
		return fixPRStateOpen, "", nil
	}
	var waits []time.Duration
	var retry func()
	hub.fixRetryAfter = func(d time.Duration, f func()) {
		waits = append(waits, d)
		retry = f
	}
	c := fixConn(hub, "alice", fixCaps(capFixPRInterrupt), time.Now())
	got := captureFixDispatch(hub)
	hub.fixQueue().Enqueue("alice", fixqueue.Item{Repo: "org/r", Number: 1, Title: "red"})
	hub.rememberFixOwner("alice", "org/r#1")
	hub.dispatchFixes("alice", []*ContributorConnection{c}, false)
	if len(*got) != 1 {
		t.Fatalf("dispatches = %d, want the interrupt", len(*got))
	}

	hub.failFixTask(c, WSMessage{Type: "task_failed", TaskID: (*got)[0].TaskID, Reason: relayRefusedNotActiveSlot})
	if q := hub.FixQueueItems("alice"); len(q) != 1 || q[0].Attempts != 0 {
		t.Fatalf("queued = %+v, want the fix back with no attempt counted", q)
	}
	if len(waits) != 1 || waits[0] != fixTaskRetryBackoff || retry == nil {
		t.Fatalf("retry armed = %v, want one after %s", waits, fixTaskRetryBackoff)
	}

	clock = clock.Add(fixTaskRetryBackoff)
	retry()
	if len(*got) != 2 || (*got)[1].Kind != taskKindFixPR || (*got)[1].Number != 1 {
		t.Fatalf("dispatches after the backoff = %+v, want #1 offered again", *got)
	}
	if q := hub.FixQueueItems("alice"); len(q) != 0 {
		t.Fatalf("queued = %+v, want the fix in flight", q)
	}
}

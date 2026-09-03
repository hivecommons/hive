package planning

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// fakeKicker records kicks and can be made to fail.
type fakeKicker struct {
	kicks []struct{ agent, msg string }
	fail  bool
}

func (f *fakeKicker) Kick(agent, message string) error {
	f.kicks = append(f.kicks, struct{ agent, msg string }{agent, message})
	if f.fail {
		return errFake
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

const errFake = errString("kick failed")

// fakeRecorder records RecordKick calls.
type fakeRecorder struct{ kicked []string }

func (f *fakeRecorder) RecordKick(agent string) { f.kicked = append(f.kicked, agent) }

// fakeSink captures sink calls.
type fakeSink struct {
	audits  []string
	alerts  []struct{ id, sev, msg string }
	notifys int
}

func (f *fakeSink) Audit(actor, action, detail, agent string) {
	f.audits = append(f.audits, action)
}
func (f *fakeSink) Alert(id, severity, message string) {
	f.alerts = append(f.alerts, struct{ id, sev, msg string }{id, severity, message})
}
func (f *fakeSink) Notify(title, message string) { f.notifys++ }

// setupStalledPlan creates an approved epic with one open child whose activity
// is backdated so the plan reads as stalled. Returns the epic ID.
func setupStalledPlan(t *testing.T, store *beads.Store, replans string) string {
	t.Helper()
	epic := mustEpic(t, store, "Stalled epic")
	if err := store.SetMetadata(epic.ID, MetaPlanStatus, PlanStatusApproved); err != nil {
		t.Fatal(err)
	}
	if replans != "" {
		if err := store.SetMetadata(epic.ID, MetaReplanCount, replans); err != nil {
			t.Fatal(err)
		}
	}
	child, err := store.Create("stuck task", beads.TypeTask, beads.PriorityMedium, "worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(child.ID, MetaParentEpic, epic.ID); err != nil {
		t.Fatal(err)
	}
	return epic.ID
}

// oldStall is a threshold small enough that any real store timestamp (now)
// counts as stalled when we detect at a time far in the future.
func farFuture() time.Time { return time.Now().Add(48 * time.Hour) }

func newTestLane(store *beads.Store, k Kicker, r KickRecorder, s Sink) *ReplanLane {
	return NewReplanLane(
		map[string]*beads.Store{"main": store},
		k, r, s,
		ReplanLaneConfig{IntervalS: 1, Stall: StallConfig{StallThreshold: time.Hour, MaxReplans: 3}},
		nil,
	)
}

func TestReplanLane_UnderCap_KicksAndAccounts(t *testing.T) {
	store := newStore(t)
	epicID := setupStalledPlan(t, store, "1")
	k, r, s := &fakeKicker{}, &fakeRecorder{}, &fakeSink{}
	lane := newTestLane(store, k, r, s)
	lane.lastRun = farFuture().Add(-48 * time.Hour) // ensure Due

	// Force detection to see the plan as stalled by running "now" in the future.
	// Run() uses time.Now(); the store child was just created (now), threshold 1h,
	// so it is NOT yet stalled. Drive the pure path directly to assert the mutation
	// deterministically, then also exercise Run for the no-op branch.
	snap := SnapshotFromStore(store)
	stalled := DetectStalledPlans(snap, farFuture(), lane.stallCfg)
	if len(stalled) != 1 {
		t.Fatalf("expected 1 stalled plan, got %d", len(stalled))
	}
	if !lane.handleStall(store, "main", stalled[0], farFuture()) {
		t.Fatal("handleStall should report a replan under cap")
	}

	// replan_count bumped 1 → 2, last_replan_at stamped.
	got, _ := store.Get(epicID)
	if got.Meta(MetaReplanCount) != "2" {
		t.Errorf("replan_count = %q, want 2", got.Meta(MetaReplanCount))
	}
	if got.Meta(MetaLastReplanAt) == "" {
		t.Error("last_replan_at not stamped")
	}
	if len(k.kicks) != 1 || k.kicks[0].agent != architectAgent {
		t.Errorf("architect not kicked: %+v", k.kicks)
	}
	if !strings.Contains(k.kicks[0].msg, "STALLED") {
		t.Errorf("kick prompt missing stall framing: %q", k.kicks[0].msg)
	}
	if len(r.kicked) != 1 {
		t.Errorf("RecordKick not called: %+v", r.kicked)
	}
	if s.notifys != 1 || len(s.alerts) != 1 || s.alerts[0].sev != "warning" {
		t.Errorf("sink not driven correctly: alerts=%+v notifys=%d", s.alerts, s.notifys)
	}
}

func TestReplanLane_AtCap_EscalatesNoKick(t *testing.T) {
	store := newStore(t)
	epicID := setupStalledPlan(t, store, "3") // == MaxReplans
	k, r, s := &fakeKicker{}, &fakeRecorder{}, &fakeSink{}
	lane := newTestLane(store, k, r, s)

	snap := SnapshotFromStore(store)
	stalled := DetectStalledPlans(snap, farFuture(), lane.stallCfg)
	if len(stalled) != 1 || !stalled[0].CapReached {
		t.Fatalf("expected cap-reached stall, got %+v", stalled)
	}
	if lane.handleStall(store, "main", stalled[0], farFuture()) {
		t.Fatal("handleStall should NOT report a replan at cap")
	}
	// No kick, no count bump.
	if len(k.kicks) != 0 {
		t.Errorf("should not kick at cap, got %+v", k.kicks)
	}
	got, _ := store.Get(epicID)
	if got.Meta(MetaReplanCount) != "3" {
		t.Errorf("replan_count changed at cap: %q", got.Meta(MetaReplanCount))
	}
	if len(s.alerts) != 1 || s.alerts[0].sev != "error" {
		t.Errorf("cap escalation should raise an error alert: %+v", s.alerts)
	}
	if !strings.Contains(s.alerts[0].msg, "replan cap") {
		t.Errorf("cap alert message unexpected: %q", s.alerts[0].msg)
	}
}

func TestReplanLane_KickFailure_StillAccounts(t *testing.T) {
	store := newStore(t)
	epicID := setupStalledPlan(t, store, "0")
	k, r, s := &fakeKicker{fail: true}, &fakeRecorder{}, &fakeSink{}
	lane := newTestLane(store, k, r, s)

	snap := SnapshotFromStore(store)
	stalled := DetectStalledPlans(snap, farFuture(), lane.stallCfg)
	if !lane.handleStall(store, "main", stalled[0], farFuture()) {
		t.Fatal("a failed kick still counts as a replan attempt")
	}
	got, _ := store.Get(epicID)
	if got.Meta(MetaReplanCount) != "1" {
		t.Errorf("replan_count should bump even on kick failure: %q", got.Meta(MetaReplanCount))
	}
	// RecordKick is NOT called when the kick itself failed.
	if len(r.kicked) != 0 {
		t.Errorf("RecordKick should be skipped on kick failure: %+v", r.kicked)
	}
}

func TestReplanLane_NilKickerAndSink(t *testing.T) {
	store := newStore(t)
	epicID := setupStalledPlan(t, store, "0")
	lane := NewReplanLane(map[string]*beads.Store{"m": store}, nil, nil, nil,
		ReplanLaneConfig{IntervalS: 1, Stall: StallConfig{StallThreshold: time.Hour, MaxReplans: 3}}, nil)

	snap := SnapshotFromStore(store)
	stalled := DetectStalledPlans(snap, farFuture(), lane.stallCfg)
	if !lane.handleStall(store, "m", stalled[0], farFuture()) {
		t.Fatal("handleStall should still account with nil kicker/sink")
	}
	got, _ := store.Get(epicID)
	if got.Meta(MetaReplanCount) != "1" {
		t.Errorf("accounting must happen without a kicker: %q", got.Meta(MetaReplanCount))
	}
}

func TestReplanLane_Run_NoStalls(t *testing.T) {
	store := newStore(t)
	setupStalledPlan(t, store, "0") // child just created → within threshold → not stalled
	k := &fakeKicker{}
	lane := newTestLane(store, k, &fakeRecorder{}, &fakeSink{})
	if n := lane.Run(context.Background()); n != 0 {
		t.Errorf("fresh plan should not replan, got %d", n)
	}
	if len(k.kicks) != 0 {
		t.Errorf("no kicks expected: %+v", k.kicks)
	}
}

func TestReplanLane_Run_CancelledContext(t *testing.T) {
	store := newStore(t)
	// Backdate is impossible via store, so make it stalled by shrinking threshold
	// to 0 so a just-created child is already "stale".
	setupStalledPlan(t, store, "0")
	k := &fakeKicker{}
	lane := NewReplanLane(map[string]*beads.Store{"m": store}, k, &fakeRecorder{}, &fakeSink{},
		ReplanLaneConfig{IntervalS: 1, Stall: StallConfig{StallThreshold: time.Nanosecond, MaxReplans: 3}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if n := lane.Run(ctx); n != 0 {
		t.Errorf("cancelled context should short-circuit, got %d replans", n)
	}
}

func TestReplanLane_Run_PerformsReplan(t *testing.T) {
	store := newStore(t)
	setupStalledPlan(t, store, "0")
	k, r, s := &fakeKicker{}, &fakeRecorder{}, &fakeSink{}
	// Threshold of 0/nanosecond so the just-created child reads as stalled at now.
	lane := NewReplanLane(map[string]*beads.Store{"m": store}, k, r, s,
		ReplanLaneConfig{IntervalS: 1, Stall: StallConfig{StallThreshold: time.Nanosecond, MaxReplans: 3}}, nil)
	time.Sleep(2 * time.Millisecond) // ensure now-updatedAt > threshold
	if n := lane.Run(context.Background()); n != 1 {
		t.Fatalf("expected 1 replan, got %d", n)
	}
	if len(k.kicks) != 1 {
		t.Errorf("architect should be kicked once: %+v", k.kicks)
	}
}

func TestReplanLane_Due(t *testing.T) {
	lane := NewReplanLane(nil, nil, nil, nil, ReplanLaneConfig{IntervalS: 3600}, nil)
	if !lane.Due(time.Now()) {
		t.Error("zero lastRun should be Due")
	}
	lane.lastRun = time.Now()
	if lane.Due(time.Now()) {
		t.Error("just-ran lane should not be Due")
	}
	if !lane.Due(time.Now().Add(2 * time.Hour)) {
		t.Error("lane should be Due after the interval elapses")
	}
}

func TestNewReplanLane_Defaults(t *testing.T) {
	lane := NewReplanLane(nil, nil, nil, nil, ReplanLaneConfig{}, nil)
	if lane.interval != DefaultReplanIntervalS*time.Second {
		t.Errorf("default interval = %v, want %v", lane.interval, DefaultReplanIntervalS*time.Second)
	}
	if lane.logger == nil {
		t.Error("nil logger should default to slog.Default()")
	}
}

func TestBuildReplanPrompt_ListsRemaining(t *testing.T) {
	sp := StalledPlan{
		EpicID:     "e1",
		EpicTitle:  "My Epic",
		StalledFor: 3 * time.Hour,
		Remaining: []SnapBead{
			{Title: "task one", Status: beads.StatusOpen},
			{Title: "task two", Status: beads.StatusBlocked},
		},
	}
	p := buildReplanPrompt(sp)
	for _, want := range []string{"My Epic", "e1", "task one", "task two", "[open]", "[blocked]", "UNFINISHED"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

// TestReplanLane_AccountingWriteFails exercises the replan_count SetMetadata
// error path (a persistence failure), where handleStall must NOT kick and must
// report no replan. Skipped as root (root bypasses filesystem permissions).
func TestReplanLane_AccountingWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; filesystem permission enforcement is bypassed")
	}
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epicID := setupStalledPlan(t, store, "0")
	_ = epicID
	k := &fakeKicker{}
	lane := newTestLane(store, k, &fakeRecorder{}, &fakeSink{})

	snap := SnapshotFromStore(store)
	stalled := DetectStalledPlans(snap, farFuture(), lane.stallCfg)
	if len(stalled) != 1 {
		t.Fatalf("want 1 stalled, got %d", len(stalled))
	}

	// Make the store dir read-only so the replan_count persist fails.
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	if lane.handleStall(store, "main", stalled[0], farFuture()) {
		t.Error("handleStall should report no replan when accounting write fails")
	}
	if len(k.kicks) != 0 {
		t.Errorf("must not kick when accounting failed: %+v", k.kicks)
	}
}

func TestRoundDur(t *testing.T) {
	if got := roundDur(90 * time.Second); got != 2*time.Minute {
		t.Errorf("roundDur(90s) = %v, want 2m", got)
	}
	if got := roundDur(45 * time.Second); got != 45*time.Second {
		t.Errorf("sub-minute should round to seconds, got %v", got)
	}
}

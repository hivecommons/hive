package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/tokens"
)

// These tests exercise the heartbeat payload collector that
// bootHeartbeatWith hands to startHeartbeat. The wiring tests in
// boot_heartbeat_deps_test.go capture the collector but only invoke it with
// the hub disabled (the nil early-return); everything below drives the real
// closure against a fresh boot and pins the nil-vs-zero gating the field
// comments promise the hub — a fabricated zero for a not-yet-measured signal
// silently misinforms operators, so each gate matters.

// newBootHeartbeatCollect boots the heartbeat phase against cfg and returns
// the captured payload collector. The token collector is real but hermetic:
// its sessions dir is empty and its persist path points into the test's
// tempdir so Summary() can never restore a live /data snapshot (#4585).
func newBootHeartbeatCollect(t *testing.T, f *bootHeartbeatFake, cfg *config.Config) *boot {
	t.Helper()
	b := newBootHeartbeatBoot(t, f, cfg)
	tc := tokens.NewCollector(t.TempDir(), b.logger)
	tc.SetPersistPath(filepath.Join(t.TempDir(), "token-summary.json"))
	b.tokenCollector = tc
	b.bootHeartbeatWith(f.deps)
	if f.collect == nil {
		t.Fatal("bootHeartbeatWith did not start the heartbeat loop")
	}
	return b
}

func TestBootHeartbeatCollect_FreshBootGatesUnmeasuredSignalsToNil(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	newBootHeartbeatCollect(t, f, cfg)

	p := f.collect()
	if p == nil {
		t.Fatal("collect() with hub enabled returned nil — the spoke would never beat")
	}

	// Identity rides every beat verbatim from config.
	if p.HiveID != cfg.HiveID || p.Org != cfg.Project.Org || p.PrimaryRepo != cfg.Project.PrimaryRepo {
		t.Fatalf("identity = %q/%q/%q, want cfg values", p.HiveID, p.Org, p.PrimaryRepo)
	}
	if !sameStringSlice(p.Repos, cfg.Project.Repos) {
		t.Fatalf("Repos = %v, want %v", p.Repos, cfg.Project.Repos)
	}
	if p.Reporter == "" {
		t.Fatal("Reporter empty — the hub cannot tell two instances of one hive apart")
	}
	if _, err := time.Parse(time.RFC3339, p.StartedAt); err != nil {
		t.Fatalf("StartedAt %q is not RFC3339: %v", p.StartedAt, err)
	}
	if p.GitHubAPIURL == "" {
		t.Fatal("GitHubAPIURL must be resolved (never empty) so the hub can tell github.com from 'spoke too old'")
	}

	// Budget: limit and ignore-flag are plain governor state and always ride;
	// spend is uninterpretable without its window, and no window has opened
	// on a fresh governor, so all three of spend/start/end must be absent.
	if p.BudgetLimit == nil || p.BudgetIgnored == nil {
		t.Fatal("BudgetLimit/BudgetIgnored must always be reported")
	}
	if p.BudgetCurrentSpend != nil || p.BudgetWindowStartsAt != "" || p.BudgetWindowEndsAt != "" {
		t.Fatalf("budget spend/window reported before any window opened: spend=%v start=%q end=%q",
			p.BudgetCurrentSpend, p.BudgetWindowStartsAt, p.BudgetWindowEndsAt)
	}

	// Before the governor's first eval, false/0 are struct defaults, not
	// readings — asserting a healthy budget or clean SLA would be a lie.
	if p.BudgetExhausted != nil || p.SLAViolations != nil {
		t.Fatalf("BudgetExhausted=%v SLAViolations=%v before first eval, want nil/nil", p.BudgetExhausted, p.SLAViolations)
	}

	// No actionable scan has completed: hold must be nil, not int zero.
	if p.HoldTotal != nil {
		t.Fatalf("HoldTotal = %d before any scan, want nil", *p.HoldTotal)
	}

	// ACMM 0 has no planning subsystem — AwaitingReview must read as absent.
	if p.ACMMLevel != 0 || p.AwaitingReview != nil {
		t.Fatalf("ACMMLevel=%d AwaitingReview=%v, want 0/nil below the planning level", p.ACMMLevel, p.AwaitingReview)
	}

	// Collectors that have never computed: nil pointers / empty timestamps so
	// the hub carries the last real value forward instead of aggregating zeros.
	if p.PRsMerged90d != nil || p.PRsRejected90d != nil || p.CVEsClosed != nil || p.FleetStatsCollectedAt != "" {
		t.Fatal("fleet-stat counts reported before the collector's first successful compute")
	}
	if p.RepoActivity != nil || p.RepoActivityCollectedAt != "" {
		t.Fatal("repo activity reported before the activity collector's first snapshot")
	}
	if p.AgentErrorStreaks != nil {
		t.Fatal("AgentErrorStreaks must be nil until a bob-sessions scan completes ('not measured', never 'no failures')")
	}
	if p.TasksCompleted7d != nil {
		t.Fatalf("TasksCompleted7d = %d with no contributor store, want nil", *p.TasksCompleted7d)
	}

	// Always-live measurements: present even on a fresh boot.
	if p.AgentsWithModel == nil {
		t.Fatal("AgentsWithModel must be a non-nil pointer so the hub can tell 'zero agents' from 'old spoke'")
	}
	if p.ConsentWedged == nil || p.NoCadenceAgents == nil {
		t.Fatal("ConsentWedged/NoCadenceAgents are live measurements and must send [] to clear a stale carry-forward")
	}
	if p.OpenFDs <= 0 {
		t.Fatalf("OpenFDs = %d, want a positive live gauge", p.OpenFDs)
	}

	// No dashboard token configured: no hash — and never the raw token.
	if p.DashboardTokenHash != "" {
		t.Fatalf("DashboardTokenHash = %q with no token configured, want empty", p.DashboardTokenHash)
	}
	if p.Tokens24h != 0 {
		t.Fatalf("Tokens24h = %d from an empty token collector, want 0", p.Tokens24h)
	}
	if p.Governor.Mode == "" {
		t.Fatal("Governor.Mode empty — the hub loses the spoke's operating mode")
	}
	// Work source: plain "github" is elided from the wire.
	if p.Governor.WorkSource != "" {
		t.Fatalf("Governor.WorkSource = %q for a default (github) source, want empty", p.Governor.WorkSource)
	}
	// Cluster health is gated on HIVE_CLUSTER_ID, which the fixture clears:
	// never guess health for a cluster this spoke was not told it is on.
	if p.ClusterHealth != nil {
		t.Fatalf("ClusterHealth=%v with no cluster id, want nil", p.ClusterHealth)
	}
}

func TestBootHeartbeatCollect_MeasuredStateFlowsIntoThePayload(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	acmm := 5
	cfg.ACMMLevel = &acmm
	cfg.Dashboard.AuthToken = "spoke-secret-token"
	cfg.Hub.ClusterID = "cluster-7"
	cfg.Hub.HiveType = "saas"
	cfg.Hub.IsPublic = true
	cfg.Agents = map[string]config.AgentConfig{
		"scout": {OnDemand: true},
	}
	b := newBootHeartbeatCollect(t, f, cfg)

	// A completed actionable scan: hold becomes a real int, zero included.
	b.lastActionable.Store(&github.ActionableResult{Hold: github.HoldResult{Total: 3}})

	p := f.collect()
	if p == nil {
		t.Fatal("collect() returned nil with hub enabled")
	}

	if p.HoldTotal == nil || *p.HoldTotal != 3 {
		t.Fatalf("HoldTotal = %v after a scan reporting 3 held items, want 3", p.HoldTotal)
	}

	// At ACMM >= 5 planning exists, so AwaitingReview is a measured zero, not
	// absent evidence.
	if p.ACMMLevel != 5 {
		t.Fatalf("ACMMLevel = %d, want 5", p.ACMMLevel)
	}
	if p.AwaitingReview == nil || *p.AwaitingReview != 0 {
		t.Fatalf("AwaitingReview = %v at ACMM 5 with no plans, want a measured 0", p.AwaitingReview)
	}

	// Hash only, never the raw token.
	if p.DashboardTokenHash == "" {
		t.Fatal("DashboardTokenHash empty with a token configured")
	}
	if strings.Contains(p.DashboardTokenHash, cfg.Dashboard.AuthToken) {
		t.Fatal("DashboardTokenHash leaks the raw dashboard token")
	}

	// Hub placement fields ride verbatim.
	if p.ClusterID != "cluster-7" || p.HiveType != "saas" || !p.IsPublic {
		t.Fatalf("cluster fields = %q/%q/%v, want cfg.Hub values", p.ClusterID, p.HiveType, p.IsPublic)
	}

	// The configured on-demand agent is summarised with its mode, so the hub
	// does not flag a deliberately-quiet agent as unhealthy.
	var scout *struct {
		state, mode string
	}
	for _, a := range p.Agents {
		if a.Name == "scout" {
			scout = &struct{ state, mode string }{a.State, a.Mode}
		}
	}
	if scout == nil {
		t.Fatalf("agent 'scout' missing from payload agents: %+v", p.Agents)
	}
	if scout.mode != "on_demand" {
		t.Fatalf("scout mode = %q, want on_demand", scout.mode)
	}
}

func TestBootHeartbeatCollect_RuntimeHubDisableSkipsTheBeat(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	newBootHeartbeatCollect(t, f, cfg)

	if p := f.collect(); p == nil {
		t.Fatal("collect() nil while hub enabled")
	}
	cfg.Hub.Enabled = false
	if p := f.collect(); p != nil {
		t.Fatalf("collect() = %+v after runtime hub disable, want nil so StartHeartbeat skips the beat", p)
	}
	// Re-enable without a re-boot: the same collector resumes beating.
	cfg.Hub.Enabled = true
	if p := f.collect(); p == nil {
		t.Fatal("collect() nil after runtime re-enable — the loop must not need a restart")
	}
}

func TestBootHeartbeatRestartCallback_IgnoresRestartDuringMinUptime(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	newBootHeartbeatCollect(t, f, cfg)

	// processStartedAt is this test process's start: always well under the
	// 10-minute floor, so the callback must refuse — acting here would let a
	// hub-delivered restart crash-loop a spoke that just came up.
	heartbeatCallback[spoke.RestartSpokeCallback](t, f)()
	if !strings.Contains(f.log.String(), "ignoring — this process just started") {
		t.Fatalf("restart during min-uptime not ignored/logged:\n%s", f.log.String())
	}
}

func TestBootHeartbeatUpgradeCallback_SameCommitIsANoOp(t *testing.T) {
	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	newBootHeartbeatCollect(t, f, cfg)

	// Pin the running commit for the duration of the test: the hub may name a
	// short SHA that is a prefix of our full one (or vice-versa), and treating
	// same-commit as "behind" caused a patch→403→exit crash-loop at HEAD.
	oldShort := gitShort
	gitShort = "0123456789abcdef"
	t.Cleanup(func() { gitShort = oldShort })

	heartbeatCallback[spoke.UpgradeCallback](t, f)("0123456789AB")
	if !strings.Contains(f.log.String(), "self-upgrade skipped: target is the running commit") {
		t.Fatalf("same-commit upgrade not skipped:\n%s", f.log.String())
	}
}

package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
)

// ---------------------------------------------------------------------------
// filterHoldByRepo
// ---------------------------------------------------------------------------

func TestFilterHoldByRepoSplitsTypeCountsAndDropsOtherRepos(t *testing.T) {
	hold := github.HoldResult{
		Issues: 2,
		PRs:    2,
		Total:  4,
		Items: []github.HoldItem{
			{Repo: "test-org/console", Number: 1, Type: "pr"},
			{Repo: "test-org/console", Number: 2, Type: "issue"},
			// Unknown type must count as an issue (default switch arm).
			{Repo: "test-org/console", Number: 3, Type: ""},
			{Repo: "test-org/docs", Number: 4, Type: "pr"},
		},
	}

	got := filterHoldByRepo(hold, "test-org/console")

	if len(got.Items) != 3 {
		t.Fatalf("Items = %v, want the 3 console items", got.Items)
	}
	for _, item := range got.Items {
		if item.Repo != "test-org/console" {
			t.Fatalf("leaked item from %q: %+v", item.Repo, item)
		}
	}
	if got.PRs != 1 || got.Issues != 2 || got.Total != 3 {
		t.Fatalf("counts = PRs %d / Issues %d / Total %d, want 1/2/3",
			got.PRs, got.Issues, got.Total)
	}
}

func TestFilterHoldByRepoNoMatchesYieldsEmptyZeroCounts(t *testing.T) {
	hold := github.HoldResult{
		Issues: 1, PRs: 1, Total: 2,
		Items: []github.HoldItem{
			{Repo: "test-org/docs", Number: 1, Type: "pr"},
			{Repo: "test-org/docs", Number: 2, Type: "issue"},
		},
	}

	got := filterHoldByRepo(hold, "test-org/console")

	if len(got.Items) != 0 || got.PRs != 0 || got.Issues != 0 || got.Total != 0 {
		t.Fatalf("filterHoldByRepo(no-match) = %+v, want empty zero-count result", got)
	}
}

// ---------------------------------------------------------------------------
// actionableForRepo
// ---------------------------------------------------------------------------

func TestActionableForRepoPassesThroughNilAndUnscoped(t *testing.T) {
	if got := actionableForRepo(nil, "test-org/console"); got != nil {
		t.Fatalf("actionableForRepo(nil, repo) = %+v, want nil", got)
	}
	actionable := &github.ActionableResult{}
	if got := actionableForRepo(actionable, ""); got != actionable {
		t.Fatalf("actionableForRepo(a, \"\") = %p, want the same pointer %p", got, actionable)
	}
}

func TestActionableForRepoNarrowsHoldAndTotals(t *testing.T) {
	actionable := &github.ActionableResult{
		Hold: github.HoldResult{
			Items: []github.HoldItem{
				{Repo: "test-org/console", Number: 10, Type: "pr"},
				{Repo: "test-org/docs", Number: 11, Type: "issue"},
			},
		},
		TotalByRepo: map[string]github.RepoCounts{
			"test-org/console": {Issues: 3, PRs: 1},
			"test-org/docs":    {Issues: 5, PRs: 2},
		},
	}

	got := actionableForRepo(actionable, "test-org/console")

	if len(got.Hold.Items) != 1 || got.Hold.Items[0].Number != 10 {
		t.Fatalf("Hold.Items = %v, want only console item #10", got.Hold.Items)
	}
	if got.Hold.PRs != 1 || got.Hold.Issues != 0 || got.Hold.Total != 1 {
		t.Fatalf("Hold counts = %+v, want PRs 1 / Issues 0 / Total 1", got.Hold)
	}
	if len(got.TotalByRepo) != 1 {
		t.Fatalf("TotalByRepo = %v, want only the scoped repo", got.TotalByRepo)
	}
	if counts := got.TotalByRepo["test-org/console"]; counts.Issues != 3 || counts.PRs != 1 {
		t.Fatalf("TotalByRepo[console] = %+v, want Issues 3 / PRs 1", counts)
	}
	// The input snapshot must not be mutated by the narrowing copy.
	if len(actionable.Hold.Items) != 2 || len(actionable.TotalByRepo) != 2 {
		t.Fatalf("input actionable mutated: %+v", actionable)
	}
}

// ---------------------------------------------------------------------------
// addCanaryPreamble
// ---------------------------------------------------------------------------

// newSchedulerWithCanaries builds a scheduler with ioscan and canaries set
// explicitly, mirroring newSchedulerWithIoscan but driving the Canaries flag.
func newSchedulerWithCanaries(ioscanEnabled, canaries bool) *Scheduler {
	e := ioscanEnabled
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console"}},
		Ioscan:  config.IoscanConfig{Enabled: &e, Canaries: &canaries},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

// swapDefaultCanaries points ioscan.DefaultCanaries at a registry persisted in
// a temp dir so the test never writes the production canary file, and restores
// the original on cleanup.
func swapDefaultCanaries(t *testing.T) *ioscan.CanaryRegistry {
	t.Helper()
	orig := ioscan.DefaultCanaries
	reg := ioscan.NewCanaryRegistry(filepath.Join(t.TempDir(), "canaries.json"))
	ioscan.DefaultCanaries = reg
	t.Cleanup(func() { ioscan.DefaultCanaries = orig })
	return reg
}

func TestAddCanaryPreambleDisabledOrEmptyMessageIsUnchanged(t *testing.T) {
	reg := swapDefaultCanaries(t)

	// Canaries flag off → message passes through untouched.
	s := newSchedulerWithCanaries(true, false)
	if got := s.addCanaryPreamble("scanner", "kick body"); got != "kick body" {
		t.Fatalf("canaries-off preamble = %q, want unchanged message", got)
	}

	// Ioscan disabled entirely → same pass-through even with canaries on.
	s = newSchedulerWithCanaries(false, true)
	if got := s.addCanaryPreamble("scanner", "kick body"); got != "kick body" {
		t.Fatalf("ioscan-off preamble = %q, want unchanged message", got)
	}

	// Empty message must never grow a preamble.
	s = newSchedulerWithCanaries(true, true)
	if got := s.addCanaryPreamble("scanner", ""); got != "" {
		t.Fatalf("empty-message preamble = %q, want empty", got)
	}

	// None of the pass-through paths may have minted a canary.
	if _, leaked := reg.Scan("scanner", ioscan.CanaryPrefix, "test"); leaked {
		t.Fatalf("pass-through paths registered a canary")
	}
}

func TestAddCanaryPreambleEnabledPrependsRegisteredToken(t *testing.T) {
	reg := swapDefaultCanaries(t)
	s := newSchedulerWithCanaries(true, true)

	const msg = "kick body"
	got := s.addCanaryPreamble("scanner", msg)

	if !strings.HasPrefix(got, "SECURITY CANARY: "+ioscan.CanaryPrefix) {
		t.Fatalf("preamble missing canary header: %q", got)
	}
	if !strings.HasSuffix(got, msg) {
		t.Fatalf("original message lost: %q", got)
	}

	// The token planted in the message must be registered for this agent so
	// the output scanner can attribute a leak.
	leak, found := reg.Scan("", got, "test")
	if !found || leak.Agent != "scanner" {
		t.Fatalf("Scan(preambled message) = %+v found=%v, want scanner-attributed leak", leak, found)
	}
}

package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/writeback"
)

const testForgejoTokenEnv = "HIVE_TEST_FORGEJO_SWEEP_TOKEN"

func forgejoSweepTestConfig() *config.Config {
	level := config.SelfMergeMinACMMLevel
	cfg := &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org:   "acme",
			Forge: config.ForgeForgejo,
			Repos: []string{"widgets", "gadgets"},
			RepoPolicies: []config.RepoPolicy{
				{Repo: "gadgets", Forge: config.ForgeGitea, ForgeURL: "https://gitea.example.com"},
			},
		},
		Forgejo: config.ForgejoConfig{URL: "https://forgejo.example.com", ForgeTokens: cruxconfig.ForgeTokens{TokenSource: cruxconfig.TokenSource{TokenEnv: testForgejoTokenEnv}}},
	}
	cfg.Governor.Labels.AutoMerge = "lgtm"
	cfg.Governor.Labels.Exempt = []string{"meta-tracker"}
	return cfg
}

// stubForgejoSweepAPI replaces the client constructor and records what it was
// built with.
func stubForgejoSweepAPI(t *testing.T, api forge.SweepAPI, err error) *[]forge.Options {
	t.Helper()
	var built []forge.Options
	prev := newForgejoSweepAPI
	newForgejoSweepAPI = func(token string, opts forge.Options) (forge.SweepAPI, error) {
		if token != "fj-secret" {
			t.Errorf("token = %q", token)
		}
		built = append(built, opts)
		return api, err
	}
	t.Cleanup(func() { newForgejoSweepAPI = prev })
	return &built
}

// sweepAPIStub answers every sweep read with "nothing to do".
type sweepAPIStub struct {
	forge.SweepAPI
	listed int
}

func (s *sweepAPIStub) ListOpenChangeRequests(context.Context, string) ([]forge.ChangeRequest, error) {
	s.listed++
	return nil, nil
}
func (s *sweepAPIStub) Self(context.Context) (string, error) { return "hive-bot", nil }
func (s *sweepAPIStub) RecentlyMergedPulls(context.Context, string, int) ([]forge.MergedPull, error) {
	return nil, nil
}

func TestForgejoSweepAPIForRepo(t *testing.T) {
	cfg := forgejoSweepTestConfig()
	built := stubForgejoSweepAPI(t, &sweepAPIStub{}, nil)

	if _, err := forgejoSweepAPIForRepo(cfg, "gadgets"); err == nil || !strings.Contains(err.Error(), "not on forgejo") {
		t.Errorf("gitea repo: err = %v", err)
	}
	t.Setenv(testForgejoTokenEnv, "")
	if _, err := forgejoSweepAPIForRepo(cfg, "widgets"); err == nil || !strings.Contains(err.Error(), testForgejoTokenEnv) {
		t.Errorf("no token: err = %v", err)
	}
	t.Setenv(testForgejoTokenEnv, "fj-secret")
	if _, err := forgejoSweepAPIForRepo(cfg, "widgets"); err != nil {
		t.Fatalf("widgets: %v", err)
	}
	if len(*built) != 1 || (*built)[0].BaseURL != "https://forgejo.example.com" || (*built)[0].Org != "acme" {
		t.Errorf("built = %+v", *built)
	}

	// A repo_policies forge_url on another host would send the forgejo
	// token there: refused before any client is built.
	cfg.Project.RepoPolicies = append(cfg.Project.RepoPolicies, config.RepoPolicy{Repo: "widgets", ForgeURL: "https://evil.example.com"})
	if _, err := forgejoSweepAPIForRepo(cfg, "widgets"); err == nil {
		t.Error("foreign forge_url host: want an error")
	}
	if len(*built) != 1 {
		t.Errorf("a client was built for a foreign host: %+v", *built)
	}
}

func TestForgejoSweeperFor(t *testing.T) {
	if forgejoSweeperFor(nil, forgejoSweepDeps{}) != nil {
		t.Error("nil config: want nil")
	}
	gh := &config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widgets"}}}
	if forgejoSweeperFor(gh, forgejoSweepDeps{}) != nil {
		t.Error("GitHub hive: want no Forgejo sweeper")
	}
	cfg := forgejoSweepTestConfig()
	cfg.AutoMerge.MaxMerges = 3
	s := forgejoSweeperFor(cfg, forgejoSweepDeps{})
	if s == nil {
		t.Fatal("want a sweeper")
	}
	if len(s.Repos) != 1 || s.Repos[0] != "widgets" {
		t.Errorf("Repos = %v, want only the forgejo repo", s.Repos)
	}
	if s.AutoMergeLabel != "lgtm" || s.MaxMerges != 3 || !s.AutoMergeEnabled("widgets") {
		t.Errorf("sweeper = %+v", s)
	}
	joined := strings.Join(s.BlockingLabels, ",")
	for _, want := range []string{forgejoHoldLabel, cfg.Sentinel.LabelOrDefault(), cfg.Governor.Labels.DecisionNeededLabelOrDefault(), "meta-tracker"} {
		if !strings.Contains(joined, want) {
			t.Errorf("BlockingLabels %v lacks %q", s.BlockingLabels, want)
		}
	}
	if s.Gate != nil {
		t.Error("no write-back gate given: want a nil Gate")
	}
	t.Setenv(testForgejoTokenEnv, "fj-secret")
	stubForgejoSweepAPI(t, &sweepAPIStub{}, nil)
	if _, err := s.API("widgets"); err != nil {
		t.Errorf("API: %v", err)
	}

	gate := writeback.New(writeback.Config{}, writeback.Deps{}, filepath.Join(t.TempDir(), "writeback.json"))
	gated := forgejoSweeperFor(cfg, forgejoSweepDeps{Gate: gate})
	if gated.Gate == nil {
		t.Fatal("write-back gate given: want Gate set")
	}
	// The hive's own issue is never gated (outside per-repo mode off).
	if d := gated.Gate("widgets", writeback.Item{Number: 5, HiveAuthored: true}, writeback.KindIssueClose, "close"); !d.Allowed {
		t.Errorf("own item refused: %+v", d)
	}
}

func TestRunForgejoSweepPass(t *testing.T) {
	t.Setenv(testForgejoTokenEnv, "fj-secret")
	stub := &sweepAPIStub{}
	stubForgejoSweepAPI(t, stub, nil)
	cfg := forgejoSweepTestConfig()
	if m, c := runForgejoSweepPass(context.Background(), nil, forgejoSweepDeps{}); m != nil || c != nil {
		t.Errorf("nil config: %v %v", m, c)
	}
	runForgejoSweepPass(context.Background(), cfg, forgejoSweepDeps{})
	if stub.listed != 1 {
		t.Errorf("listed = %d, want one automerge listing", stub.listed)
	}
	off := false
	cfg.Issues.CloseOnMerge = &off
	runForgejoSweepPass(context.Background(), cfg, forgejoSweepDeps{})
	if stub.listed != 2 {
		t.Errorf("listed = %d", stub.listed)
	}
}

func TestSafeForgejoSweepPassRecovers(t *testing.T) {
	t.Setenv(testForgejoTokenEnv, "fj-secret")
	// A nil API inside the stub panics on the first call.
	stubForgejoSweepAPI(t, &panicAPI{}, nil)
	_, _, err := safeForgejoSweepPass(context.Background(), forgejoSweepTestConfig(), forgejoSweepDeps{})
	if err == nil {
		t.Error("want the panic reported as an error")
	}
	stubForgejoSweepAPI(t, nil, errors.New("down"))
	if _, _, err := safeForgejoSweepPass(context.Background(), forgejoSweepTestConfig(), forgejoSweepDeps{}); err != nil {
		t.Errorf("client error must not panic: %v", err)
	}
}

type panicAPI struct{ forge.SweepAPI }

func TestStartForgejoSweepsRunsAndStops(t *testing.T) {
	t.Setenv(testForgejoTokenEnv, "fj-secret")
	stub := &sweepAPIStub{}
	stubForgejoSweepAPI(t, stub, nil)
	cfg := forgejoSweepTestConfig()
	ctx, cancel := context.WithCancel(context.Background())
	passes := make(chan struct{}, 8)
	done := startForgejoSweeps(ctx, func() *config.Config {
		select {
		case passes <- struct{}{}:
		default:
		}
		return cfg
	}, forgejoSweepDeps{}, time.Millisecond)
	select {
	case <-passes:
	case <-time.After(5 * time.Second):
		t.Fatal("no sweep pass ran")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep loop did not stop")
	}
	// A zero interval falls back to the default and still stops.
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := startForgejoSweeps(ctx2, func() *config.Config { return nil }, forgejoSweepDeps{}, 0)
	cancel2()
	<-done2
}

func TestStartForgejoSweepsLogsPanic(t *testing.T) {
	t.Setenv(testForgejoTokenEnv, "fj-secret")
	stubForgejoSweepAPI(t, &panicAPI{}, nil)
	cfg := forgejoSweepTestConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan struct{}, 1)
	done := startForgejoSweeps(ctx, func() *config.Config {
		select {
		case ran <- struct{}{}:
		default:
		}
		return cfg
	}, forgejoSweepDeps{Logger: slog.New(slog.DiscardHandler)}, time.Millisecond)
	<-ran
	cancel()
	<-done
}

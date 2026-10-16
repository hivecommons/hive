package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence/complexity"
	"github.com/hivecommons/hive/pkg/github"
)

func complexityTestConfig(enabled bool) *config.Config {
	cfg := &config.Config{}
	cfg.Project.Org = "o"
	cfg.Project.Repos = []string{"a", "x/b"}
	cfg.Evidence.Complexity.Enabled = &enabled
	return cfg
}

func TestComplexityRepos(t *testing.T) {
	got := complexityRepos(complexityTestConfig(true))
	if len(got) != 2 || got[0] != "o/a" || got[1] != "x/b" {
		t.Fatalf("repos = %v", got)
	}
	if complexityRepos(nil) != nil {
		t.Error("nil config must yield nil")
	}
}

func TestNewComplexityRunner(t *testing.T) {
	noClient := func() *github.Client { return nil }
	if r := newComplexityRunner(nil, noClient, nil); r != nil {
		t.Error("nil config must disable the runner")
	}
	if r := newComplexityRunner(complexityTestConfig(false), noClient, nil); r != nil {
		t.Error("evidence.complexity.enabled=false must disable the runner")
	}
	cfg := complexityTestConfig(true)
	r := newComplexityRunner(cfg, noClient, slog.Default())
	if r == nil {
		t.Fatal("default config must enable the runner")
	}
	if r.Root != github.ReviewEvidenceRoot || len(r.Analyzers) != 0 {
		t.Errorf("runner = %+v", r)
	}
	if got := r.Repos(); len(got) != 2 {
		t.Errorf("repos = %v", got)
	}
	// Without a GitHub client the source fails cleanly instead of panicking.
	if _, err := r.Source.Fetch(context.Background(), "o/a"); err == nil {
		t.Error("nil client must surface an error")
	}
	cfg.Evidence.Complexity.StaticAnalysis = true
	if r := newComplexityRunner(cfg, noClient, nil); len(r.Analyzers) != len(complexity.DefaultAnalyzers()) {
		t.Errorf("static_analysis=true must install the default analyzers, got %d", len(r.Analyzers))
	}
}

func TestStartComplexityRunnerShutdownHook(t *testing.T) {
	b := &boot{ctx: context.Background()}
	b.startComplexityRunner(nil)
	if b.preShutdownHooks.count() != 0 {
		t.Fatal("nil runner must not register a hook")
	}
	r := &complexity.Runner{
		Root:   t.TempDir(),
		Repos:  func() []string { return nil },
		Source: complexity.SourceFunc(func(context.Context, string) (complexity.Checkout, error) { return complexity.Checkout{}, nil }),
	}
	b.startComplexityRunner(r)
	if b.preShutdownHooks.count() != 1 {
		t.Fatalf("hooks = %d", b.preShutdownHooks.count())
	}
	// The hook stops the loop and returns only once it has exited.
	b.preShutdownHooks.run()
}

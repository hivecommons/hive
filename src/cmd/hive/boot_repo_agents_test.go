package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/repoagents"
)

// pauseRecorder records PauseBy calls; every other runtime method is unused
// by the code under test.
type pauseRecorder struct {
	hostedRuntime
	paused  []string
	reasons []string
	err     error
}

func (p *pauseRecorder) PauseBy(name, trigger, reason, by string) error {
	p.paused = append(p.paused, name+"|"+trigger+"|"+by)
	p.reasons = append(p.reasons, reason)
	return p.err
}

func TestManagedRepoSlugs(t *testing.T) {
	if managedRepoSlugs(nil) != nil {
		t.Fatal("nil config should give nil")
	}
	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widgets", "other/gadgets", " "}}}
	if got, want := managedRepoSlugs(cfg), []string{"acme/widgets", "other/gadgets"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("slugs = %v, want %v", got, want)
	}
}

func TestRepoAgentsInterval(t *testing.T) {
	var logs bytes.Buffer
	b := &boot{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	t.Setenv(repoAgentsIntervalEnv, "")
	if got := repoAgentsInterval(b); got != repoagents.DefaultInterval {
		t.Errorf("unset = %v", got)
	}
	t.Setenv(repoAgentsIntervalEnv, "5m")
	if got := repoAgentsInterval(b); got != 5*time.Minute {
		t.Errorf("5m = %v", got)
	}
	t.Setenv(repoAgentsIntervalEnv, "soon")
	if got := repoAgentsInterval(b); got != repoagents.DefaultInterval || logs.Len() == 0 {
		t.Errorf("invalid = %v, logged=%d", got, logs.Len())
	}
	t.Setenv(repoAgentsIntervalEnv, "-1m")
	if got := repoAgentsInterval(&boot{}); got != repoagents.DefaultInterval {
		t.Errorf("negative = %v", got)
	}
}

func TestPauseOrphanedRepoAgent(t *testing.T) {
	src := &config.DefinitionSourceConfig{Owner: "acme", Repo: "widgets", Path: ".hive/agents/foo.yaml", Discovered: true}
	cfg := &config.Config{}
	cfg.Agents = map[string]config.AgentConfig{
		"foo":   {DefinitionSource: src},
		"moved": {DefinitionSource: &config.DefinitionSourceConfig{Owner: "acme", Repo: "widgets", Path: ".hive/agents/other.yaml"}},
		"plain": {},
	}
	rec := &pauseRecorder{}
	var logs bytes.Buffer
	b := &boot{cfg: cfg, agentMgr: rec, logger: slog.New(slog.NewTextHandler(&logs, nil))}

	entry := repoagents.Entry{Repo: "acme/widgets", Path: ".hive/agents/foo.yaml", Agent: "foo"}
	b.pauseOrphanedRepoAgent(entry)
	if len(rec.paused) != 1 || rec.paused[0] != "foo|"+repoAgentsOrphanTrigger+"|"+repoAgentsOrphanTrigger {
		t.Fatalf("paused = %v", rec.paused)
	}

	// Agents that no longer point at the removed file, or are unknown, stay as they are.
	for _, e := range []repoagents.Entry{
		{Repo: "acme/widgets", Path: ".hive/agents/foo.yaml", Agent: "moved"},
		{Repo: "acme/widgets", Path: ".hive/agents/foo.yaml", Agent: "plain"},
		{Repo: "acme/widgets", Path: ".hive/agents/foo.yaml", Agent: "gone"},
		{Repo: "acme/widgets", Path: ".hive/agents/foo.yaml"},
	} {
		b.pauseOrphanedRepoAgent(e)
	}
	if len(rec.paused) != 1 {
		t.Fatalf("unexpected pauses: %v", rec.paused)
	}

	rec.err = errors.New("boom")
	b.pauseOrphanedRepoAgent(entry)
	if logs.Len() == 0 {
		t.Error("pause failure should be logged")
	}
	(&boot{}).pauseOrphanedRepoAgent(entry) // no runtime: no-op
}

func TestNoteRepoAgentPush(t *testing.T) {
	b := &boot{}
	b.noteRepoAgentPush("push", []byte(`{}`)) // no discoverer: no-op

	b.repoAgents = repoagents.New(repoagents.Options{})
	push := []byte(`{"ref":"refs/heads/main","repository":{"full_name":"acme/widgets","default_branch":"main"},"commits":[{"added":[".hive/agents/foo.yaml"]}]}`)
	b.noteRepoAgentPush("issues", push)
	b.noteRepoAgentPush("push", []byte(`{"ref":"refs/heads/feature","repository":{"full_name":"acme/widgets","default_branch":"main"}}`))
	b.noteRepoAgentPush("push", push)
	// Exactly one rescan was queued: fill the rest of the buffer and count.
	queued := 1
	for b.repoAgents.Trigger("x/y") {
		queued++
	}
	const wantQueued = 16
	if queued != wantQueued {
		t.Fatalf("queued = %d, want %d (one push rescan plus fillers)", queued, wantQueued)
	}
}

func TestWireRepoAgentDiscovery(t *testing.T) {
	(&boot{}).wireRepoAgentDiscovery() // no config, client or ctx: off

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Setenv(repoAgentsStateEnv, filepath.Join(t.TempDir(), "repo-agents.json"))
	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widgets"}}}
	b := &boot{ctx: ctx, cfg: cfg, ghClient: fakeGitHubClient(t), logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	b.wireRepoAgentDiscovery()
	if b.repoAgents == nil {
		t.Fatal("discoverer not wired")
	}
	if got := b.repoAgents.List(); len(got) != 0 {
		t.Fatalf("a repo off the allowlist was scanned: %+v", got)
	}
}

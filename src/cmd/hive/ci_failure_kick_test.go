package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

type ciKickRecord struct {
	agent, message string
}

func newCIFailureKickerForTest(t *testing.T, cfg *config.Config) (*ciFailureKicker, *[]ciKickRecord) {
	t.Helper()
	var sent []ciKickRecord
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	k := &ciFailureKicker{
		cfg:       func() *config.Config { return cfg },
		gov:       governor.New(cfg.Governor, cfg.EnabledAgents(), logger),
		attribute: func(repo string, number int) string { return "quality" },
		message: func(agent string, sig github.CIFailureSignal) string {
			return "CI failed on " + sig.Repo + " for " + agent
		},
		send: func(_ context.Context, agent, message string) error {
			sent = append(sent, ciKickRecord{agent: agent, message: message})
			return nil
		},
		logger: logger,
		ctx:    context.Background(),
		spawn:  func(fn func()) { fn() },
	}
	return k, &sent
}

func ciKickTestConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Org: "acme", Repos: []string{"widgets"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"quality": {Enabled: true, Mode: "ISSUES_AND_PRS"},
				"scanner": {Enabled: true, Mode: "ISSUES_AND_PRS"},
			},
		},
	}
}

// The webhook kick honours every gate the CEL trigger path honours — flag,
// config pause, runtime pause, repo pause, governor eligibility — plus the
// per-head cooldown, and otherwise delivers one targeted kick to the agent
// the audit log attributes the PR to (scanner when unattributed).
func TestCIFailureKicker_GatesAndKicks(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	t.Cleanup(github.ResetCIFailureSignalsForTest(clock))
	cfg := ciKickTestConfig()
	k, sent := newCIFailureKickerForTest(t, cfg)
	sig := github.CIFailureSignal{Repo: "acme/widgets", Number: 7, HeadSHA: "abc", Check: "build"}

	out := k.React(sig)
	if !out.Kicked || out.Agent != "quality" || len(*sent) != 1 || (*sent)[0].agent != "quality" {
		t.Fatalf("first kick: %+v, sent = %+v", out, *sent)
	}
	if got := github.CIFailureSignals().Snapshot().Kicks; got != 1 {
		t.Fatalf("kicks recorded = %d", got)
	}

	// Same head again inside the cooldown: refused.
	if out := k.React(sig); out.Kicked || out.Reason != "cooldown" {
		t.Fatalf("cooldown: %+v", out)
	}
	// New head: a fresh kick.
	sig2 := sig
	sig2.HeadSHA = "def"
	if out := k.React(sig2); !out.Kicked || len(*sent) != 2 {
		t.Fatalf("new head: %+v, sent = %d", out, len(*sent))
	}
	// Past the cooldown the original head may kick again.
	now = now.Add(cfg.CIFailureKickCooldown() + time.Second)
	if out := k.React(sig); !out.Kicked || len(*sent) != 3 {
		t.Fatalf("after cooldown: %+v, sent = %d", out, len(*sent))
	}

	sig3 := sig
	sig3.HeadSHA = "ghi"
	ac := cfg.Agents["quality"]
	ac.Paused = true
	cfg.Agents["quality"] = ac
	if out := k.React(sig3); out.Kicked || out.Reason != "agent paused" {
		t.Fatalf("config pause: %+v", out)
	}
	ac.Paused = false
	cfg.Agents["quality"] = ac

	k.isPaused = func(string) bool { return true }
	if out := k.React(sig3); out.Kicked || out.Reason != "agent paused" {
		t.Fatalf("runtime pause: %+v", out)
	}
	k.isPaused = nil

	cfg.Project.PausedRepos = []config.RepoPause{{Repo: "widgets"}}
	if out := k.React(sig3); out.Kicked || out.Reason != "repo paused" {
		t.Fatalf("repo pause: %+v", out)
	}
	cfg.Project.PausedRepos = nil

	off := false
	cfg.Governor.CIFailureWebhookKick = &off
	if out := k.React(sig3); out.Kicked || out.Reason != "disabled" {
		t.Fatalf("flag off: %+v", out)
	}
	cfg.Governor.CIFailureWebhookKick = nil

	k.message = func(string, github.CIFailureSignal) string { return "" }
	if out := k.React(sig3); out.Kicked || out.Reason != "nothing to repair" {
		t.Fatalf("empty message: %+v", out)
	}
	k.message = func(agent string, _ github.CIFailureSignal) string { return "m" }

	// Unattributed PR routes to scanner; an unknown agent is not kicked.
	k.attribute = func(string, int) string { return "" }
	if out := k.React(sig3); !out.Kicked || out.Agent != ciFailureKickDefaultAgent {
		t.Fatalf("default agent: %+v", out)
	}
	k.attribute = func(string, int) string { return "ghost" }
	sig4 := sig
	sig4.HeadSHA = "jkl"
	if out := k.React(sig4); out.Kicked || out.Reason != "unknown agent" {
		t.Fatalf("unknown agent: %+v", out)
	}

	// A delivery failure is logged, not surfaced as a kick.
	k.attribute = func(string, int) string { return "quality" }
	k.send = func(context.Context, string, string) error { return errors.New("pane gone") }
	sig5 := sig
	sig5.HeadSHA = "mno"
	if out := k.React(sig5); !out.Kicked {
		t.Fatalf("send failure must still report dispatch: %+v", out)
	}
	if len(*sent) != 4 {
		t.Fatalf("sent = %d, want 4", len(*sent))
	}
}

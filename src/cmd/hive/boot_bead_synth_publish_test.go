package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func TestBeadSynthPublisherSettings(t *testing.T) {
	if _, _, ok := beadSynthPublisherSettings(nil); ok {
		t.Fatal("nil config must not enable publishing")
	}
	cfg := &config.Config{}
	if _, _, ok := beadSynthPublisherSettings(cfg); ok {
		t.Fatal("disabled publish block must not enable publishing")
	}

	cfg.Project = config.ProjectConfig{Org: "acme", Repos: []string{"first", "second"}}
	cfg.Knowledge.BeadSynthesizer.TargetLayer = "project"
	cfg.Knowledge.BeadSynthesizer.Publish.Enabled = true
	pc, cadence, ok := beadSynthPublisherSettings(cfg)
	if !ok || pc.Repo != "first" || pc.Org != "acme" || pc.Path != config.DefaultBeadSynthPublishPath ||
		pc.Layer != "project" || cadence != config.DefaultBeadSynthPublishCadence {
		t.Fatalf("defaults: %+v cadence=%q ok=%v", pc, cadence, ok)
	}

	cfg.Project.PrimaryRepo = "primary"
	if pc, _, _ := beadSynthPublisherSettings(cfg); pc.Repo != "primary" {
		t.Fatalf("primary repo must win over the first repo: %q", pc.Repo)
	}

	cfg.Knowledge.BeadSynthesizer.Publish = config.BeadSynthPublishConfig{
		Enabled: true, Repo: "other/docs", Path: ".hive/knowledge/", BaseBranch: "dev", Cadence: "daily",
	}
	pc, cadence, _ = beadSynthPublisherSettings(cfg)
	if pc.Repo != "other/docs" || pc.Path != ".hive/knowledge/" || pc.BaseBranch != "dev" || cadence != "daily" {
		t.Fatalf("explicit settings: %+v cadence=%q", pc, cadence)
	}
}

func newBeadSynthPublishBoot(cfg *config.Config) (*boot, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	return &boot{
		cfg:       cfg,
		logger:    logger,
		beadSynth: knowledge.NewBeadSynthesizer(nil, nil, knowledge.BeadSynthesizerConfig{}, logger, nil),
	}, &buf
}

func TestInstallBeadSynthPublisher(t *testing.T) {
	(&boot{}).installBeadSynthPublisher()

	cfg := &config.Config{}
	cfg.Project = config.ProjectConfig{Org: "acme", PrimaryRepo: "widgets"}
	b, logs := newBeadSynthPublishBoot(cfg)
	b.installBeadSynthPublisher()
	if logs.Len() != 0 {
		t.Fatalf("disabled publishing must stay silent: %s", logs)
	}

	cfg.Knowledge.BeadSynthesizer.Publish = config.BeadSynthPublishConfig{Enabled: true, Mode: "commit"}
	b, logs = newBeadSynthPublishBoot(cfg)
	b.installBeadSynthPublisher()
	if !strings.Contains(logs.String(), "publishing via PR") || !strings.Contains(logs.String(), "bead-synth publish enabled") {
		t.Fatalf("commit mode must fall back to PR and install: %s", logs)
	}

	cfg.Knowledge.BeadSynthesizer.Publish = config.BeadSynthPublishConfig{Enabled: true, Cadence: "weekly"}
	b, logs = newBeadSynthPublishBoot(cfg)
	b.installBeadSynthPublisher()
	if !strings.Contains(logs.String(), "bead-synth publish disabled") {
		t.Fatalf("an invalid cadence must disable publishing: %s", logs)
	}

	cfg.Project = config.ProjectConfig{}
	cfg.Knowledge.BeadSynthesizer.Publish = config.BeadSynthPublishConfig{Enabled: true}
	b, logs = newBeadSynthPublishBoot(cfg)
	b.installBeadSynthPublisher()
	if !strings.Contains(logs.String(), "bead-synth publish disabled") {
		t.Fatalf("no repository must disable publishing: %s", logs)
	}
}

func TestLiveBeadSynthRepoClient_NoClient(t *testing.T) {
	ctx := context.Background()
	for name, l := range map[string]liveBeadSynthRepoClient{
		"nil provider": {},
		"nil client":   {current: func() *github.Client { return nil }},
	} {
		calls := []error{}
		_, err := l.DefaultBranch(ctx, "o", "r")
		calls = append(calls, err)
		_, _, _, err = l.OpenPRForHead(ctx, "o/r", "b")
		calls = append(calls, err)
		_, err = l.GetFileContentRef(ctx, "o", "r", "p", "main")
		calls = append(calls, err)
		_, err = l.LatestCommitHash(ctx, "o", "r", "b")
		calls = append(calls, err)
		_, err = l.ResetBranch(ctx, "o/r", "b", "main")
		calls = append(calls, err)
		_, err = l.CommitFilesOnBranch(ctx, "o/r", "b", "h", "x", "", map[string]string{"a": "a"})
		calls = append(calls, err)
		_, err = l.CreatePR(ctx, "o/r", "b", "main", "t", "")
		calls = append(calls, err)
		for i, err := range calls {
			if !errors.Is(err, github.ErrNoGitHubClient) {
				t.Errorf("%s: call %d: got %v", name, i, err)
			}
		}
	}
}

func TestLiveBeadSynthRepoClient_DelegatesToCurrentClient(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	client := github.NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	l := liveBeadSynthRepoClient{current: func() *github.Client { return client }}
	ctx := context.Background()

	if _, err := l.DefaultBranch(ctx, "o", "r"); err == nil {
		t.Error("DefaultBranch")
	}
	if _, _, _, err := l.OpenPRForHead(ctx, "o/r", "b"); err == nil {
		t.Error("OpenPRForHead")
	}
	if _, err := l.GetFileContentRef(ctx, "o", "r", "p", "main"); err == nil {
		t.Error("GetFileContentRef")
	}
	if _, err := l.LatestCommitHash(ctx, "o", "r", "b"); err == nil {
		t.Error("LatestCommitHash")
	}
	if _, err := l.ResetBranch(ctx, "o/r", "b", "main"); err == nil {
		t.Error("ResetBranch")
	}
	if _, err := l.CommitFilesOnBranch(ctx, "o/r", "b", "h", "x", "", map[string]string{"a": "a"}); err == nil {
		t.Error("CommitFilesOnBranch")
	}
	if _, err := l.CreatePR(ctx, "o/r", "b", "main", "", ""); err == nil {
		t.Error("CreatePR")
	}
	if hits.Load() == 0 {
		t.Fatal("calls must reach the current client")
	}
}

package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatcher_ReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")

	initial := minimalValidYAML("original-org", "ghp_tok")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32
	var lastOrg atomic.Value

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	w := NewWatcher(path, func(cfg *Config) {
		reloadCount.Add(1)
		lastOrg.Store(cfg.Project.Org)
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Start(ctx)

	// Give the watcher time to start
	time.Sleep(100 * time.Millisecond)

	// Modify the file
	updated := minimalValidYAML("updated-org", "ghp_tok")
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}

	// Wait for the debounced reload to deliver the *new* content.
	//
	// Poll on the observable outcome (lastOrg) rather than on reloadCount.
	// The callback increments reloadCount before it stores lastOrg, so a
	// loop that exits on reloadCount > 0 can observe the counter in the
	// window between those two statements and then read a still-unset
	// lastOrg — yielding org == "" and a spurious failure. That window is
	// tiny locally but is readily hit under -race on a loaded CI runner.
	const waitForReload = 10 * time.Second
	const pollInterval = 10 * time.Millisecond
	deadline := time.Now().Add(waitForReload)
	var org string
	for time.Now().Before(deadline) {
		if v, ok := lastOrg.Load().(string); ok && v == "updated-org" {
			org = v
			break
		}
		time.Sleep(pollInterval)
	}

	if reloadCount.Load() == 0 {
		t.Fatal("expected at least one reload after file change")
	}

	if org != "updated-org" {
		last, _ := lastOrg.Load().(string)
		t.Errorf("expected org = %q after reload, got %q (reloads=%d)",
			"updated-org", last, reloadCount.Load())
	}
}

func TestSaveObserverSkipsProgrammaticSaveAndAllowsNextExternalReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("initial-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}

	origRuntime := RuntimeConfigFile
	RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	t.Cleanup(func() { RuntimeConfigFile = origRuntime })
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	restoreSAToken := SetSATokenFileForTest(filepath.Join(dir, "no-such-sa-token"))
	t.Cleanup(restoreSAToken)

	reloaded := make(chan string, 1)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWatcher(path, func(cfg *Config) {
		reloaded <- cfg.Project.Org
	}, logger)

	SetSaveObserver(w.SkipNext)
	t.Cleanup(func() { SetSaveObserver(nil) })

	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "programmatic-org", Repos: []string{"repo-a"}},
		GitHub:     GitHubConfig{Token: "ghp_tok"},
		Agents:     map[string]AgentConfig{"worker": {Backend: "claude", Enabled: true}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	w.reload()
	select {
	case org := <-reloaded:
		t.Fatalf("programmatic save reloaded %q, want skip", org)
	default:
	}

	if err := os.WriteFile(path, []byte(minimalValidYAML("external-org", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}
	w.reload()
	select {
	case org := <-reloaded:
		if org != "external-org" {
			t.Fatalf("external reload org = %q, want external-org", org)
		}
	default:
		t.Fatal("external edit did not reload after the one skipped save")
	}
}

func TestWatcher_CancelStops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("o", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWatcher(path, func(cfg *Config) {}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Start(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop after context cancellation")
	}
}

func TestWatcher_Debounce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("o", "ghp_tok")), 0o600); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWatcher(path, func(cfg *Config) {
		reloadCount.Add(1)
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	// Write multiple times in quick succession — should debounce to one reload
	const rapidWrites = 5
	for i := 0; i < rapidWrites; i++ {
		os.WriteFile(path, []byte(minimalValidYAML("o", "ghp_tok")), 0o600)
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for debounce to settle
	time.Sleep(time.Second)

	count := reloadCount.Load()
	if count > 2 {
		t.Errorf("expected debouncing to reduce reloads, got %d reloads for %d writes", count, rapidWrites)
	}
}

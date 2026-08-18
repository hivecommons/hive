package policies

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const defaultPolicyBranch = "main"

type Watcher struct {
	repoURL      string
	branch       string
	subPath      string
	localDir     string
	pollInterval time.Duration
	policies     map[string][]byte
	mu           sync.RWMutex
	logger       *slog.Logger
}

func NewWatcher(repoURL, branch, subPath, localDir string, pollInterval time.Duration, logger *slog.Logger) *Watcher {
	if branch == "" {
		branch = defaultPolicyBranch
	}
	return &Watcher{
		repoURL:      repoURL,
		branch:       branch,
		subPath:      subPath,
		localDir:     localDir,
		pollInterval: pollInterval,
		policies:     make(map[string][]byte),
		logger:       logger,
	}
}

func (w *Watcher) Start(ctx context.Context) error {
	if err := w.initialClone(); err != nil {
		return fmt.Errorf("initial policy clone: %w", err)
	}

	if err := w.loadPolicies(); err != nil {
		return fmt.Errorf("loading policies: %w", err)
	}

	go w.pollLoop(ctx)

	return nil
}

func (w *Watcher) GetPolicy(agentName string) ([]byte, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	data, ok := w.policies[agentName]
	return data, ok
}

func (w *Watcher) AllPolicies() map[string][]byte {
	w.mu.RLock()
	defer w.mu.RUnlock()

	result := make(map[string][]byte, len(w.policies))
	for k, v := range w.policies {
		result[k] = v
	}
	return result
}

func (w *Watcher) initialClone() error {
	if _, err := os.Stat(filepath.Join(w.localDir, ".git")); err == nil {
		cmd := exec.Command("git", "-C", w.localDir, "pull", "--rebase", "origin", w.branch)
		if out, err := cmd.CombinedOutput(); err != nil {
			w.logger.Warn("git pull failed, re-cloning", "error", err, "output", string(out))
			os.RemoveAll(w.localDir)
		} else {
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(w.localDir), 0755); err != nil {
		return err
	}

	if info, err := os.Stat(w.localDir); err == nil && info.IsDir() {
		os.RemoveAll(w.localDir)
	}

	cmd := exec.Command("git", "clone", "--depth", "1", "--branch", w.branch, w.repoURL, w.localDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone %s: %w\n%s", w.repoURL, err, string(out))
	}

	return nil
}

func (w *Watcher) loadPolicies() error {
	policiesDir := filepath.Join(w.localDir, w.subPath)

	entries, err := os.ReadDir(policiesDir)
	if err != nil {
		return fmt.Errorf("reading policies dir %s: %w", policiesDir, err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if filepath.Ext(name) != ".md" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(policiesDir, name))
		if err != nil {
			w.logger.Warn("failed to read policy file", "file", name, "error", err)
			continue
		}

		agentName := policyFileToAgent(name)
		w.policies[agentName] = data
	}

	w.logger.Info("policies loaded", "count", len(w.policies))
	return nil
}

func (w *Watcher) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pull()
		}
	}
}

func (w *Watcher) pull() {
	cmd := exec.Command("git", "-C", w.localDir, "pull", "--rebase", "origin", w.branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.logger.Warn("policy repo pull failed", "error", err, "output", string(out))
		return
	}

	if string(out) == "Already up to date.\n" {
		return
	}

	w.logger.Info("policy repo updated, reloading")
	if err := w.loadPolicies(); err != nil {
		w.logger.Warn("failed to reload policies after pull", "error", err)
	}
}

func policyFileToAgent(filename string) string {
	name := filepath.Base(filename)
	name = name[:len(name)-len(filepath.Ext(name))]

	suffixes := []string{"-policy", "_policy"}
	for _, suffix := range suffixes {
		if idx := len(name) - len(suffix); idx > 0 && name[idx:] == suffix {
			return name[:idx]
		}
	}
	return name
}

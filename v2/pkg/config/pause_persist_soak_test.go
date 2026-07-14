package config

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestPausePersist_ConcurrentSoak reproduces the production pause-persistence
// path and proves it survives 30 restart cycles with CONCURRENT pauses — the
// exact scenario (#1885) where unsynchronized concurrent cfg.Save() calls
// dropped some pauses so agents reverted to running on restart.
//
// Each "restart" = save the current config, reload it from disk, and assert
// the paused set is exactly what we set. Pauses within a cycle are applied
// concurrently through a mutex-serialized persist callback (mirroring main.go).
func TestPausePersist_ConcurrentSoak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")

	agents := map[string]AgentConfig{
		"supervisor":    {Backend: "claude"},
		"scanner":       {Backend: "claude"},
		"quality":       {Backend: "claude"},
		"brainstorm":    {Backend: "claude"},
		"ci-maintainer": {Backend: "claude"},
		"architect":     {Backend: "claude"},
		"guide":         {Backend: "claude"},
		"sec-check":     {Backend: "claude"},
		"strategist":    {Backend: "claude"},
	}
	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "testorg", Repos: []string{"repo1"}},
		GitHub:     GitHubConfig{AppID: 3568013},
		Agents:     agents,
	}

	// The mutex-serialized persist callback from cmd/hive/main.go.
	var persistMu sync.Mutex
	persist := func(name string, paused bool) {
		persistMu.Lock()
		defer persistMu.Unlock()
		ac, ok := cfg.Agents[name]
		if !ok || ac.Paused == paused {
			return
		}
		ac.Paused = paused
		cfg.Agents[name] = ac
		if err := cfg.Save(); err != nil {
			t.Errorf("Save: %v", err)
		}
	}

	// The operator's intended paused set (everything except scanner).
	wantPaused := []string{"supervisor", "quality", "brainstorm", "ci-maintainer",
		"architect", "guide", "sec-check", "strategist"}

	for cycle := 0; cycle < 30; cycle++ {
		// Apply all pauses CONCURRENTLY — this is what clobbered before.
		var wg sync.WaitGroup
		for _, name := range wantPaused {
			wg.Add(1)
			go func(n string) { defer wg.Done(); persist(n, true) }(name)
		}
		wg.Wait()

		// "Restart": reload the persisted config from disk.
		reloaded, err := Load(path)
		if err != nil {
			t.Fatalf("cycle %d: reload: %v", cycle, err)
		}

		// Every intended pause must have survived.
		for _, n := range wantPaused {
			if !reloaded.Agents[n].Paused {
				t.Fatalf("cycle %d: agent %q not paused after restart (persistence dropped it)", cycle, n)
			}
		}
		// scanner must stay running.
		if reloaded.Agents["scanner"].Paused {
			t.Fatalf("cycle %d: scanner unexpectedly paused", cycle)
		}

		// Now unpause half concurrently and confirm THAT persists too.
		unpause := wantPaused[:4]
		wg = sync.WaitGroup{}
		for _, name := range unpause {
			wg.Add(1)
			go func(n string) { defer wg.Done(); persist(n, false) }(name)
		}
		wg.Wait()
		reloaded2, err := Load(path)
		if err != nil {
			t.Fatalf("cycle %d: reload2: %v", cycle, err)
		}
		for _, n := range unpause {
			if reloaded2.Agents[n].Paused {
				t.Fatalf("cycle %d: agent %q still paused after unpause", cycle, n)
			}
		}
		// Re-pause them for the next cycle's assertions.
		for _, name := range unpause {
			persist(name, true)
		}
	}
	_ = fmt.Sprint // keep import if trimmed
}

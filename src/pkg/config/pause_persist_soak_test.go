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

	// The persist callback from cmd/hive/main.go, via the real production method.
	persist := func(name string, paused bool) {
		if _, err := cfg.SetAgentPausedAndSave(name, paused); err != nil {
			t.Errorf("persist: %v", err)
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

// TestPausePersist_TwoUnsynchronizedSavers reproduces the ACTUAL production bug
// the earlier soak test missed: there are TWO independent goroutines that call
// cfg.Save(), and they are NOT guarded by the same higher-level mutex.
//
//   - Path A: AgentMgr.Pause() -> mutex-guarded persist callback -> cfg.Save()
//   - Path B: handlePause HTTP handler -> go PersistFunc() -> persistState() ->
//     cfg.Save(), running fully async with NO shared lock with Path A.
//
// When 7 agents are paused in quick succession, each pause fires BOTH paths.
// Path B marshals c.Agents at whatever partial state it holds and can land its
// write AFTER Path A's, clobbering it — so only the last agent or two reach the
// PVC. Modeling Path B WITHOUT the callback mutex proves the file-level saveMu
// in Config.Save() is what actually closes the race: remove saveMu and this
// test drops pauses; with it, all survive.
func TestPausePersist_TwoUnsynchronizedSavers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")

	names := []string{"supervisor", "quality", "ci-maintainer", "architect",
		"guide", "sec-check", "strategist"}
	agents := map[string]AgentConfig{"scanner": {Backend: "claude"}}
	for _, n := range names {
		agents[n] = AgentConfig{Backend: "claude"}
	}
	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "testorg", Repos: []string{"repo1"}},
		GitHub:     GitHubConfig{AppID: 3568013},
		Agents:     agents,
	}

	// Path A: the pause callback (main.go) -> SetAgentPausedAndSave.
	pathA := func(name string) {
		if _, err := cfg.SetAgentPausedAndSave(name, true); err != nil {
			t.Errorf("pathA: %v", err)
		}
	}
	// Path B: the async PersistFunc (persistState) -> ReconcilePausedAndSave,
	// carrying the authoritative live paused set. Both paths go through the
	// real production methods, which share saveMu — so the map mutation and
	// the file write are both serialized. Remove saveMu and this races/drops.
	pathB := func(livePaused map[string]bool) {
		if err := cfg.ReconcilePausedAndSave(livePaused); err != nil {
			t.Errorf("pathB: %v", err)
		}
	}

	// Build the full target paused set once; each goroutine gets its own copy
	// so the test harness itself is race-free (production's persistState builds
	// a fresh map from AllStatuses on every call). The concurrency under test is
	// pathA vs pathB both mutating cfg.Agents and writing the file.
	fullPaused := make(map[string]bool, len(names))
	for _, n := range names {
		fullPaused[n] = true
	}

	for cycle := 0; cycle < 30; cycle++ {
		var wg sync.WaitGroup
		for _, name := range names {
			wg.Add(1)
			go func(n string) {
				defer wg.Done()
				pathA(n)                     // Path A: this pause
				pathB(cloneBoolMap(fullPaused)) // Path B: async full persist
			}(name)
		}
		wg.Wait()

		reloaded, err := Load(path)
		if err != nil {
			t.Fatalf("cycle %d: reload: %v", cycle, err)
		}
		for _, n := range names {
			if !reloaded.Agents[n].Paused {
				t.Fatalf("cycle %d: agent %q not paused after restart — lost-write race", cycle, n)
			}
		}
		// Reset for next cycle (mutex-guarded, no map race).
		reset := make(map[string]bool, len(names))
		for _, n := range names {
			reset[n] = false
		}
		if err := cfg.ReconcilePausedAndSave(reset); err != nil {
			t.Fatalf("cycle %d: reset save: %v", cycle, err)
		}
	}
}

func cloneBoolMap(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

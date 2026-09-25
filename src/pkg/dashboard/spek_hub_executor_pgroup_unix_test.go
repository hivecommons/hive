//go:build !windows

package dashboard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// A `sh -c` stage that forks a grandchild holding the output pipe: without
// group kill + WaitDelay, cancelling the context kills only sh and Wait blocks
// until the grandchild exits on its own.
func TestSpekHubRunStageCommandKillsGrandchildrenOnCancel(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	worktree := t.TempDir()
	pidFile := filepath.Join(worktree, "grandchild.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	var pid int
	go func() {
		defer close(done)
		_, pid, _ = e.runStageCommand(ctx, worktree, os.Environ(), spekHubStage{runKey: "myorg/repo1#57", stage: StageSpec, gen: 1},
			[]string{"sh", "-c", "sleep 300 & echo $! > " + pidFile + "; wait"})
	}()
	select {
	case <-done:
	case <-time.After(spekHubWaitDelay + 5*time.Second):
		t.Fatal("runStageCommand still blocked after cancel + WaitDelay")
	}
	if pid == 0 {
		t.Fatal("stage process never started")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("grandchild pid not recorded: %v", err)
	}
	var gpid int
	if _, err := fmt.Sscan(string(raw), &gpid); err != nil || gpid <= 0 {
		t.Fatalf("bad grandchild pid %q: %v", raw, err)
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(3 * time.Second)
	for {
		if err := syscall.Kill(gpid, 0); err != nil {
			return
		}
		select {
		case <-tick.C:
		case <-timeout:
			_ = exec.Command("kill", "-9", string(raw)).Run()
			t.Fatalf("grandchild %d survived stage cancellation", gpid)
		}
	}
}

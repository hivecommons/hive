package meetllm

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
)

func newPool(t *testing.T, c Config, n int) *CLIPool {
	t.Helper()
	p, err := NewCLIPool(c, n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// Overlapping turns run on separate warm processes, never interleaved on
// one, and the pool grows no further than its size.
func TestCLIPoolParallelTurns(t *testing.T) {
	dir := t.TempDir()
	c := cliConfig(t, dir, fakeSlow)
	c.UsageFile = filepath.Join(dir, "usage.jsonl")
	c.MaxInFlight = 8
	c.Gate = NewGate(2)
	c.Pool = newPool(t, c, 2)
	if err := c.Pool.Warm(); err != nil {
		t.Fatal(err)
	}
	const calls = 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	pids := map[int]bool{}
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reply, err := Complete(context.Background(), c, http.DefaultClient, "q")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			pids[replyPID(t, reply)] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(pids) != 2 {
		t.Fatalf("turns ran on %d processes, want 2: %v", len(pids), pids)
	}
	if n := c.Pool.Started(); n != 2 {
		t.Fatalf("pool runs %d CLIs, want 2", n)
	}
	if n, _ := UsageTotal(c.UsageFile); n != calls*fakeTokensPerTurn {
		t.Fatalf("usage %d, want %d", n, calls*fakeTokensPerTurn)
	}
}

// One request at a time keeps a single warm CLI: the pool only grows when
// calls overlap.
func TestCLIPoolSequentialStaysSingle(t *testing.T) {
	dir := t.TempDir()
	c := cliConfig(t, dir, fakeOK)
	c.Pool = newPool(t, c, 3)
	for range 3 {
		if _, err := Complete(context.Background(), c, http.DefaultClient, "q"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(readLines(t, filepath.Join(dir, "starts"))); n != 1 {
		t.Fatalf("sequential turns started %d CLIs", n)
	}
}

// A closed pool refuses turns.
func TestCLIPoolClosed(t *testing.T) {
	c := cliConfig(t, t.TempDir(), fakeOK)
	c.Pool = newPool(t, c, 2)
	if err := c.Pool.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Complete(context.Background(), c, http.DefaultClient, "q"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed pool err = %v", err)
	}
	if _, err := NewCLIPool(Config{Backend: "codex"}, 2); err == nil {
		t.Fatal("want an error for a backend with no warm mode")
	}
}

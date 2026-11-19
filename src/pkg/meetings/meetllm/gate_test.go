package meetllm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateServer is a chat endpoint that holds every request until release is
// closed, counting the most requests it held at once.
func gateServer(t *testing.T, tokens string) (*httptest.Server, chan struct{}, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	release := make(chan struct{})
	var now, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := now.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		now.Add(-1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"total_tokens":` + tokens + `}}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	return srv, release, &now, &peak
}

func gateConfig(srv *httptest.Server, dir string, g *Gate) Config {
	return Config{
		Endpoint: srv.URL, Model: "m", Timeout: 5 * time.Second,
		UsageFile: filepath.Join(dir, "usage.jsonl"), MaxInFlight: 8, Gate: g,
	}
}

// A meeting's gate lets up to its size of calls run at once, and no more.
func TestCompleteGateRunsCallsInParallel(t *testing.T) {
	for _, size := range []int{1, 3} {
		srv, release, now, peak := gateServer(t, "10")
		c := gateConfig(srv, t.TempDir(), NewGate(size))
		const calls = 4
		var wg sync.WaitGroup
		errs := make(chan error, calls)
		for range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := Complete(context.Background(), c, srv.Client(), "q")
				errs <- err
			}()
		}
		waitFor(t, "calls in flight", func() bool { return int(now.Load()) == size })
		// Give a call past the gate's size time to show up if it could.
		time.Sleep(50 * time.Millisecond)
		if got := int(peak.Load()); got != size {
			t.Fatalf("gate %d: %d calls in flight at once", size, got)
		}
		close(release)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("gate %d: %v", size, err)
			}
		}
		if n, _ := UsageTotal(c.UsageFile); n != calls*10 {
			t.Fatalf("gate %d: usage %d, want %d", size, n, calls*10)
		}
	}
}

// Concurrent calls cannot all pass the budget check on the same ledger
// total: a call in flight holds a reservation, so the next one is refused
// before it reaches the model.
func TestCompleteGateReservesBudget(t *testing.T) {
	srv, release, now, _ := gateServer(t, "100")
	c := gateConfig(srv, t.TempDir(), NewGate(3))
	c.MaxTokens = 150
	first := make(chan error, 1)
	go func() {
		_, err := Complete(context.Background(), c, srv.Client(), "q")
		first <- err
	}()
	waitFor(t, "the first call in flight", func() bool { return now.Load() == 1 })
	if _, err := Complete(context.Background(), c, srv.Client(), "q2"); !errors.Is(err, ErrBudget) {
		t.Fatalf("second call while the first holds the budget: err = %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if r := c.Gate.Reserved(); r != 0 {
		t.Fatalf("reservation left after the call: %d", r)
	}
	// Sequential calls keep the old rule: refused once the ledger is spent.
	if _, err := Complete(context.Background(), c, srv.Client(), "q3"); err != nil {
		t.Fatalf("a call under budget: %v", err)
	}
	if _, err := Complete(context.Background(), c, srv.Client(), "q4"); !errors.Is(err, ErrBudget) {
		t.Fatalf("over budget err = %v", err)
	}
}

func TestGateReserveAndSettle(t *testing.T) {
	g := NewGate(0)
	if g.Size() != DefaultPerMeetingInFlight {
		t.Fatalf("size %d", g.Size())
	}
	held, err := g.reserve(0, 0)
	if err != nil || held != defaultCallEstimate {
		t.Fatalf("reserve without a budget = %d %v", held, err)
	}
	g.settle(held, defaultCallEstimate+500)
	if g.Reserved() != 0 || g.estimate != defaultCallEstimate+500 {
		t.Fatalf("after settle reserved %d estimate %d", g.Reserved(), g.estimate)
	}
	a, _ := g.reserve(0, 10_000)
	if _, err := g.reserve(10_000-a, 10_000); !errors.Is(err, ErrBudget) {
		t.Fatalf("reservation not counted: %v", err)
	}
	g.settle(a, 0)
	g.settle(a, 0)
	if g.Reserved() != 0 {
		t.Fatalf("double settle went negative: %d", g.Reserved())
	}
}

// A call waiting for the meeting's place gives up at its deadline.
func TestCompleteGateBusy(t *testing.T) {
	srv, _, now, _ := gateServer(t, "1")
	c := gateConfig(srv, t.TempDir(), NewGate(1))
	held := c
	go func() { _, _ = Complete(context.Background(), held, srv.Client(), "hold") }()
	waitFor(t, "the held call", func() bool { return now.Load() == 1 })
	c.Timeout = 50 * time.Millisecond
	if _, err := Complete(context.Background(), c, srv.Client(), "q"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy err = %v", err)
	}
}

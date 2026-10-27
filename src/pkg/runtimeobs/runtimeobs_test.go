package runtimeobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func TestParseThreads(t *testing.T) {
	status := []byte("Name:\thive\nState:\tS (sleeping)\nThreads:\t156\nSigQ:\t0/1\n")
	if got := ParseThreads(status); got != 156 {
		t.Fatalf("ParseThreads = %d, want 156", got)
	}
	if got := ParseThreads([]byte("Name:\thive\n")); got != 0 {
		t.Fatalf("ParseThreads without field = %d, want 0", got)
	}
}

func TestTakeIsPopulated(t *testing.T) {
	s := Take()
	if s.Goroutines < 1 {
		t.Fatalf("Goroutines = %d, want >= 1", s.Goroutines)
	}
	if s.TotalSys == 0 || s.NextGC == 0 {
		t.Fatalf("runtime/metrics samples missing: %+v", s)
	}
}

func TestStartLoggerEmitsPerTick(t *testing.T) {
	var buf bytes.Buffer
	written := make(chan struct{}, 8)
	logger := slog.New(&signallingHandler{Handler: slog.NewTextHandler(&buf, nil), done: written})
	tick := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartLogger(ctx, logger, tick)
	for i := 0; i < 3; i++ {
		tick <- time.Time{}
		// Each tick is handled synchronously; wait for its record before the
		// next so the buffer is complete when the goroutine is stopped.
		select {
		case <-written:
		case <-time.After(5 * time.Second):
			t.Fatalf("tick %d: no log record written", i)
		}
	}
	cancel()
	out := buf.String()
	if n := strings.Count(out, "mem: runtime snapshot"); n != 3 {
		t.Fatalf("got %d mem lines, want 3:\n%s", n, out)
	}
	for _, key := range []string{"heap_inuse=", "heap_objects=", "goroutines=", "threads=", "next_gc="} {
		if !strings.Contains(out, key) {
			t.Errorf("mem line lacks %s:\n%s", key, out)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:6060": true,
		"localhost:6060": true,
		"[::1]:6060":     true,
		"127.9.9.9:1":    true,
		"0.0.0.0:6060":   false,
		":6060":          false,
		"10.0.0.1:6060":  false,
		"127.0.0.1":      false,
	} {
		if got := IsLoopbackAddr(addr); got != want {
			t.Errorf("IsLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestStartPprofRejectsNonLoopback(t *testing.T) {
	if _, _, err := StartPprof("0.0.0.0:0", nil); !errors.Is(err, ErrNotLoopback) {
		t.Fatalf("err = %v, want ErrNotLoopback", err)
	}
	if _, stop, err := StartPprof("", nil); err != nil || stop == nil {
		t.Fatalf("empty addr: err=%v stop-nil=%v", err, stop == nil)
	}
}

func TestStartPprofServesIndexAndRuntime(t *testing.T) {
	bound, stop, err := StartPprof("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/goroutine?debug=1", "/debug/runtime"} {
		resp, err := http.Get("http://" + bound + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
		if path == "/debug/runtime" && !strings.Contains(string(body), "goroutines=") {
			t.Fatalf("/debug/runtime body: %s", body)
		}
	}
}

func TestCgroupMemoryLimit(t *testing.T) {
	files := func(m map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := m[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("missing")
		}
	}
	cases := []struct {
		name  string
		files map[string]string
		want  int64
		ok    bool
	}{
		{"v2 limit", map[string]string{cgroupV2MemoryMax: "42949672960\n"}, 42949672960, true},
		{"v2 max", map[string]string{cgroupV2MemoryMax: "max\n"}, 0, false},
		{"v1 limit", map[string]string{cgroupV1MemoryLimit: "25769803776\n"}, 25769803776, true},
		{"v1 unlimited", map[string]string{cgroupV1MemoryLimit: "9223372036854771712\n"}, 0, false},
		{"none", map[string]string{}, 0, false},
		{"garbage", map[string]string{cgroupV2MemoryMax: "lots\n"}, 0, false},
	}
	for _, tc := range cases {
		got, ok := CgroupMemoryLimit(files(tc.files))
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%d,%v) want (%d,%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestApplyMemoryLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	const cg = int64(40 << 30)
	readV2 := func(p string) ([]byte, error) {
		if p == cgroupV2MemoryMax {
			return []byte("42949672960"), nil
		}
		return nil, errors.New("missing")
	}
	out := ApplyMemoryLimit(func(string) string { return "" }, readV2)
	want := DeriveMemoryLimit(cg)
	if out.Source != "cgroup" || out.Limit != want || debug.SetMemoryLimit(-1) != want {
		t.Fatalf("cgroup path: %+v (runtime limit %d, want %d)", out, debug.SetMemoryLimit(-1), want)
	}
	if want >= cg || want < cg*80/100 {
		t.Fatalf("derived limit %d is not ~%d%% of %d", want, MemoryLimitPercent, cg)
	}

	debug.SetMemoryLimit(prev)
	out = ApplyMemoryLimit(func(string) string { return "16GiB" }, readV2)
	if out.Source != "env" || debug.SetMemoryLimit(-1) != prev {
		t.Fatalf("env path must not touch the runtime limit: %+v", out)
	}

	out = ApplyMemoryLimit(func(string) string { return "" }, func(string) ([]byte, error) { return nil, errors.New("missing") })
	if out.Source != "none" || debug.SetMemoryLimit(-1) != prev {
		t.Fatalf("no-cgroup path must not touch the runtime limit: %+v", out)
	}
}

// signallingHandler sends on done after every record so tests can wait on the
// observable write instead of sleeping.
type signallingHandler struct {
	slog.Handler
	done chan<- struct{}
}

func (h *signallingHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.Handler.Handle(ctx, r)
	h.done <- struct{}{}
	return err
}

func TestStartLoggerDefaultsAndStopsOnCancel(t *testing.T) {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	StartLogger(ctx, nil, nil)
	cancel()
	deadline := time.After(5 * time.Second)
	for runtime.NumGoroutine() > before {
		select {
		case <-deadline:
			t.Fatalf("logger goroutine did not stop: %d goroutines, baseline %d", runtime.NumGoroutine(), before)
		default:
			runtime.Gosched()
		}
	}
}

func TestStartPprofEmptyAddrIsDisabled(t *testing.T) {
	bound, stop, err := StartPprof("", nil)
	if err != nil || bound != "" || stop == nil {
		t.Fatalf("StartPprof(\"\") = %q, %v, stop nil=%v", bound, err, stop == nil)
	}
	stop()
}

func TestStartPprofReportsListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	defer ln.Close()
	if _, _, err := StartPprof(ln.Addr().String(), nil); err == nil {
		t.Fatal("StartPprof on an occupied port must fail")
	}
}

func TestThreadCountWithoutProcStatus(t *testing.T) {
	saved := procSelfStatus
	procSelfStatus = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { procSelfStatus = saved })
	if got := threadCount(); got != 0 {
		t.Fatalf("threadCount without /proc = %d, want 0", got)
	}
	if s := Take(); s.Threads != 0 {
		t.Fatalf("Take().Threads without /proc = %d, want 0", s.Threads)
	}
}

func TestApplyMemoryLimitUsesProcessDefaults(t *testing.T) {
	saved := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(saved) })
	out := ApplyMemoryLimit(nil, nil)
	switch out.Source {
	case "env", "none", "cgroup":
	default:
		t.Fatalf("unexpected source %q: %+v", out.Source, out)
	}
	if out.Limit <= 0 {
		t.Fatalf("effective limit must be positive: %+v", out)
	}
}

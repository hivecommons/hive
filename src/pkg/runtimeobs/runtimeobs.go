// Package runtimeobs gives the hive process the three things it lacked when
// hivecommons/hive#14681 (RSS +360 MiB/min, OOM at 24Gi) had to be diagnosed
// from /proc alone: a loopback net/http/pprof listener, a periodic memory /
// goroutine / thread log line, and a cgroup-aware GOMEMLIMIT default so the
// GC pushes back before the kernel does.
package runtimeobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"
)

const (
	// LogInterval is how often StartLogger emits the `mem:` line.
	LogInterval = 60 * time.Second

	// MemoryLimitPercent is the share of the cgroup memory limit handed to
	// the Go runtime as GOMEMLIMIT when the env var is unset. The remaining
	// 15% covers the non-Go RSS in the same cgroup (agent CLIs run in the
	// same container) and runtime overhead outside the heap.
	MemoryLimitPercent = 85

	// cgroupV2MemoryMax and cgroupV1MemoryLimit are the files the container
	// memory limit is read from, in order of preference. "max" (v2) or the
	// kernel's "no limit" sentinel (v1) leave GOMEMLIMIT untouched.
	cgroupV2MemoryMax   = "/sys/fs/cgroup/memory.max"
	cgroupV1MemoryLimit = "/sys/fs/cgroup/memory/memory.limit_in_bytes"

	// cgroupV1NoLimitFloor: v1 reports an unlimited cgroup as a page-rounded
	// value just under 2^63; anything at or above this is "no limit".
	cgroupV1NoLimitFloor = 1 << 62

	// memoryLimitEnv is the Go runtime's own variable; when set the runtime
	// has already applied it and this package only reports it.
	memoryLimitEnv = "GOMEMLIMIT"

	// pprofReadHeaderTimeout bounds a slow client on the debug listener; the
	// handlers themselves stream for as long as the profile asks (seconds=).
	pprofReadHeaderTimeout = 10 * time.Second
)

// procSelfStatus is where the OS thread count comes from on Linux; a
// variable so tests can point it at a missing file.
var procSelfStatus = "/proc/self/status"

// Snapshot is one reading of the process-level runtime counters. Every field
// comes from runtime/metrics (no stop-the-world) except Threads, which is
// read from /proc on Linux and is 0 where that is unavailable.
type Snapshot struct {
	HeapInuse   uint64 `json:"heap_inuse_bytes"`
	HeapLive    uint64 `json:"heap_live_bytes"`
	HeapObjects uint64 `json:"heap_objects"`
	NextGC      uint64 `json:"next_gc_bytes"`
	TotalSys    uint64 `json:"total_sys_bytes"`
	NumGC       uint64 `json:"gc_cycles"`
	Goroutines  int    `json:"goroutines"`
	Threads     int    `json:"threads"`
	MemLimit    int64  `json:"gomemlimit_bytes"`
}

var sampleNames = []string{
	"/memory/classes/heap/objects:bytes",
	"/gc/heap/live:bytes",
	"/gc/heap/objects:objects",
	"/gc/heap/goal:bytes",
	"/memory/classes/total:bytes",
	"/gc/cycles/total:gc-cycles",
}

// Take reads a Snapshot.
func Take() Snapshot {
	samples := make([]metrics.Sample, len(sampleNames))
	for i, n := range sampleNames {
		samples[i].Name = n
	}
	metrics.Read(samples)
	u := func(i int) uint64 {
		if samples[i].Value.Kind() == metrics.KindUint64 {
			return samples[i].Value.Uint64()
		}
		return 0
	}
	return Snapshot{
		HeapInuse:   u(0),
		HeapLive:    u(1),
		HeapObjects: u(2),
		NextGC:      u(3),
		TotalSys:    u(4),
		NumGC:       u(5),
		Goroutines:  runtime.NumGoroutine(),
		Threads:     threadCount(),
		MemLimit:    debug.SetMemoryLimit(-1),
	}
}

// LogAttrs renders the snapshot as slog attributes.
func (s Snapshot) LogAttrs() []any {
	return []any{
		"heap_inuse", s.HeapInuse,
		"heap_live", s.HeapLive,
		"heap_objects", s.HeapObjects,
		"total_sys", s.TotalSys,
		"next_gc", s.NextGC,
		"gc_cycles", s.NumGC,
		"goroutines", s.Goroutines,
		"threads", s.Threads,
		"gomemlimit", s.MemLimit,
	}
}

// threadCount parses Threads: from /proc/self/status; 0 when unavailable.
func threadCount() int {
	data, err := os.ReadFile(procSelfStatus)
	if err != nil {
		return 0
	}
	return ParseThreads(data)
}

// ParseThreads extracts the Threads: field from a /proc/<pid>/status body.
func ParseThreads(status []byte) int {
	for _, line := range bytes.Split(status, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("Threads:")) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(line[len("Threads:"):])))
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// StartLogger emits one `mem:` INFO line per tick until ctx is done. tick
// is injectable so tests drive it without sleeping; nil means a real ticker
// at LogInterval.
func StartLogger(ctx context.Context, logger *slog.Logger, tick <-chan time.Time) {
	if logger == nil {
		logger = slog.Default()
	}
	var stop func()
	if tick == nil {
		t := time.NewTicker(LogInterval)
		tick, stop = t.C, t.Stop
	}
	go func() {
		if stop != nil {
			defer stop()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
				logger.Info("mem: runtime snapshot", Take().LogAttrs()...)
			}
		}
	}()
}

// ErrNotLoopback is returned by StartPprof for a non-loopback bind host.
var ErrNotLoopback = errors.New("pprof listener address must be loopback (127.0.0.0/8, ::1, or localhost)")

// IsLoopbackAddr reports whether host:port names a loopback host.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// StartPprof serves net/http/pprof on addr, which must be a loopback address.
// It returns the bound address and a stop func. An empty addr is a no-op.
func StartPprof(addr string, logger *slog.Logger) (bound string, stop func(), err error) {
	if addr == "" {
		return "", func() {}, nil
	}
	if !IsLoopbackAddr(addr) {
		return "", nil, fmt.Errorf("%w: %q", ErrNotLoopback, addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/debug/runtime", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		attrs := Take().LogAttrs()
		for i := 0; i+1 < len(attrs); i += 2 {
			fmt.Fprintf(w, "%v=%v\n", attrs[i], attrs[i+1])
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: pprofReadHeaderTimeout}
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && logger != nil {
			logger.Warn("pprof listener stopped", "addr", addr, "error", serveErr)
		}
	}()
	return ln.Addr().String(), func() { _ = srv.Close() }, nil
}

// MemoryLimitOutcome says what ApplyMemoryLimit did, for the startup log.
type MemoryLimitOutcome struct {
	// Source is "env" (GOMEMLIMIT already set), "cgroup" (derived), or
	// "none" (no limit found; runtime default of math.MaxInt64 kept).
	Source string
	// CgroupLimit is the raw cgroup limit in bytes when one was found.
	CgroupLimit int64
	// Limit is the effective GOMEMLIMIT in bytes.
	Limit int64
}

// ApplyMemoryLimit sets the Go soft memory limit to MemoryLimitPercent of
// the cgroup memory limit unless GOMEMLIMIT is already set (the runtime has
// then applied it) or no finite cgroup limit exists. getenv and readFile are
// injectable for tests; nil means os.Getenv / os.ReadFile.
func ApplyMemoryLimit(getenv func(string) string, readFile func(string) ([]byte, error)) MemoryLimitOutcome {
	if getenv == nil {
		getenv = os.Getenv
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	if strings.TrimSpace(getenv(memoryLimitEnv)) != "" {
		return MemoryLimitOutcome{Source: "env", Limit: debug.SetMemoryLimit(-1)}
	}
	cg, ok := CgroupMemoryLimit(readFile)
	if !ok {
		return MemoryLimitOutcome{Source: "none", Limit: debug.SetMemoryLimit(-1)}
	}
	limit := DeriveMemoryLimit(cg)
	debug.SetMemoryLimit(limit)
	return MemoryLimitOutcome{Source: "cgroup", CgroupLimit: cg, Limit: limit}
}

// DeriveMemoryLimit is MemoryLimitPercent of the cgroup limit.
func DeriveMemoryLimit(cgroupLimit int64) int64 {
	return cgroupLimit / 100 * MemoryLimitPercent
}

// CgroupMemoryLimit reads the container memory limit from cgroup v2, then
// v1. ok is false when neither file exists or the cgroup is unlimited.
func CgroupMemoryLimit(readFile func(string) ([]byte, error)) (limit int64, ok bool) {
	for _, path := range []string{cgroupV2MemoryMax, cgroupV1MemoryLimit} {
		data, err := readFile(path)
		if err != nil {
			continue
		}
		raw := strings.TrimSpace(string(data))
		if raw == "max" || raw == "" {
			return 0, false
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 || n >= cgroupV1NoLimitFloor {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

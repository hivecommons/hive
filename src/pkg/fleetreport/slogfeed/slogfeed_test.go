package slogfeed

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

type observed struct {
	class, key, message, diagnostics string
}

func newFeed(t *testing.T) (*slog.Logger, *[]observed, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	var seen []observed
	h := NewWithObserver(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}), func(class, key, message, diagnostics string) {
		seen = append(seen, observed{class, key, message, diagnostics})
	})
	return slog.New(h), &seen, &out
}

func TestHandlerForwardsAndObservesWarnPlus(t *testing.T) {
	logger, seen, out := newFeed(t)
	logger.Info("issue-request watcher started", "dir", "/var/run/x")
	logger.Warn("relay response write failed, will retry with backoff", "repo", "owner/repo", "error", "boom 42")
	logger.Error("kick failed: tmux send-keys exit 1")
	if n := strings.Count(out.String(), "\n"); n != 3 {
		t.Fatalf("wrapped handler saw %d records, want 3: %s", n, out.String())
	}
	if len(*seen) != 1 {
		t.Fatalf("observed = %+v, want exactly the WARN (the ERROR matches kick_failed_repeatedly keywords)", *seen)
	}
	got := (*seen)[0]
	if got.class != fleetreport.ClassUnclassifiedRepeated {
		t.Fatalf("class = %q", got.class)
	}
	if got.key != fleetreport.NormalizeMessage("relay response write failed, will retry with backoff") {
		t.Fatalf("key = %q", got.key)
	}
	if !strings.Contains(got.diagnostics, "WARN") || !strings.Contains(got.diagnostics, "repo=owner/repo") || !strings.Contains(got.diagnostics, "error=boom 42") {
		t.Fatalf("diagnostics = %q", got.diagnostics)
	}
}

func TestHandlerSkipsBenignAndExplicitClasses(t *testing.T) {
	logger, seen, _ := newFeed(t)
	logger.Warn("read tcp 10.0.0.1:443: i/o timeout")
	logger.Warn("relay fetch aborted: context canceled")
	logger.Warn("restart storm detected for agent quality")
	logger.Error("advisory post failed: 502")
	// The relay watcher's own retry line is covered by issue_request_stuck.
	logger.Warn("issue-request watcher: request failed, will retry with backoff")
	logger.Warn("")
	if len(*seen) != 0 {
		t.Fatalf("observed = %+v, want none", *seen)
	}
}

func TestHandlerWithAttrsCarriesBoundAttrsIntoDiagnostics(t *testing.T) {
	logger, seen, _ := newFeed(t)
	logger.With("agent", "quality").WithGroup("req").Warn("ledger write failed", "path", "/data/x.json")
	if len(*seen) != 1 {
		t.Fatalf("observed = %+v", *seen)
	}
	d := (*seen)[0].diagnostics
	if !strings.Contains(d, "agent=quality") || !strings.Contains(d, "path=/data/x.json") {
		t.Fatalf("diagnostics = %q", d)
	}
}

func TestKeyBoundsAndNormalises(t *testing.T) {
	a := Key("request failed for owner/repo-1 after 3 attempts (id 0123456789abcdef)")
	b := Key("request failed for owner/other-9 after 17 attempts (id fedcba9876543210)")
	if a == "" || a != b {
		t.Fatalf("keys differ: %q vs %q", a, b)
	}
	long := Key(strings.Repeat("x", 10*maxKeyBytes))
	if len(long) != maxKeyBytes {
		t.Fatalf("key not bounded: %d", len(long))
	}
	if Key("   ") != "" {
		t.Fatal("blank message produced a key")
	}
}

func TestFeedIntoTrackerQualifiesAtThresholdAndIdleClears(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	tr := fleetreport.NewRuntimeTracker(clock)
	var out bytes.Buffer
	logger := slog.New(NewWithObserver(slog.NewJSONHandler(&out, nil), tr.Observe))
	for i := 0; i < 4; i++ {
		logger.Warn("relay response write failed, will retry with backoff", "attempt", i)
		now = now.Add(time.Minute)
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("4 repeats produced evidence: %+v", ev)
	}
	logger.Warn("relay response write failed, will retry with backoff", "attempt", 99)
	ev := tr.Evidence()
	if len(ev) != 1 || ev[0].Class != fleetreport.ClassUnclassifiedRepeated || ev[0].Count != 5 {
		t.Fatalf("evidence = %+v", ev)
	}
	// Silence for 2× the window clears the condition and records it so the
	// publisher posts exactly one "cleared" comment.
	now = now.Add(21 * time.Minute)
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("idle condition still reported: %+v", ev)
	}
	cleared := tr.TakeCleared()
	if len(cleared) != 1 || cleared[0].Class != fleetreport.ClassUnclassifiedRepeated {
		t.Fatalf("TakeCleared = %+v", cleared)
	}
}

func TestHandlerEnabledDefersToWrapped(t *testing.T) {
	var out bytes.Buffer
	h := New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("INFO enabled on a WARN-level wrapped handler")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("ERROR not enabled")
	}
}

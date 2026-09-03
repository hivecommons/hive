package tracing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/hivecommons/hive/pkg/timeline"
)

func TestFormatReachTime(t *testing.T) {
	if got := formatReachTime(0); got != "" {
		t.Errorf("zero must render empty, got %q", got)
	}
	sec := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC).Unix()
	if got := formatReachTime(sec); got != "2026-08-22T12:00:00Z" {
		t.Errorf("formatReachTime = %q", got)
	}
}

func TestTimelineSpanNameFallback(t *testing.T) {
	if got := TimelineSpanName(timeline.Event{Kind: timeline.KindKicked}); got != "agent.kick" {
		t.Errorf("known kind = %q", got)
	}
	if got := TimelineSpanName(timeline.Event{Kind: timeline.Kind("mystery")}); got != "hive.timeline" {
		t.Errorf("unknown kind must fall back, got %q", got)
	}
}

func TestAgentKickAttributes(t *testing.T) {
	attrs := AgentKickAttributes("scanner", "copilot", "claude", "triage", "auto", 3)
	got := map[string]string{}
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value.String()
	}
	want := map[string]string{
		AttrHiveAgent:         "scanner",
		AttrGenAISystem:       "copilot",
		AttrGenAIRequestModel: "claude",
		AttrHiveLane:          "triage",
		AttrHiveACMMLevel:     "3",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("attr %s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}

	minimal := AgentKickAttributes("scanner", "", "", "", "", 0)
	for _, kv := range minimal {
		if kv.Key != attribute.Key(AttrHiveAgent) && !strings.Contains(string(kv.Key), "governor") {
			t.Errorf("empty optional fields must be omitted, found %s", kv.Key)
		}
	}
}

func TestSaveReachStateWriteError(t *testing.T) {
	resetReach(t)
	_, span := StartSpan(context.Background(), "governor.eval_cycle")
	span.End()

	missing := filepath.Join(t.TempDir(), "no-such-dir", "reach.json")
	if err := SaveReachState(missing); err == nil {
		t.Error("write into a missing directory must error")
	}
}

func TestSaveReachStateRenameError(t *testing.T) {
	resetReach(t)
	_, span := StartSpan(context.Background(), "governor.eval_cycle")
	span.End()

	dir := t.TempDir()
	// Destination is an existing non-empty directory, so os.Rename must fail.
	dest := filepath.Join(dir, "reach.json")
	if err := os.MkdirAll(filepath.Join(dest, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveReachState(dest); err == nil {
		t.Error("rename onto a non-empty directory must error")
	}
}

func TestReachOverflowLogSaturation(t *testing.T) {
	h := resetReach(t)
	ctx := context.Background()

	// Fill the component registry to its cap, then push well past the
	// once-per-name overflow log bound to hit the saturation branch.
	for i := 0; i < MaxReachComponents; i++ {
		_, span := StartSpan(ctx, fmt.Sprintf("comp%03d.op", i))
		span.End()
	}
	extra := maxOverflowLoggedComponents + 5
	for i := 0; i < extra; i++ {
		_, span := StartSpan(ctx, fmt.Sprintf("overflow%04d.op", i))
		span.End()
	}
	// Repeat one refused component: already-seen names must not log again.
	_, repeat := StartSpan(ctx, "overflow0000.op")
	repeat.End()

	report := ReachSnapshot()
	if report == nil {
		t.Fatal("expected a reach report")
	}
	if report.OverflowSpans != int64(extra)+1 {
		t.Errorf("overflow spans = %d, want %d", report.OverflowSpans, extra+1)
	}
	saturated := false
	h.mu.Lock()
	for _, msg := range h.msgs {
		if strings.Contains(msg, "saturated") {
			saturated = true
		}
	}
	h.mu.Unlock()
	if !saturated {
		t.Error("saturation warning must be logged once the name set is full")
	}
}

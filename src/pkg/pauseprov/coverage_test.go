package pauseprov

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOpenCorruptAndMissing(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{nope"), 0o600)
	if s, err := Open(bad, nil); err == nil || s == nil || s.LastResume("a") != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if s, err := Open(dir, nil); err == nil || s == nil {
		t.Fatalf("directory: %v", err)
	}
	if s, err := Open(filepath.Join(dir, "none.json"), nil); err != nil || s == nil {
		t.Fatalf("missing: %v", err)
	}
	if s, err := Open("", nil); err != nil || s == nil {
		t.Fatal("memory store")
	}
}

func TestAgentsAreBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.json")
	s, _ := Open(p, nil)
	for i := 0; i < MaxAgents+3; i++ {
		s.ObserveResume("agent"+strconv.Itoa(i), Event{Trigger: "acmm-pack", At: t0.Add(time.Duration(i) * time.Minute)})
	}
	if s.LastResume("agent0") != nil || s.LastResume("agent"+strconv.Itoa(MaxAgents+2)) == nil {
		t.Fatal("bound")
	}
	s2, _ := Open(p, nil)
	if s2.LastResume("agent"+strconv.Itoa(MaxAgents+2)) == nil {
		t.Fatal("not durable")
	}
}

func TestStaleIntentIsNotBorrowed(t *testing.T) {
	s, _ := Open("", nil)
	now := t0
	s.SetClock(func() time.Time { return now })
	s.IntendResume("a", "alice")
	now = t0.Add(2 * IntentTTL)
	s.ObserveResume("a", Event{Trigger: "dashboard-api"})
	if r := s.LastResume("a"); r == nil || r.By != "" || r.At.IsZero() {
		t.Fatalf("%+v", r)
	}
}

func TestDefaultStore(t *testing.T) {
	prev := Default()
	defer SetDefault(prev)
	s, _ := Open("", nil)
	SetDefault(s)
	if Default() != s {
		t.Fatal("default")
	}
}

func TestClassifyLongTriggerAndDescribeEdges(t *testing.T) {
	a := Classify(strings.Repeat("x", 200), "")
	if a.Kind != KindAuto || len([]rune(a.Label)) > maxTriggerRunes+20 {
		t.Fatalf("%+v", a)
	}
	if Classify("config", "").Kind != KindConfig {
		t.Fatal("config")
	}
	d := Describe(false, PauseFacts{}, &Event{})
	if d == nil || d.LastResume == nil || d.LastResume.Kind != KindUnknown || !strings.Contains(d.LastResume.Line, UnknownLabel) {
		t.Fatalf("%+v", d)
	}
	if got := quote("a\x00 b\n\tc " + strings.Repeat("y", 300)); strings.ContainsAny(got, "\x00\n\t") || len([]rune(got)) > maxQuoteRunes {
		t.Fatalf("%q", got)
	}
}

package nousstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var gateNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func TestGatePaths(t *testing.T) {
	l := New("/data/nous")
	for got, want := range map[string]string{
		l.GatesDir():         "/data/nous/gates",
		l.GatePendingPath():  "/data/nous/gates/pending.json",
		l.GateDecisionPath(): "/data/nous/gates/decision.json",
		l.GateHistoryPath():  "/data/nous/gates/history.jsonl",
		l.GateNotifiedPath(): "/data/nous/gates/notified",
	} {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
}

func TestValidGateDecision(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{GateApprove, true}, {GateReject, true}, {GateAbort, true},
		{"", false}, {"Approve", false}, {"approved", false}, {"yes", false},
	} {
		if got := ValidGateDecision(tc.in); got != tc.want {
			t.Errorf("ValidGateDecision(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestGateRoundTrip(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))

	if _, ok, err := l.LoadGatePending(); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v, want no pending gate", ok, err)
	}
	if _, ok, err := l.LoadGateDecision(); err != nil || ok {
		t.Fatalf("empty store: decision ok=%v err=%v", ok, err)
	}
	if hist, err := l.GateHistory(); err != nil || hist != nil {
		t.Fatalf("empty history = %v, %v", hist, err)
	}

	req := GateRequest{
		ID:       "design-1",
		Gate:     "design",
		Question: "Run the cache-size experiment?",
		Scope:    "governor",
		Summary:  "3 arms, 2h",
		Artifact: json.RawMessage(`{"arms":3}`),
	}
	if err := l.WriteGatePending(req, gateNow); err != nil {
		t.Fatal(err)
	}
	got, ok, err := l.LoadGatePending()
	if err != nil || !ok {
		t.Fatalf("LoadGatePending: ok=%v err=%v", ok, err)
	}
	if got.ID != "design-1" || got.Question != req.Question || got.Gate != "design" || got.Scope != "governor" {
		t.Fatalf("pending = %+v", got)
	}
	if !got.RequestedAt.Equal(gateNow) {
		t.Fatalf("RequestedAt = %v, want stamped with now", got.RequestedAt)
	}
	var art map[string]int
	if err := json.Unmarshal(got.Artifact, &art); err != nil || art["arms"] != 3 {
		t.Fatalf("artifact = %s (%v)", got.Artifact, err)
	}

	// A second publish replaces the first (newer gate wins).
	if err := l.WriteGatePending(GateRequest{ID: "design-2", Question: "q2", RequestedAt: gateNow.Add(-time.Hour)}, gateNow); err != nil {
		t.Fatal(err)
	}
	got, _, _ = l.LoadGatePending()
	if got.ID != "design-2" || !got.RequestedAt.Equal(gateNow.Add(-time.Hour)) {
		t.Fatalf("replaced pending = %+v", got)
	}

	later := gateNow.Add(5 * time.Minute)
	d := GateDecision{GateID: "design-2", Decision: GateApprove, Reason: "go", DecidedBy: "alice"}
	if err := l.RecordGateDecision(d, later); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l.LoadGatePending(); ok {
		t.Fatal("pending gate must be cleared once decided")
	}
	dec, ok, err := l.LoadGateDecision()
	if err != nil || !ok {
		t.Fatalf("LoadGateDecision: ok=%v err=%v", ok, err)
	}
	if dec.GateID != "design-2" || dec.Decision != GateApprove || dec.Reason != "go" || dec.DecidedBy != "alice" || !dec.DecidedAt.Equal(later) {
		t.Fatalf("decision = %+v", dec)
	}

	// A second decision appends to history and replaces the live file.
	if err := l.WriteGatePending(GateRequest{ID: "findings-1", Question: "ship?"}, later); err != nil {
		t.Fatal(err)
	}
	explicit := later.Add(time.Minute)
	if err := l.RecordGateDecision(GateDecision{GateID: "findings-1", Decision: GateReject, DecidedAt: explicit}, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	hist, err := l.GateHistory()
	if err != nil || len(hist) != 2 {
		t.Fatalf("history = %+v, %v; want 2 entries", hist, err)
	}
	if hist[0].GateID != "design-2" || hist[1].GateID != "findings-1" || !hist[1].DecidedAt.Equal(explicit) {
		t.Fatalf("history order/stamps wrong: %+v", hist)
	}
	dec, _, _ = l.LoadGateDecision()
	if dec.GateID != "findings-1" || dec.Decision != GateReject {
		t.Fatalf("live decision = %+v", dec)
	}
	if entries, _ := os.ReadDir(l.GatesDir()); len(entries) != 2 {
		// decision.json + history.jsonl; no leftover .tmp or pending.json
		t.Fatalf("gates dir = %v", entries)
	}
}

func TestWriteGatePendingFillsIDAndRejectsEmptyQuestion(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	err := l.WriteGatePending(GateRequest{ID: "x"}, gateNow)
	if !errors.Is(err, ErrGateMalformed) {
		t.Fatalf("empty question err = %v, want ErrGateMalformed", err)
	}
	if err := l.WriteGatePending(GateRequest{Question: "q"}, gateNow); err != nil {
		t.Fatal(err)
	}
	got, _, _ := l.LoadGatePending()
	if !strings.HasPrefix(got.ID, gateIDPrefix) || got.ID == gateIDPrefix {
		t.Fatalf("generated id = %q", got.ID)
	}
}

func TestLoadGatePendingMalformed(t *testing.T) {
	for name, body := range map[string]string{
		"not json":    "{",
		"no question": `{"id":"a"}`,
	} {
		t.Run(name, func(t *testing.T) {
			l := New(filepath.Join(t.TempDir(), "nous"))
			write(t, l.GatePendingPath(), body, gateNow)
			_, ok, err := l.LoadGatePending()
			if ok || !errors.Is(err, ErrGateMalformed) {
				t.Fatalf("ok=%v err=%v, want ErrGateMalformed", ok, err)
			}
		})
	}

	// An agent that omits the id gets a stable one derived from the file.
	l := New(filepath.Join(t.TempDir(), "nous"))
	write(t, l.GatePendingPath(), `{"question":"q"}`, gateNow)
	a, ok, err := l.LoadGatePending()
	if err != nil || !ok || a.ID == "" {
		t.Fatalf("id-less gate: %+v ok=%v err=%v", a, ok, err)
	}
	b, _, _ := l.LoadGatePending()
	if a.ID != b.ID {
		t.Fatalf("derived id must be stable: %q vs %q", a.ID, b.ID)
	}

	// An unreadable pending file (a directory) is a plain error, not "no gate".
	l = New(filepath.Join(t.TempDir(), "nous"))
	if err := os.MkdirAll(l.GatePendingPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := l.LoadGatePending(); ok || err == nil || errors.Is(err, ErrGateMalformed) {
		t.Fatalf("directory as pending: ok=%v err=%v", ok, err)
	}
}

func TestRecordGateDecisionValidation(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	for name, d := range map[string]GateDecision{
		"bad decision": {GateID: "g", Decision: "maybe"},
		"no gate id":   {Decision: GateApprove},
	} {
		if err := l.RecordGateDecision(d, gateNow); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := os.Stat(l.GatesDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected decisions must not create files: %v", err)
	}
}

func TestRecordGateDecisionWriteFailures(t *testing.T) {
	// Root is a regular file: the gates dir cannot be created.
	file := filepath.Join(t.TempDir(), "file")
	write(t, file, "x", gateNow)
	if err := New(file).RecordGateDecision(GateDecision{GateID: "g", Decision: GateApprove}, gateNow); err == nil {
		t.Fatal("expected error when gates dir cannot be created")
	}
	if err := New(file).WriteGatePending(GateRequest{Question: "q"}, gateNow); err == nil {
		t.Fatal("expected error when gates dir cannot be created")
	}
	if err := New(file).MarkGateNotified("g"); err == nil {
		t.Fatal("expected error when gates dir cannot be created")
	}

	// History path is a directory: the append fails before anything else.
	l := New(filepath.Join(t.TempDir(), "nous"))
	if err := os.MkdirAll(l.GateHistoryPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, l.GatePendingPath(), `{"id":"g","question":"q"}`, gateNow)
	if err := l.RecordGateDecision(GateDecision{GateID: "g", Decision: GateApprove}, gateNow); err == nil {
		t.Fatal("expected history append error")
	}
	if _, ok, _ := l.LoadGatePending(); !ok {
		t.Fatal("a failed decision must leave the gate pending")
	}

	// Decision path is a directory: history is written, live file fails.
	l = New(filepath.Join(t.TempDir(), "nous"))
	if err := os.MkdirAll(l.GateDecisionPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordGateDecision(GateDecision{GateID: "g", Decision: GateApprove}, gateNow); err == nil {
		t.Fatal("expected decision write error")
	}
	if hist, _ := l.GateHistory(); len(hist) != 1 {
		t.Fatalf("history must be written before the live file: %+v", hist)
	}
}

func TestLoadGateDecisionAndHistoryErrors(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	write(t, l.GateDecisionPath(), "{", gateNow)
	if _, ok, err := l.LoadGateDecision(); ok || err == nil {
		t.Fatalf("corrupt decision: ok=%v err=%v", ok, err)
	}
	if err := os.MkdirAll(filepath.Join(t.TempDir(), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	l2 := New(filepath.Join(t.TempDir(), "nous"))
	if err := os.MkdirAll(l2.GateDecisionPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := l2.LoadGateDecision(); ok || err == nil {
		t.Fatalf("unreadable decision: ok=%v err=%v", ok, err)
	}
	if err := os.MkdirAll(l2.GateHistoryPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.GateHistory(); err == nil {
		t.Fatal("unreadable history must error")
	}

	// Bad lines in history are skipped, good ones kept.
	l3 := New(filepath.Join(t.TempDir(), "nous"))
	write(t, l3.GateHistoryPath(), "garbage\n"+`{"gate_id":"g","decision":"approve"}`+"\n\n", gateNow)
	hist, err := l3.GateHistory()
	if err != nil || len(hist) != 1 || hist[0].GateID != "g" {
		t.Fatalf("history = %+v, %v", hist, err)
	}
}

func TestGateNotifiedMarker(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	if got := l.LastNotifiedGate(); got != "" {
		t.Fatalf("fresh marker = %q", got)
	}
	if err := l.MarkGateNotified("design-1"); err != nil {
		t.Fatal(err)
	}
	if got := l.LastNotifiedGate(); got != "design-1" {
		t.Fatalf("marker = %q", got)
	}
	if err := l.MarkGateNotified("design-2"); err != nil {
		t.Fatal(err)
	}
	if got := l.LastNotifiedGate(); got != "design-2" {
		t.Fatalf("marker after overwrite = %q", got)
	}
}

func TestClearGatePendingMissingIsFine(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	if err := l.ClearGatePending(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.GatePendingPath(), "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.ClearGatePending(); err == nil {
		t.Fatal("removing a non-empty directory must surface an error")
	}
}

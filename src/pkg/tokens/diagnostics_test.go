package tokens

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// Diagnostics is the collector's channel for distinguishing "zero tokens
// because nothing ran" from "zero tokens because metering is broken". These
// tests pin the accessor and the scan paths that populate it — previously the
// accessor had no coverage at all.

func newDiagCollector(t *testing.T, sessionsDir string) *Collector {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := NewCollector(sessionsDir, logger)
	c.SetPersistPath(filepath.Join(t.TempDir(), "token-summary.json"))
	return c
}

func TestDiagnostics_ZeroValueBeforeAnyScan(t *testing.T) {
	c := newDiagCollector(t, t.TempDir())
	if d := c.Diagnostics(); d != (Diagnostics{}) {
		t.Fatalf("fresh collector diagnostics = %+v, want zero value", d)
	}
}

func TestDiagnostics_HealthyScanIsClean(t *testing.T) {
	c := newDiagCollector(t, t.TempDir())
	c.scan()
	if d := c.Diagnostics(); d != (Diagnostics{}) {
		t.Fatalf("healthy scan diagnostics = %+v, want zero value", d)
	}
}

func TestDiagnostics_LiveCaptureFlag(t *testing.T) {
	c := newDiagCollector(t, t.TempDir())
	c.SetCopilotLiveCapture(1_700_000_000_000)
	c.scan()
	if d := c.Diagnostics(); !d.LiveCaptureEnabled {
		t.Fatalf("diagnostics = %+v, want LiveCaptureEnabled=true", d)
	}

	// Disabling live capture must drop the flag on the next scan.
	c.SetCopilotLiveCapture(0)
	c.scan()
	if d := c.Diagnostics(); d.LiveCaptureEnabled {
		t.Fatalf("diagnostics = %+v, want LiveCaptureEnabled=false after disable", d)
	}
}

func TestDiagnostics_ScanErrorIsSurfaced(t *testing.T) {
	// A "[" in the sessions dir makes the *.jsonl glob pattern invalid, which
	// is the only hermetic way to force CollectFromDir to fail.
	c := newDiagCollector(t, filepath.Join(t.TempDir(), "se[ssions"))
	c.scan()
	d := c.Diagnostics()
	if d.LastScanError == "" {
		t.Fatalf("diagnostics = %+v, want non-empty LastScanError", d)
	}
}

func TestDiagnostics_BobScanErrorIsSurfacedThenCleared(t *testing.T) {
	c := newDiagCollector(t, t.TempDir())
	c.SetBobSessionsDir(filepath.Join(t.TempDir(), "bo[b"))
	c.scan()
	if d := c.Diagnostics(); d.LastBobScanError == "" {
		t.Fatalf("diagnostics = %+v, want non-empty LastBobScanError", d)
	}

	// Diagnostics are rebuilt per scan: once the bob dir is healthy the stale
	// error must not linger and mislead the health endpoint.
	c.SetBobSessionsDir(t.TempDir())
	c.scan()
	if d := c.Diagnostics(); d.LastBobScanError != "" {
		t.Fatalf("diagnostics = %+v, want LastBobScanError cleared after healthy scan", d)
	}
}

package nousstore

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.NewFile(0, os.DevNull), &slog.HandlerOptions{Level: slog.LevelError}))
}

func write(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestResolvePrecedence(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	if got := Resolve("", getenv); got.Root != DefaultRoot {
		t.Fatalf("default root = %q, want %q", got.Root, DefaultRoot)
	}
	if got := Resolve("", nil); got.Root != DefaultRoot {
		t.Fatalf("nil getenv root = %q, want %q", got.Root, DefaultRoot)
	}
	if got := Resolve("  /mnt/nous/  ", getenv); got.Root != "/mnt/nous" {
		t.Fatalf("configured root = %q, want /mnt/nous (trimmed, cleaned)", got.Root)
	}
	env[EnvRoot] = "/override/nous"
	if got := Resolve("/mnt/nous", getenv); got.Root != "/override/nous" {
		t.Fatalf("env root = %q, want /override/nous", got.Root)
	}
	env[EnvRoot] = "   "
	if got := Resolve("/mnt/nous", getenv); got.Root != "/mnt/nous" {
		t.Fatalf("blank env must not win: root = %q", got.Root)
	}
}

func TestLayoutPaths(t *testing.T) {
	l := New("/x/nous")
	cases := map[string]string{
		l.GovernorDir():     "/x/nous/governor",
		l.SnapshotDir():     "/x/nous/snapshots",
		l.LedgerPath():      "/x/nous/governor/ledger.json",
		l.PrinciplesPath():  "/x/nous/governor/principles.json",
		l.PanelConfigPath(): "/x/nous/panel-config.json",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path %q, want %q", got, want)
		}
	}
}

func TestEnsureDirs(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{l.GovernorDir(), l.SnapshotDir(), l.GatesDir()} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("%s not a directory: %v", dir, err)
		}
	}
	if err := l.EnsureDirs(); err != nil {
		t.Fatalf("second EnsureDirs must be idempotent: %v", err)
	}

	// A root that is a regular file cannot grow subdirectories.
	file := filepath.Join(t.TempDir(), "file")
	write(t, file, "x", time.Now())
	if err := New(file).EnsureDirs(); err == nil {
		t.Fatal("EnsureDirs under a regular file must fail")
	}
}

func TestMigrateLegacyCopiesMissingAndNewer(t *testing.T) {
	base := t.TempDir()
	legacy := filepath.Join(base, "legacy")
	l := New(filepath.Join(base, "nous"))
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := old.Add(time.Hour)

	// ledger: only in legacy → copied. principles: durable copy is NEWER →
	// kept as is.
	write(t, filepath.Join(legacy, LedgerFile), `{"iterations":[1]}`, old)
	write(t, filepath.Join(legacy, PrinciplesFile), `legacy-principles`, old)
	write(t, l.PrinciplesPath(), `durable-principles`, newer)

	res, err := l.MigrateLegacy(legacy, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Copied) != 1 || res.Copied[0] != l.LedgerPath() {
		t.Fatalf("copied = %v, want only the ledger", res.Copied)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != l.PrinciplesPath() {
		t.Fatalf("skipped = %v, want only the principles", res.Skipped)
	}
	if got := read(t, l.LedgerPath()); got != `{"iterations":[1]}` {
		t.Fatalf("ledger = %q", got)
	}
	if got := read(t, l.PrinciplesPath()); got != `durable-principles` {
		t.Fatalf("newer durable principles were overwritten: %q", got)
	}
	// The legacy files are never removed.
	if _, err := os.Stat(filepath.Join(legacy, LedgerFile)); err != nil {
		t.Fatalf("legacy ledger must survive migration: %v", err)
	}

	// Now the legacy principles file is touched AFTER the durable copy →
	// the forward sync picks it up on the next boot.
	write(t, filepath.Join(legacy, PrinciplesFile), `legacy-principles-v2`, newer.Add(time.Hour))
	res, err = l.MigrateLegacy(legacy, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Copied) != 1 || res.Copied[0] != l.PrinciplesPath() {
		t.Fatalf("second pass copied = %v, want principles", res.Copied)
	}
	if got := read(t, l.PrinciplesPath()); got != `legacy-principles-v2` {
		t.Fatalf("principles = %q, want forward-synced copy", got)
	}

	// Equal mtimes: durable wins (not strictly older).
	res, err = l.MigrateLegacy(legacy, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Copied) != 0 {
		t.Fatalf("third pass must copy nothing, copied %v", res.Copied)
	}
}

func TestMigrateLegacyNoopCases(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))

	// Missing legacy dir: nothing to do, no error.
	res, err := l.MigrateLegacy(filepath.Join(t.TempDir(), "absent"), nil)
	if err != nil || len(res.Copied) != 0 || len(res.Skipped) != len(migratedFiles) {
		t.Fatalf("absent legacy: res=%+v err=%v", res, err)
	}
	// Empty legacy dir disables migration entirely.
	res, err = l.MigrateLegacy("", nil)
	if err != nil || len(res.Copied) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("empty legacy: res=%+v err=%v", res, err)
	}
	// Legacy == durable: nothing to copy onto itself.
	res, err = l.MigrateLegacy(l.GovernorDir(), nil)
	if err != nil || len(res.Copied) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("self legacy: res=%+v err=%v", res, err)
	}
	// A directory named ledger.json in legacy is not a file; skipped.
	legacy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(legacy, LedgerFile), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err = l.MigrateLegacy(legacy, nil)
	if err != nil || len(res.Copied) != 0 {
		t.Fatalf("dir-as-ledger: res=%+v err=%v", res, err)
	}
}

func TestMigrateLegacyReportsWriteFailure(t *testing.T) {
	base := t.TempDir()
	legacy := filepath.Join(base, "legacy")
	write(t, filepath.Join(legacy, LedgerFile), `{}`, time.Now())
	// Durable root is a regular file, so the governor dir cannot be created.
	rootFile := filepath.Join(base, "rootfile")
	write(t, rootFile, "x", time.Now())
	l := New(rootFile)

	res, err := l.MigrateLegacy(legacy, quietLogger())
	if err == nil {
		t.Fatal("expected an error when the durable dir cannot be created")
	}
	if len(res.Copied) != 0 {
		t.Fatalf("nothing should be reported copied on failure: %v", res.Copied)
	}
}

func TestResolveGovernorDir(t *testing.T) {
	base := t.TempDir()
	legacy := filepath.Join(base, "legacy")
	l := New(filepath.Join(base, "nous"))

	// Nothing anywhere → durable dir, no fallback.
	if dir, fb := l.ResolveGovernorDir(legacy); dir != l.GovernorDir() || fb {
		t.Fatalf("empty: %q fallback=%v", dir, fb)
	}
	// Only legacy has a file → fallback.
	write(t, filepath.Join(legacy, PrinciplesFile), `{}`, time.Now())
	if dir, fb := l.ResolveGovernorDir(legacy); dir != legacy || !fb {
		t.Fatalf("legacy-only: %q fallback=%v", dir, fb)
	}
	// Empty legacy arg never falls back.
	if dir, fb := l.ResolveGovernorDir(""); dir != l.GovernorDir() || fb {
		t.Fatalf("no legacy arg: %q fallback=%v", dir, fb)
	}
	// Durable has a file → durable wins even if legacy also has one.
	write(t, l.LedgerPath(), `{}`, time.Now())
	if dir, fb := l.ResolveGovernorDir(legacy); dir != l.GovernorDir() || fb {
		t.Fatalf("durable present: %q fallback=%v", dir, fb)
	}
}

func TestPanelConfigRoundTrip(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))

	if _, ok, err := l.LoadPanelConfig(); ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v, want false/nil", ok, err)
	}

	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("EDT", -4*3600))
	in := PanelConfig{
		Mode:  "suggest",
		Scope: "both",
		Config: map[string]interface{}{
			"goals":    map[string]interface{}{"governor": "reduce MTTR"},
			"schedule": map[string]interface{}{"experiment_duration_hours": float64(4)},
		},
	}
	if err := l.SavePanelConfig(in, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.PanelConfigPath() + tmpSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file must not linger after a successful save: %v", err)
	}

	out, ok, err := l.LoadPanelConfig()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if out.Mode != "suggest" || out.Scope != "both" {
		t.Fatalf("mode/scope = %q/%q", out.Mode, out.Scope)
	}
	if !out.SavedAt.Equal(now) || out.SavedAt.Location() != time.UTC {
		t.Fatalf("saved_at = %v (%v), want %v in UTC", out.SavedAt, out.SavedAt.Location(), now)
	}
	goals, _ := out.Config["goals"].(map[string]interface{})
	if goals["governor"] != "reduce MTTR" {
		t.Fatalf("config round trip lost goals: %+v", out.Config)
	}

	// A second save replaces the first atomically.
	in.Mode = "observe"
	if err := l.SavePanelConfig(in, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	out, _, err = l.LoadPanelConfig()
	if err != nil || out.Mode != "observe" {
		t.Fatalf("second save: mode=%q err=%v", out.Mode, err)
	}
}

func TestPanelConfigErrors(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	write(t, l.PanelConfigPath(), `{not json`, time.Now())
	if _, ok, err := l.LoadPanelConfig(); err == nil || ok {
		t.Fatalf("corrupt file must be an error, got ok=%v err=%v", ok, err)
	}

	// Root path is a regular file → cannot create the root.
	file := filepath.Join(t.TempDir(), "file")
	write(t, file, "x", time.Now())
	if err := New(file).SavePanelConfig(PanelConfig{Mode: "observe"}, time.Now()); err == nil {
		t.Fatal("SavePanelConfig under a regular file must fail")
	}
	// Panel config path is a directory → the read fails with a non-NotExist error.
	l2 := New(t.TempDir())
	if err := os.MkdirAll(l2.PanelConfigPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l2.LoadPanelConfig(); err == nil {
		t.Fatal("reading a directory as panel config must fail")
	}
	// Unmarshalable config value.
	bad := PanelConfig{Config: map[string]interface{}{"ch": make(chan int)}}
	if err := New(t.TempDir()).SavePanelConfig(bad, time.Now()); err == nil {
		t.Fatal("unmarshalable config must fail")
	}
}

func TestWriteAtomicRenameFailureCleansTemp(t *testing.T) {
	dir := t.TempDir()
	// Destination is a non-empty directory: rename of a file onto it fails.
	dst := filepath.Join(dir, "dst")
	if err := os.MkdirAll(filepath.Join(dst, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(dst, []byte("x")); err == nil {
		t.Fatal("rename onto a non-empty directory must fail")
	}
	if _, err := os.Stat(dst + tmpSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file must be removed after a failed rename: %v", err)
	}
	// Unwritable temp location.
	if err := writeAtomic(filepath.Join(dir, "missing", "f"), []byte("x")); err == nil {
		t.Fatal("write into a missing directory must fail")
	}
}

func TestCopyFileReadFailure(t *testing.T) {
	if err := copyFile(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "dst")); err == nil {
		t.Fatal("copying a missing source must fail")
	}
}

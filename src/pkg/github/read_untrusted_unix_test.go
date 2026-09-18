//go:build unix

package github

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// #7576: the drop-box directories accept files of any type from any agent
// UID. These tests assert the reader invariants: a FIFO must not block the
// caller, a symlink must not be followed, an oversize file must be rejected,
// and a plain regular file must round-trip. They run only on unix because
// mkfifo/symlink semantics are the unix attack surface.

func TestReadUntrustedFile_RegularFileRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o640); err != nil {
		t.Fatal(err)
	}
	data, fi, err := readUntrustedFile(path, 1024)
	if err != nil {
		t.Fatalf("regular file rejected: %v", err)
	}
	if string(data) != `{"a":1}` {
		t.Fatalf("content mismatch: %q", data)
	}
	if fi == nil || !fi.Mode().IsRegular() {
		t.Fatalf("expected regular-file FileInfo, got %v", fi)
	}
}

func TestReadUntrustedFile_FIFODoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := syscall.Mkfifo(path, 0o640); err != nil {
		t.Skipf("cannot create FIFO: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := readUntrustedFile(path, 1024)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errDropBoxFileRejected) {
			t.Fatalf("FIFO must be rejected, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readUntrustedFile blocked on a FIFO — this is the #7576 watcher-stall attack")
	}
}

func TestReadUntrustedFile_SymlinkNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret")
	if err := os.WriteFile(target, []byte(`{"stolen":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "x.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	data, _, err := readUntrustedFile(link, 1024)
	if err == nil {
		t.Fatalf("symlink must be rejected, got content %q", data)
	}
	if !errors.Is(err, errDropBoxFileRejected) {
		t.Fatalf("symlink rejection must be errDropBoxFileRejected, got %v", err)
	}
}

func TestReadUntrustedFile_OversizeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.json")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 100)), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readUntrustedFile(path, 99); !errors.Is(err, errDropBoxFileRejected) {
		t.Fatalf("oversize file must be rejected, got %v", err)
	}
}

// The audit ingester must survive hostile spool contents: a FIFO must neither
// block the ingest pass nor survive it, and a symlink must not have its
// target's content laundered into the durable log.
func TestIngestTokenAccessEvents_HostileSpoolFiles(t *testing.T) {
	spool, logPath := testTokenAccessPaths(t)
	if !PrepareTokenAccessAudit(nil) {
		t.Fatal("PrepareTokenAccessAudit failed")
	}

	fifo := filepath.Join(spool, "0-fifo.json")
	if err := syscall.Mkfifo(fifo, 0o640); err != nil {
		t.Skipf("cannot create FIFO: %v", err)
	}
	secret := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(secret, []byte(`{"stolen":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(spool, "1-link.json")); err != nil {
		t.Fatal(err)
	}
	dropTokenAccessEvent(t, spool, "2-real.json", `{"op":"gh","uid":1}`)

	done := make(chan int, 1)
	go func() { done <- IngestTokenAccessEventsOnce(nil, spool, logPath, time.Now()) }()
	var appended int
	select {
	case appended = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ingest pass blocked on hostile spool file (#7576)")
	}
	if appended != 1 {
		t.Fatalf("expected exactly the real event appended, got %d", appended)
	}
	for _, line := range readLogLines(t, logPath) {
		if v, ok := line["stolen"]; ok {
			t.Fatalf("symlink target content laundered into audit log: %v", v)
		}
	}
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Fatalf("hostile or consumed file left in spool: %s", e.Name())
	}
}

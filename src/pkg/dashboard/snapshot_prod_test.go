package dashboard

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBuildSnapshotProdRunsBuilderWithAuthEnv(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(8080, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	s.snapshotDir = dir
	s.authToken = "test-token"
	outputFile := filepath.Join(dir, "snapshot.html")

	orig := runSnapshotBuilder
	t.Cleanup(func() { runSnapshotBuilder = orig })
	var gotArgs []string
	var gotEnv []string
	runSnapshotBuilder = func(args []string, env []string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		gotEnv = append([]string(nil), env...)
		return []byte("ok"), nil
	}

	buildSnapshotProd(s, outputFile, "dark")

	wantArgs := []string{
		"/opt/hive/dashboard/build-snapshot.mjs",
		"--mode", "dark",
		"--base-path", "/snapshot",
		"--html", "/opt/hive/proxy/public/index.html",
		"http://localhost:8080", outputFile,
	}
	if !slices.Equal(gotArgs, wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}
	if !slices.Contains(gotEnv, "NODE_TLS_REJECT_UNAUTHORIZED=0") {
		t.Fatalf("env missing NODE_TLS_REJECT_UNAUTHORIZED: %#v", gotEnv)
	}
	if !slices.Contains(gotEnv, "DASHBOARD_AUTH_TOKEN=test-token") {
		t.Fatalf("env missing auth token: %#v", gotEnv)
	}
}

func TestBuildSnapshotProdBuilderFailureReturns(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(9090, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	s.snapshotDir = dir

	orig := runSnapshotBuilder
	t.Cleanup(func() { runSnapshotBuilder = orig })
	runSnapshotBuilder = func(args []string, env []string) ([]byte, error) {
		return []byte("builder failed"), errors.New("boom")
	}

	buildSnapshotProd(s, filepath.Join(dir, "snapshot.html"), "light")
}

func TestBuildSnapshotProdMkdirFailureSkipsBuilder(t *testing.T) {
	dir := t.TempDir()
	notDir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notDir, []byte("file"), 0o600); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	s := NewServer(9090, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	s.snapshotDir = filepath.Join(notDir, "child")

	orig := runSnapshotBuilder
	t.Cleanup(func() { runSnapshotBuilder = orig })
	runSnapshotBuilder = func(args []string, env []string) ([]byte, error) {
		t.Fatal("builder should not run after snapshot directory creation fails")
		return nil, nil
	}

	buildSnapshotProd(s, filepath.Join(dir, "snapshot.html"), "light")
}

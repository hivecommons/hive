package dashboard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// --- shared helpers for the batch-D test files ---

func testLoggerCovD() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func doPutRawCovD(s *Server, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func doPostRawCovD(s *Server, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

// --- status_builder.go ---

// TestCovD_StatusProviders covers the package-level provider setters and the
// /proc-reading resource collectors (which hit their fallback branches on
// non-Linux CI).
func TestCovD_StatusProviders(t *testing.T) {
	SetBackendAuthProvider(func(backend string) (bool, bool) { return true, true })
	if fn := getBackendAuthFn(); fn == nil {
		t.Fatal("backend auth fn not set")
	} else {
		fn("claude")
	}
	// restore to nil so other tests are unaffected.
	defer SetBackendAuthProvider(nil)

	SetEntitledModelsProvider(func(endpoint string) ([]string, string, bool) {
		return []string{"m"}, "probe", true
	})
	if fn := getEntitledModelsFn(); fn == nil {
		t.Fatal("entitled models fn not set")
	} else {
		fn("http://x")
	}
	defer SetEntitledModelsProvider(nil)

	// readProcMemTotalBytes / collectSystemResources: on macOS /proc is absent,
	// exercising the error/fallback branches. On Linux they read real values.
	_ = readProcMemTotalBytes()
	res := collectSystemResources()
	if res == nil {
		t.Fatal("collectSystemResources returned nil")
	}
}

// --- audit.go ---

// TestCovD_Audit covers AuditLog, auditFromRequest, GetAudit, loadFromDisk,
// Recent, auditDetail, and handleAuditLog role gating.
func TestCovD_Audit(t *testing.T) {
	s := NewServer(0, testLoggerCovD())

	s.AuditLog("bob", "did_thing", "detail", "scanner")
	s.AuditLog("", "system_thing", "", "") // empty user → "system"
	if got := s.GetAudit(); got == nil {
		t.Fatal("GetAudit returned nil")
	}

	// auditFromRequest with and without X-Hive-User header.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Hive-User", "alice")
	s.auditFromRequest(req, "req_action", "d", "")
	s.auditFromRequest(httptest.NewRequest(http.MethodGet, "/y", nil), "req_action2", "", "")

	if r := s.audit.Recent(2); len(r) == 0 {
		t.Error("Recent returned no entries")
	}
	if r := s.audit.Recent(0); len(r) == 0 {
		t.Error("Recent(0) should return all")
	}

	// loadFromDisk on a fresh AuditLog with no file just returns.
	s.audit.loadFromDisk()

	// auditDetail formatting.
	if auditDetail() != "" {
		t.Error("auditDetail() should be empty")
	}
	if auditDetail("k", "v") != "k=v" {
		t.Errorf("auditDetail k=v wrong: %q", auditDetail("k", "v"))
	}
	if auditDetail("a", "1", "b", "2") != "a=1, b=2" {
		t.Errorf("auditDetail multi wrong: %q", auditDetail("a", "1", "b", "2"))
	}

	// handleAuditLog: default owner role → 200; explicit bad role → 403.
	rec := httptest.NewRecorder()
	s.handleAuditLog(rec, httptest.NewRequest(http.MethodGet, "/api/audit", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("audit default role = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req.Header.Set("X-Hive-Role", "read-only")
	s.handleAuditLog(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("audit read-only = %d, want 403", rec.Code)
	}
}

// --- litellm_key_store.go ---

// TestCovD_LiteLLMKeyStore covers writeLiteLLMKeyFile, actionableWriteError,
// patchKeyIntoHiveSecrets (not-in-cluster + token-read error), and
// storeLiteLLMAPIKey (happy path via a temp key file).
func TestCovD_LiteLLMKeyStore(t *testing.T) {
	s := NewServer(0, testLoggerCovD())

	// Point the PVC key file at a temp path.
	oldFile := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(t.TempDir(), "secrets", "litellm_api_key")
	defer func() { writableLiteLLMKeyFile = oldFile }()

	if err := writeLiteLLMKeyFile("sk-litellm"); err != nil {
		t.Fatalf("writeLiteLLMKeyFile: %v", err)
	}
	data, err := os.ReadFile(writableLiteLLMKeyFile)
	if err != nil || string(data) != "sk-litellm" {
		t.Errorf("key file wrong: %q err=%v", string(data), err)
	}

	// storeLiteLLMAPIKey: PVC write succeeds → returns the PVC path; the Secret
	// patch legitimately reports not-in-cluster (no KUBERNETES_SERVICE_HOST).
	path, err := s.storeLiteLLMAPIKey("sk-two")
	if err != nil {
		t.Fatalf("storeLiteLLMAPIKey: %v", err)
	}
	if path != writableLiteLLMKeyFile {
		t.Errorf("store returned %q, want PVC path %q", path, writableLiteLLMKeyFile)
	}

	// actionableWriteError: permission error → operator-readable; generic wrap.
	permErr := actionableWriteError("/x", os.ErrPermission)
	if permErr == nil || !errors.Is(permErr, os.ErrPermission) {
		t.Error("actionableWriteError should wrap the permission error")
	}
	rofsErr := actionableWriteError("/x", syscall.EROFS)
	if rofsErr == nil {
		t.Error("actionableWriteError should handle EROFS")
	}
	genErr := actionableWriteError("/x", errors.New("boom"))
	if genErr == nil {
		t.Error("actionableWriteError should wrap generic errors")
	}

	// patchKeyIntoHiveSecrets: no service host/port → errNotInCluster.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if err := patchKeyIntoHiveSecrets("k", "v"); !errors.Is(err, errNotInCluster) {
		t.Errorf("patchKeyIntoHiveSecrets not-in-cluster = %v", err)
	}

	// With host/port set but a serviceaccount dir missing the token → read error.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	oldSA := serviceAccountDir
	serviceAccountDir = t.TempDir() // empty → token read fails
	defer func() { serviceAccountDir = oldSA }()
	if err := patchKeyIntoHiveSecrets("k", "v"); err == nil || errors.Is(err, errNotInCluster) {
		t.Errorf("patchKeyIntoHiveSecrets should fail reading token, got %v", err)
	}
}

// --- workspace_cleanup.go ---

// TestCovD_WorkspaceHelpers covers isSkippedEntry, isPermError, fileOwnerName,
// and removeTree.
func TestCovD_WorkspaceHelpers(t *testing.T) {
	// isSkippedEntry.
	for _, name := range []string{".cache", "beads.json", ".github", ".hidden"} {
		if !isSkippedEntry(name) {
			t.Errorf("isSkippedEntry(%q) = false, want true", name)
		}
	}
	if isSkippedEntry("realworkdir") {
		t.Error("isSkippedEntry(realworkdir) should be false")
	}

	// isPermError: EPERM-wrapping PathError true; plain error false.
	pe := &os.PathError{Op: "open", Path: "/x", Err: syscall.EPERM}
	if !isPermError(pe) {
		t.Error("isPermError should be true for EPERM PathError")
	}
	if isPermError(errors.New("nope")) {
		t.Error("isPermError should be false for a generic error")
	}

	// fileOwnerName on an existing file → a username, no error.
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, err := fileOwnerName(f); err != nil || name == "" {
		t.Errorf("fileOwnerName = %q, err=%v", name, err)
	}
	if _, err := fileOwnerName(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("fileOwnerName on missing file should error")
	}

	// removeTree on a dir tree → gone.
	root := t.TempDir()
	sub := filepath.Join(root, "tree", "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeTree(filepath.Join(root, "tree")); err != nil {
		t.Fatalf("removeTree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tree")); !os.IsNotExist(err) {
		t.Error("removeTree did not remove the tree")
	}
}

// TestCovD_SweepWorkspaces exercises sweepWorkspaces against a temp workspace
// root: stale dir + stale file removed, skipped + fresh entries preserved.
// Also drives StartWorkspaceCleanup with an already-cancelled context so it
// runs the initial sweep once and returns.
func TestCovD_SweepWorkspaces(t *testing.T) {
	logger := testLoggerCovD()

	// missing root → early Debug+return (default macOS path has no /data/agents).
	oldRoot := agentWorkspaceRoot
	agentWorkspaceRoot = filepath.Join(t.TempDir(), "does-not-exist")
	sweepWorkspaces(logger, nil)

	// Build a real temp workspace tree.
	root := t.TempDir()
	agentWorkspaceRoot = root
	defer func() { agentWorkspaceRoot = oldRoot }()

	agentDir := filepath.Join(root, "scanner")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-directory entry at the agent-root level is skipped by the outer loop.
	if err := os.WriteFile(filepath.Join(root, "loosefile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	stale := 3 * time.Hour
	old := time.Now().Add(-stale)

	staleDir := filepath.Join(agentDir, "staleDir")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleFile := filepath.Join(agentDir, "staleFile")
	if err := os.WriteFile(staleFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	freshFile := filepath.Join(agentDir, "freshFile")
	if err := os.WriteFile(freshFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	skipped := filepath.Join(agentDir, ".cache")
	if err := os.MkdirAll(skipped, 0o755); err != nil {
		t.Fatal(err)
	}
	// Age the stale entries and the skipped one; leave freshFile recent.
	for _, p := range []string{staleDir, staleFile, skipped} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	s := NewServer(0, logger)
	sweepWorkspaces(logger, s.audit)

	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Error("stale dir not removed")
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Error("stale file not removed")
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Error("fresh file should be preserved")
	}
	if _, err := os.Stat(skipped); err != nil {
		t.Error("skipped .cache dir should be preserved")
	}

	// StartWorkspaceCleanup runs the initial sweep then returns on a cancelled ctx.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	StartWorkspaceCleanup(ctx, logger, nil)
}

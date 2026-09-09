package hub

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Persistence-failure hardening for the session revocation store
// (hub_session_revocation.go saveRevokedSessions / loadRevokedSessions).
//
// The happy paths — revoke, persist, survive a restart — are pinned by
// session_f10_f15_test.go. What was NOT pinned is the direction the file's own
// comments call the unsafe one: what happens when the PVC misbehaves. A load
// that cannot read its file must fail CLOSED (reject every v3 session) rather
// than silently forgetting which sessions were revoked, and a save that cannot
// write must degrade to a logged error without panicking or corrupting the
// last good file. These tests pin exactly those branches.

// revocationTestServer builds the minimal HubServer the persistence functions
// need — a logger and a store — with revokedSessionsPath pointed at a caller
// chosen location. Mirrors the "restarted" server shape session_f10_f15_test.go
// already relies on.
func revocationTestServer(t *testing.T, path string) *HubServer {
	t.Helper()
	prev := revokedSessionsPath
	revokedSessionsPath = path
	t.Cleanup(func() { revokedSessionsPath = prev })
	return &HubServer{
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		revokedSessions: newRevokedSessions(),
	}
}

// requireNonRoot skips permission-based failure injection under root, which
// bypasses file modes. Same guard as hub_generations_store_test.go.
func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root — file permissions cannot inject failures")
	}
}

// TestLoadRevokedSessionsUnreadableFileFailsClosed pins the load-failure
// contract: an existing-but-unreadable revocation file must put the store into
// fail-closed mode, where EVERY session id reads as revoked. Answering "not
// revoked" on an I/O error is precisely the F10 bug reintroduced by a
// permissions mistake on the PVC.
func TestLoadRevokedSessionsUnreadableFileFailsClosed(t *testing.T) {
	requireNonRoot(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.json")
	if err := os.WriteFile(path, []byte(`{"sid-a": 9999999999}`), 0o000); err != nil {
		t.Fatal(err)
	}
	s := revocationTestServer(t, path)

	s.loadRevokedSessions()

	now := time.Now()
	if !s.revokedSessions.isRevoked("sid-a", now) {
		t.Error("unreadable revocation file did not fail closed — a session that WAS " +
			"revoked reads as live after an I/O error (F10 reintroduced)")
	}
	if !s.revokedSessions.isRevoked("sid-never-seen", now) {
		t.Error("fail-closed mode must reject EVERY session id, not just known ones — " +
			"the hub does not know which sessions are revoked")
	}
	if lookup := s.hubSessionRevokedLookup(now); lookup == nil || !lookup("anything") {
		t.Error("hubSessionRevokedLookup does not surface fail-closed mode to the cookie verifier")
	}

	// POSITIVE CONTROL: fail-closed is recoverable, not a death sentence. A
	// subsequent successful load (operator fixed the file) clears the mode.
	if s.revokedSessions.load(map[string]int64{}, now) {
		t.Error("loading an empty healthy set reported pruning")
	}
	if s.revokedSessions.isRevoked("sid-never-seen", now) {
		t.Error("successful load did not clear fail-closed mode — everyone stays locked out forever")
	}
}

// TestLoadRevokedSessionsCorruptFileQuarantineRenameFailure: when the file is
// corrupt AND the quarantine rename also fails (read-only dir), the store must
// STILL fail closed. The quarantine is a courtesy for the operator; the
// security posture must not depend on it succeeding.
func TestLoadRevokedSessionsCorruptFileQuarantineRenameFailure(t *testing.T) {
	requireNonRoot(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Read-only dir: the file itself stays readable, but renaming it out fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s := revocationTestServer(t, path)

	s.loadRevokedSessions()

	if !s.revokedSessions.isRevoked("any-sid", time.Now()) {
		t.Error("corrupt file whose quarantine rename failed did not fail closed — " +
			"the unquarantinable corrupt set is silently treated as empty")
	}
	if _, err := os.Stat(path + revokedSessionsQuarantineSuffix); err == nil {
		t.Error("quarantine file exists despite the rename being expected to fail — " +
			"the failure injection did not take, test proves nothing")
	}
}

// TestLoadRevokedSessionsPrunesExpiredAndRewrites pins the load-time prune: an
// entry whose expiry has passed is dropped and the FILE is rewritten without
// it, which is the only thing keeping the persisted set bounded across
// restarts. The live entry must survive both the prune and the rewrite.
func TestLoadRevokedSessionsPrunesExpiredAndRewrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.json")
	now := time.Now()
	stored := map[string]int64{
		"sid-live":    now.Add(time.Hour).Unix(),
		"sid-expired": now.Add(-time.Hour).Unix(),
	}
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s := revocationTestServer(t, path)

	s.loadRevokedSessions()

	if !s.revokedSessions.isRevoked("sid-live", now) {
		t.Error("live entry lost during load-time prune — a revoked session came back")
	}
	if s.revokedSessions.isRevoked("sid-expired", now) {
		t.Error("expired entry survived the load-time prune")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("revocation file unreadable after prune rewrite: %v", err)
	}
	var rewritten map[string]int64
	if err := json.Unmarshal(onDisk, &rewritten); err != nil {
		t.Fatalf("prune rewrite produced invalid JSON: %v", err)
	}
	if _, ok := rewritten["sid-expired"]; ok {
		t.Error("prune did not rewrite the file — expired entries accumulate on the PVC forever")
	}
	if _, ok := rewritten["sid-live"]; !ok {
		t.Error("prune rewrite dropped the LIVE entry — the rewrite un-revoked a session")
	}
}

// TestSaveRevokedSessionsMkdirFailureIsNonFatal: when the parent directory
// cannot be created (a regular file sits where the dir should be), save must
// log and return, not panic. saveRevokedSessions is called from the logout
// request path, so a panic here is a remotely triggerable crash.
func TestSaveRevokedSessionsMkdirFailureIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("i am a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := revocationTestServer(t, filepath.Join(blocker, "sub", "revoked.json"))
	s.revokedSessions.revoke("sid-a", time.Now().Add(time.Hour).Unix(), time.Now())

	s.saveRevokedSessions() // must not panic

	// The in-memory revocation must survive the failed persist: rejecting the
	// session until the next successful flush is the safe direction.
	if !s.revokedSessions.isRevoked("sid-a", time.Now()) {
		t.Error("failed save dropped the in-memory revocation")
	}
}

// TestSaveRevokedSessionsWriteFailureLeavesLastGoodFileIntact: when the tmp
// file cannot be written (read-only dir), the previously persisted set must
// survive byte-for-byte. Truncating or half-writing the real file on a failed
// save would destroy the revocation set a restart depends on.
func TestSaveRevokedSessionsWriteFailureLeavesLastGoodFileIntact(t *testing.T) {
	requireNonRoot(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.json")
	previous := []byte(`{"sid-old": 9999999999}`)
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s := revocationTestServer(t, path)
	s.revokedSessions.revoke("sid-new", time.Now().Add(time.Hour).Unix(), time.Now())

	s.saveRevokedSessions() // must not panic

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("last good revocation file unreadable after failed save: %v", err)
	}
	if string(got) != string(previous) {
		t.Errorf("failed save altered the last good file: got %q, want %q", got, previous)
	}
}

// TestSaveRevokedSessionsRenameFailureIsNonFatal: the tmp write succeeds but
// the rename onto the final path fails (a non-empty directory occupies it).
// Must log and return without panicking.
func TestSaveRevokedSessionsRenameFailureIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.json")
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := revocationTestServer(t, path)
	s.revokedSessions.revoke("sid-a", time.Now().Add(time.Hour).Unix(), time.Now())

	s.saveRevokedSessions() // must not panic

	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Error("rename-failure injection did not hold — target is no longer the blocking directory")
	}
}

// TestRevokedSessionsNilSafety pins every nil-receiver guard in the store and
// on the server-level wrappers. The comments in hub_session_revocation.go
// promise these are safe on hand-built servers; hold them to it, in the safe
// direction (a nil store revokes nothing but also never panics the hub).
func TestRevokedSessionsNilSafety(t *testing.T) {
	now := time.Now()
	var r *revokedSessions
	if r.isRevoked("sid", now) {
		t.Error("nil store claims a session is revoked")
	}
	if r.revoke("sid", now.Add(time.Hour).Unix(), now) {
		t.Error("nil store claims to have revoked")
	}
	if r.atCapacity() {
		t.Error("nil store claims to be at capacity")
	}
	if r.pruneExpired(now) {
		t.Error("nil store claims to have pruned")
	}
	if got := r.snapshot(); len(got) != 0 {
		t.Errorf("nil store snapshot has %d entries", len(got))
	}
	if r.load(map[string]int64{"sid": 1}, now) {
		t.Error("nil store claims to have loaded")
	}
	r.markLoadFailed() // must not panic

	var s *HubServer
	s.saveRevokedSessions()  // must not panic
	s.loadRevokedSessions()  // must not panic
	s.flushRevokedSessions() // must not panic
	if s.revokeHubSessionCookieAt("anything", now) {
		t.Error("nil server claims to have revoked")
	}
	if s.hubSessionRevokedLookup(now) != nil {
		t.Error("nil server returned a revocation lookup")
	}

	// A server whose store is nil (not just a nil server) takes the same guards.
	empty := &HubServer{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	empty.saveRevokedSessions()
	empty.loadRevokedSessions()
	if empty.revokeHubSessionCookieAt("anything", now) {
		t.Error("server with nil store claims to have revoked")
	}
	if empty.hubSessionRevokedLookup(now) != nil {
		t.Error("server with nil store returned a revocation lookup")
	}
}

// TestRevokeHubSessionCookieAtInputGuards pins the value-shape guards on the
// revocation entry point: values that carry no revocable session id — empty,
// garbage, or v2-shaped — must be refused without touching the store, and a
// second revocation of the same cookie must report false (nothing changed)
// rather than pretending to revoke again.
func TestRevokeHubSessionCookieAtInputGuards(t *testing.T) {
	dir := t.TempDir()
	s := revocationTestServer(t, filepath.Join(dir, "revoked.json"))
	now := time.Now()

	for _, value := range []string{"", "garbage", "not.a.cookie"} {
		if s.revokeHubSessionCookieAt(value, now) {
			t.Errorf("revoked a value with no session id: %q", value)
		}
	}
	if got := len(s.revokedSessions.snapshot()); got != 0 {
		t.Errorf("sid-less values grew the store to %d entries", got)
	}

	// POSITIVE CONTROL first, then the duplicate: a real v3 cookie revokes once.
	seed := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	value, sid := mintHubUserCookieValueV3(seed, "alice", now, time.Hour)
	if value == "" || sid == "" {
		t.Fatal("failed to mint a v3 cookie for the positive control")
	}
	if !s.revokeHubSessionCookieAt(value, now) {
		t.Fatal("a well-formed v3 cookie was not revoked — the guards are too strict")
	}
	if !s.revokedSessions.isRevoked(sid, now) {
		t.Fatal("revocation reported success but the sid is not revoked")
	}
	if s.revokeHubSessionCookieAt(value, now) {
		t.Error("revoking the SAME cookie twice reported success — callers use the " +
			"return to decide on disk writes, so duplicates must report false")
	}
	if !s.revokedSessions.isRevoked(sid, now) {
		t.Error("the duplicate revocation un-revoked the session")
	}
}

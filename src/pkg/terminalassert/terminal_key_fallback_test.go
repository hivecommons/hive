package terminalassert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for the lane-3 standalone-spoke fallback key (#6489): when a hive can
// prove neither a hub-injected HIVE_TERMINAL_KEY (lane 1) nor its own per-hive
// identity for the self-derive lane (lane 2, HIVE_HUB_SECRET+HIVE_ID), it falls
// back to a persisted per-instance random key so `Open a terminal` is not
// structurally unavailable on a standalone, non-hub-provisioned docker-compose
// spoke. See SigningKey's lane-3 doc comment for the full design rationale.

// resetFallbackEnv clears every lane so only lane 3 can possibly resolve, and
// points it at a scratch directory so tests never touch a real /data.
func resetFallbackEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv(EnvTerminalKey, "")
	t.Setenv(envHubSecret, "")
	t.Setenv(envHiveID, "")
	t.Setenv(EnvFallbackKeyDir, dir)
}

// TestFallbackKeyGeneratedAndNonEmpty pins the base case: with no hub lane
// configured at all, SigningKey() still resolves to a usable, non-empty key.
func TestFallbackKeyGeneratedAndNonEmpty(t *testing.T) {
	resetFallbackEnv(t, t.TempDir())

	got := SigningKey()
	if got == "" {
		t.Fatal("expected a generated fallback key, got empty")
	}
}

// TestFallbackKeyPersistedAndReusedAcrossRestart pins persistence: a second
// resolution (simulating a fresh process after a restart, since nothing here is
// cached in package state) reads the SAME key back off disk rather than minting
// a new one — otherwise every restart would invalidate the terminal cookie.
func TestFallbackKeyPersistedAndReusedAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	resetFallbackEnv(t, dir)

	first := SigningKey()
	if first == "" {
		t.Fatal("expected a generated fallback key")
	}
	second := SigningKey()
	if second != first {
		t.Fatalf("fallback key not persisted/reused: got %q then %q", first, second)
	}
}

// TestFallbackKeyIgnoresEmptyPersistedFile pins the corrupt/partial-file arm:
// an existing but blank key file must not resolve to an empty signing key. This
// also exercises the generation-race path where O_EXCL observes a file that the
// initial read could not use.
func TestFallbackKeyIgnoresEmptyPersistedFile(t *testing.T) {
	dir := t.TempDir()
	resetFallbackEnv(t, dir)

	path := filepath.Join(dir, fallbackKeyFile)
	if err := os.WriteFile(path, []byte(" \n\t"), 0o600); err != nil {
		t.Fatalf("seed empty fallback file: %v", err)
	}

	if got := SigningKey(); got == "" {
		t.Fatal("blank persisted fallback key resolved as empty instead of generating an in-memory key")
	}
	if data, err := os.ReadFile(path); err != nil {
		t.Fatalf("read seeded fallback file: %v", err)
	} else if strings.TrimSpace(string(data)) != "" {
		t.Fatalf("fallback key file was unexpectedly rewritten: %q", string(data))
	}
}

// TestFallbackKeyReturnsInMemoryKeyWhenDirectoryUnusable pins the
// best-effort persistence contract: a read-only/broken persistence target must
// not make standalone terminals structurally unavailable.
func TestFallbackKeyReturnsInMemoryKeyWhenDirectoryUnusable(t *testing.T) {
	dir := t.TempDir()
	badDir := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(badDir, []byte("blocks MkdirAll"), 0o600); err != nil {
		t.Fatalf("seed non-directory fallback path: %v", err)
	}
	resetFallbackEnv(t, badDir)

	if got := SigningKey(); got == "" {
		t.Fatal("unusable fallback directory must still yield an in-memory key")
	}
	info, err := os.Stat(badDir)
	if err != nil {
		t.Fatalf("stat fallback path: %v", err)
	}
	if info.IsDir() {
		t.Fatal("fallback path unexpectedly became a directory")
	}
}

// TestFallbackKeyFilePermsAreOwnerOnly pins CWE-522: the persisted key file must
// be 0600, never group/world readable — it is a symmetric secret good for
// forging any user's terminal session on this hive.
func TestFallbackKeyFilePermsAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	resetFallbackEnv(t, dir)

	if got := SigningKey(); got == "" {
		t.Fatal("expected a generated fallback key")
	}

	path := filepath.Join(dir, fallbackKeyFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("persisted key file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("persisted key file mode = %o, want 0600", perm)
	}
}

// TestFallbackKeyIsPerInstanceRandomNotDerived pins the N3 invariant this lane
// must uphold: the fallback is per-instance RANDOM, never derived from any
// value every standalone spoke could plausibly share (which would just
// reintroduce a fleet-uniform lane under a new name). Two independent instances
// (distinct directories, i.e. distinct persisted files) must get DIFFERENT
// keys, even though both see identical (empty) env.
func TestFallbackKeyIsPerInstanceRandomNotDerived(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()

	resetFallbackEnv(t, dirA)
	keyA := SigningKey()
	resetFallbackEnv(t, dirB)
	keyB := SigningKey()

	if keyA == "" || keyB == "" {
		t.Fatal("expected generated fallback keys")
	}
	if keyA == keyB {
		t.Fatal("two independent instances derived the SAME fallback key — this is not per-instance random")
	}
}

// TestFallbackKeyPrecedenceBelowHubLanes pins that lane 3 is the LAST resort:
// either hub lane, when present, wins over the persisted fallback — including
// after a fallback file already exists on disk (e.g. a spoke that ran
// standalone for a while and was then hub-provisioned).
func TestFallbackKeyPrecedenceBelowHubLanes(t *testing.T) {
	dir := t.TempDir()
	resetFallbackEnv(t, dir)

	fallback := SigningKey()
	if fallback == "" {
		t.Fatal("expected a generated fallback key")
	}

	// Lane 2 (self-derive) must win over an already-persisted lane-3 file.
	t.Setenv(envHubSecret, "master")
	t.Setenv(envHiveID, "hive-1")
	derived := DerivePerHiveKey("master", InfoKey, "hive-1")
	if got := SigningKey(); got != derived {
		t.Fatalf("self-derive lane did not win over the persisted fallback: got %q, want %q", got, derived)
	}
	if got := SigningKey(); got == fallback {
		t.Fatal("hub lane resolved to the standalone fallback key — precedence is broken")
	}

	// Lane 1 (hub-injected) must win over everything, including lane 2.
	t.Setenv(EnvTerminalKey, "hub-injected-key")
	if got := SigningKey(); got != "hub-injected-key" {
		t.Fatalf("hub-injected key did not win: got %q", got)
	}
}

// TestFallbackKeyDoesNotApplyWhenHubProvisioned is the N3-adjacent guard the
// task calls out explicitly: a hub-provisioned hive (either lane 1 or a
// complete lane 2) must NEVER consult, generate, or touch the lane-3 fallback
// file at all — even one that does not yet exist. A fallback that silently
// activates alongside a working hub lane would be unreachable in practice, but
// pinning "the file is never created" here catches any future refactor that
// reorders the lanes.
func TestFallbackKeyDoesNotApplyWhenHubProvisioned(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvFallbackKeyDir, dir)

	t.Setenv(EnvTerminalKey, "hub-injected-key")
	t.Setenv(envHubSecret, "")
	t.Setenv(envHiveID, "")
	if got := SigningKey(); got != "hub-injected-key" {
		t.Fatalf("lane 1 did not resolve: got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, fallbackKeyFile)); err == nil {
		t.Fatal("lane 3 persisted a fallback file even though lane 1 (hub-injected) resolved")
	}

	t.Setenv(EnvTerminalKey, "")
	t.Setenv(envHubSecret, "master")
	t.Setenv(envHiveID, "hive-1")
	if got, want := SigningKey(), DerivePerHiveKey("master", InfoKey, "hive-1"); got != want {
		t.Fatalf("lane 2 did not resolve: got %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, fallbackKeyFile)); err == nil {
		t.Fatal("lane 3 persisted a fallback file even though lane 2 (self-derive) resolved")
	}
}

// TestFallbackKeyDefaultsUnderHiveDataDir pins the production default location
// (no override): under /data/.hive, alongside the other generated per-instance
// secrets the entrypoint already keeps there.
func TestFallbackKeyDefaultsUnderHiveDataDir(t *testing.T) {
	if fallbackKeyDir() != defaultFallbackKeyDir {
		t.Fatalf("fallbackKeyDir() = %q, want %q when %s is unset", fallbackKeyDir(), defaultFallbackKeyDir, EnvFallbackKeyDir)
	}
	if !strings.HasSuffix(defaultFallbackKeyDir, "/.hive") {
		t.Fatalf("defaultFallbackKeyDir = %q, want the hive private data dir (…/.hive)", defaultFallbackKeyDir)
	}
}

// TestFallbackKeyRoundTripsAMintedAssertion is the end-to-end proof: the
// standalone lane actually lets a terminal open, which is the whole point of
// #6489 — a spoke with no hub lanes at all can still mint AND verify its own
// terminal assertions.
func TestFallbackKeyRoundTripsAMintedAssertion(t *testing.T) {
	resetFallbackEnv(t, t.TempDir())

	key := SigningKey()
	if key == "" {
		t.Fatal("expected a generated fallback key")
	}
	now := time.Now()
	tok := Mint(key, "alice", "owner", "standalone-hive", now)
	if tok == "" {
		t.Fatal("expected a minted assertion under the fallback key")
	}
	user, role, err := Verify(SigningKey(), tok, "standalone-hive", now)
	if err != nil {
		t.Fatalf("verify failed under the (reused) fallback key: %v", err)
	}
	if user != "alice" || role != "owner" {
		t.Fatalf("got user=%q role=%q, want alice/owner", user, role)
	}
}

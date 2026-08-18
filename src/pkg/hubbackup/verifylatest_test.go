package hubbackup

import (
	"path/filepath"
	"strings"
	"testing"
)

// ---- NewObjectStore success path + resolveNamespace ----

// The constructor must resolve the tenancy namespace before the store is usable,
// since every object path embeds it.
func TestNewObjectStoreResolvesNamespace(t *testing.T) {
	srv, _ := ociMock(t, map[string][]byte{})
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)

	store, err := NewObjectStore("backups")
	if err != nil {
		t.Fatalf("NewObjectStore: %v", err)
	}
	if store.namespace != "testnamespace" {
		t.Fatalf("namespace not resolved from the service, got %q", store.namespace)
	}
	if store.bucket != "backups" {
		t.Fatalf("bucket = %q", store.bucket)
	}
}

// A PKCS8-encoded key must be accepted as well as PKCS1 — real oci-api-key
// Secrets carry either form.
func TestNewObjectStoreAcceptsPKCS8Key(t *testing.T) {
	srv, _ := ociMock(t, map[string][]byte{})
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(envOCIPrivateKey, testPKCS8KeyPEM(t))

	if _, err := NewObjectStore("backups"); err != nil {
		t.Fatalf("a PKCS8 private key must be accepted: %v", err)
	}
}

// A namespace lookup failure must fail construction rather than yielding a
// store that writes to path "/n//b/...".
func TestNewObjectStoreFailsWhenNamespaceUnresolvable(t *testing.T) {
	srv := newStatusServer(t, 500)
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)

	if _, err := NewObjectStore("backups"); err == nil {
		t.Fatal("an unresolvable namespace must fail construction")
	}
}

// A non-RSA PKCS8 key must be rejected: the OCI signature scheme is RSA-only.
func TestNewObjectStoreRejectsNonRSAKey(t *testing.T) {
	setOCIEnv(t)
	t.Setenv(envOCIPrivateKey, testED25519KeyPEM(t))
	if _, err := NewObjectStore("backups"); err == nil {
		t.Fatal("a non-RSA key must be rejected")
	}
}

// ---- VerifyLatest ----

// VerifyLatest must fetch the NEWEST archive and verify it end to end.
func TestVerifyLatestVerifiesNewestArchive(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)

	key := decodeTestKey(t)
	good, _, err := Build(key, nil, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The older object is deliberately garbage: if VerifyLatest picks the wrong
	// one, the test fails.
	objects := map[string][]byte{
		"hive-hub-backup-20260101T000000Z.tar.gz.enc": []byte("garbage-old-archive"),
		"hive-hub-backup-20260731T000000Z.tar.gz.enc": good,
	}
	srv, _ := ociMock(t, objects)
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBucket, "backups")

	man, err := VerifyLatest(quietLogger())
	if err != nil {
		t.Fatalf("VerifyLatest: %v", err)
	}
	if man == nil || len(man.Files) == 0 {
		t.Fatal("VerifyLatest returned an empty manifest")
	}
}

// An empty bucket must be an explicit error: "no backups exist" must never be
// reported as a successful verification.
func TestVerifyLatestFailsOnEmptyBucket(t *testing.T) {
	srv, _ := ociMock(t, map[string][]byte{})
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(EnvBucket, "backups")

	_, err := VerifyLatest(quietLogger())
	if err == nil {
		t.Fatal("an empty bucket must be an error, not a silent pass")
	}
	if !strings.Contains(err.Error(), "no archives") {
		t.Errorf("error should say no archives were found, got %v", err)
	}
}

// A stored archive that is corrupt must be reported as such — this is the whole
// point of a verification job.
func TestVerifyLatestDetectsCorruptStoredArchive(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	key := decodeTestKey(t)
	good, _, err := Build(key, nil, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	corrupt := append([]byte(nil), good...)
	corrupt[len(corrupt)/2] ^= 0xFF

	srv, _ := ociMock(t, map[string][]byte{
		"hive-hub-backup-20260731T000000Z.tar.gz.enc": corrupt,
	})
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(EnvBucket, "backups")

	if _, err := VerifyLatest(quietLogger()); err == nil {
		t.Fatal("a corrupt stored archive must fail verification")
	}
}

func TestVerifyLatestRequiresKey(t *testing.T) {
	t.Setenv(EnvBackupKey, "")
	if _, err := VerifyLatest(quietLogger()); err == nil {
		t.Fatal("VerifyLatest without a key must fail")
	}
}

// ---- Run against object storage ----

// The full remote path: upload, read back, verify what actually landed, prune.
// Read-back is what catches a truncated or half-written upload.
func TestRunUploadsVerifiesAndPrunes(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(EnvRetention, "2")

	// Pre-existing archives so Prune has something to trim.
	objects := map[string][]byte{
		"hive-hub-backup-20260101T000000Z.tar.gz.enc": []byte("old1"),
		"hive-hub-backup-20260102T000000Z.tar.gz.enc": []byte("old2"),
	}
	srv, _ := ociMock(t, objects)
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBucket, "backups")

	man, name, err := Run(Options{SkipSpokes: true, SkipSecrets: true}, quietLogger())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if man == nil {
		t.Fatal("Run returned no manifest")
	}
	stored, ok := objects[name]
	if !ok {
		t.Fatalf("archive %q was not uploaded; bucket has %v", name, keysOf(objects))
	}
	if _, err := Verify(decodeTestKey(t), stored); err != nil {
		t.Fatalf("the archive that landed in storage does not verify: %v", err)
	}
	// Retention 2 must be honoured.
	if len(objects) != 2 {
		t.Fatalf("retention=2 must leave 2 archives, got %d: %v", len(objects), keysOf(objects))
	}
	if _, ok := objects[name]; !ok {
		t.Fatal("Prune deleted the archive that was just written")
	}
}

// If the upload fails, Run must report an error rather than claim success.
func TestRunFailsWhenUploadFails(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)

	srv := newNamespaceThenStatusServer(t, 403)
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBucket, "backups")

	_, _, err := Run(Options{SkipSpokes: true, SkipSecrets: true}, quietLogger())
	if err == nil {
		t.Fatal("a failed upload must be reported, not treated as a good backup")
	}
	if !strings.Contains(err.Error(), "upload") {
		t.Errorf("error should identify the upload step, got %v", err)
	}
}

// Missing OCI credentials must fail before any archive work claims success.
func TestRunFailsWithoutObjectStoreCredentials(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(envOCITenancy, "")
	t.Setenv(EnvBucket, "backups")

	if _, _, err := Run(Options{SkipSpokes: true, SkipSecrets: true}, quietLogger()); err == nil {
		t.Fatal("Run must fail when object storage credentials are incomplete")
	}
}

// ---- Build tolerates a partially-broken hub PVC ----

// A hub whose saas dir or registry is missing must still produce an archive:
// a partial backup beats no backup during an incident.
func TestBuildToleratesMissingHubPieces(t *testing.T) {
	dir := t.TempDir() // no saas dir, no registry
	t.Setenv(EnvDataDir, dir)

	sealed, man, err := Build(decodeTestKey(t), nil, nil, quietLogger())
	if err != nil {
		t.Fatalf("Build must tolerate a partial hub PVC: %v", err)
	}
	if man.HubDataDir != dir {
		t.Errorf("manifest should record the data dir, got %q", man.HubDataDir)
	}
	if _, err := Verify(decodeTestKey(t), sealed); err != nil {
		t.Fatalf("even a near-empty archive must verify: %v", err)
	}
}

// Unreadable files inside the tree are skipped rather than aborting the run.
func TestAddTreeSkipsUnreadableFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "saas", "readable.json"), `{"ok":1}`)
	secret := filepath.Join(dir, "saas", "locked.json")
	mustWrite(t, secret, `{"secret":1}`)
	if err := chmodUnreadable(secret); err != nil {
		t.Skipf("cannot make a file unreadable here: %v", err)
	}
	t.Setenv(EnvDataDir, dir)

	sealed, man, err := Build(decodeTestKey(t), nil, nil, quietLogger())
	if err != nil {
		t.Fatalf("an unreadable file must not abort the backup: %v", err)
	}
	if _, err := Verify(decodeTestKey(t), sealed); err != nil {
		t.Fatalf("archive must still verify: %v", err)
	}
	for _, f := range man.Files {
		if strings.HasSuffix(f.Path, "locked.json") {
			t.Fatal("an unreadable file must not be recorded in the manifest")
		}
	}
}

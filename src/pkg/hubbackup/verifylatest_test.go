package hubbackup

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

func fakeBackupKubectl(t *testing.T, pod string) {
	t.Helper()
	fakeKubectl(t, map[string]string{
		"get secret": `{"kind":"Secret","data":{}}`,
		"get pods":   pod,
		"exec":       spokeStream(map[string]string{spokeRuntimeConfigFile: "project: fixture"}),
	})
}

func TestRunProtectsLegacyUntilFullRetentionIsReplaced(t *testing.T) {
	t.Setenv(EnvDataDir, seedHubData(t))
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(EnvRetention, "2")
	fakeBackupKubectl(t, "pod")

	key := decodeTestKey(t)
	spokes := fakeSpokes{spokes: []SpokeConfig{
		{ID: "hosted-x", Files: map[string][]byte{spokeRuntimeConfigFile: []byte("project: fixture")}},
	}}
	good, _, err := Build(key, spokes, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	older, newer := ArchiveName(mustTime(t, "2026-01-01T00:00:00Z")), ArchiveName(mustTime(t, "2026-01-02T00:00:00Z"))
	objects := map[string][]byte{older: good, newer: []byte("incomplete legacy archive")}
	srv, _ := ociMock(t, objects)
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBucket, "backups")

	_, first, err := Run(Options{}, quietLogger())
	if err != nil || !strings.HasPrefix(first, fleetArchivePrefix) {
		t.Fatalf("publish first fleet backup: %q, %v", first, err)
	}
	if len(objects) != 3 || objects[older] == nil || objects[newer] == nil {
		t.Fatalf("one verified fleet backup cannot replace both legacy points: %v", keysOf(objects))
	}

	_, second, err := Run(Options{}, quietLogger())
	if err != nil || first == second {
		t.Fatalf("publish second fleet backup: %q, %v", second, err)
	}
	if len(objects) != 2 || objects[first] == nil || objects[second] == nil {
		t.Fatalf("retention should now contain two published backups: %v", keysOf(objects))
	}
	if man, err := Verify(key, objects[second]); err != nil || len(man.SpokeIDs) != 1 {
		t.Fatalf("published archive should contain the captured spoke: %+v, %v", man, err)
	}
}

func TestFailedReadBackKeepsArchiveOutOfRetention(t *testing.T) {
	t.Setenv(EnvDataDir, seedHubData(t))
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(EnvRetention, "1")
	fakeBackupKubectl(t, "pod")

	old := ArchiveName(mustTime(t, "2026-01-01T00:00:00Z"))
	objects := map[string][]byte{old: []byte("older recovery point")}
	var requests []string
	handler := ociBucketHandler(t, objects, &requests)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, stagingArchivePrefix) {
			_, _ = w.Write([]byte("wrong archive bytes"))
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBucket, "backups")

	if _, _, err := Run(Options{}, quietLogger()); err == nil || !strings.Contains(err.Error(), "read-back differs") {
		t.Fatalf("expected failed read-back, got %v", err)
	}
	if len(objects) != 1 || objects[old] == nil {
		t.Fatalf("read-back failure disturbed retention: %v", keysOf(objects))
	}
	for _, request := range requests {
		if (strings.HasPrefix(request, "PUT ") && !strings.Contains(request, stagingArchivePrefix)) || strings.HasSuffix(request, "/o") {
			t.Fatalf("unverified run attempted publication or retention: %v", requests)
		}
	}
}

func TestIncompleteHubBackupLeavesRemoteRetentionAlone(t *testing.T) {
	for _, filename := range []string{credentialKeyFile, heartbeatMasterFile} {
		t.Run(filename, func(t *testing.T) {
			dir := seedHubData(t)
			t.Setenv(EnvDataDir, dir)
			t.Setenv(EnvBackupKey, testKey)
			fakeBackupKubectl(t, "pod")
			t.Setenv(EnvRetention, "2")

			good, _, err := Build(decodeTestKey(t), nil, nil, quietLogger())
			if err != nil {
				t.Fatal(err)
			}
			objects := map[string][]byte{
				"hive-hub-backup-20260101T000000Z.tar.gz.enc": good,
				"hive-hub-backup-20260102T000000Z.tar.gz.enc": good,
			}
			srv, requests := ociMock(t, objects)
			setOCIEnv(t)
			t.Setenv(envOCIEndpoint, srv.URL)
			t.Setenv(EnvBucket, "backups")
			if err := os.Remove(filepath.Join(dir, saasSubdir, filename)); err != nil {
				t.Fatal(err)
			}

			_, _, err = Run(Options{SkipSpokes: true}, quietLogger())
			if err == nil || !strings.Contains(err.Error(), filepath.Join(hubPrefix, saasSubdir, filename)) {
				t.Fatalf("Run must identify the missing key, got %v", err)
			}
			if len(*requests) > 0 {
				t.Fatalf("partial backup contacted object storage: %v", *requests)
			}
			if len(objects) != 2 {
				t.Fatalf("usable backup count changed: %v", keysOf(objects))
			}
		})
	}
}

func TestHubOnlyBackupRequiresClusterRegistryForNonDefaultCluster(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)
	fakeBackupKubectl(t, "pod")
	t.Setenv(envOCITenancy, "") // a regression must never contact real storage
	if err := os.Remove(filepath.Join(dir, saasSubdir, clustersFile)); err != nil {
		t.Fatal(err)
	}
	_, def, err := Build(decodeTestKey(t), nil, nil, quietLogger())
	if err != nil || len(def.HubErrors) > 0 {
		t.Fatalf("the default hive-oke cluster does not need a file: %+v, %v", def, err)
	}

	mustWrite(t, filepath.Join(dir, registryFile), `{"hives":[{"id":"hosted-x","clusterId":"custom"}]}`)
	_, _, err = Run(Options{SkipSpokes: true}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "hub/saas/clusters.json") {
		t.Fatalf("a hub-only run must still capture its non-default cluster registry: %v", err)
	}
}

func TestHubOnlyAndPartialBackupsHaveTheirOwnHistory(t *testing.T) {
	t.Setenv(EnvDataDir, seedHubData(t))
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(EnvRetention, "2")

	objects := make(map[string][]byte)
	srv, _ := ociMock(t, objects)
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBucket, "backups")

	fakeBackupKubectl(t, "pod")
	run := func(opts Options, prefix string) string {
		t.Helper()
		_, name, err := Run(opts, quietLogger())
		if err != nil || !strings.HasPrefix(name, prefix) {
			t.Fatalf("expected %s archive, got %q, %v", prefix, name, err)
		}
		return name
	}
	fleetOld := run(Options{}, fleetArchivePrefix)
	fleetNew := run(Options{}, fleetArchivePrefix)

	fakeBackupKubectl(t, "")
	partialOld := run(Options{}, partialArchivePrefix)
	partialNew := run(Options{}, partialArchivePrefix)
	if len(objects) != 4 || objects[partialOld] == nil || objects[partialNew] == nil {
		t.Fatalf("consecutive spoke failures should leave two recovery points: %v", keysOf(objects))
	}
	latest, err := VerifyLatest(quietLogger())
	if err != nil || latest.SpokeErrors["hosted-x"] == "" {
		t.Fatalf("verify latest should show the current spoke gap: %+v, %v", latest, err)
	}

	hubOld := run(Options{SkipSpokes: true}, hubOnlyArchivePrefix)
	hubMid := run(Options{SkipSpokes: true}, hubOnlyArchivePrefix)
	hubNew := run(Options{SkipSpokes: true}, hubOnlyArchivePrefix)
	if len(objects) != 6 || objects[fleetOld] == nil || objects[fleetNew] == nil ||
		objects[partialOld] == nil || objects[partialNew] == nil ||
		objects[hubMid] == nil || objects[hubNew] == nil || objects[hubOld] != nil {
		t.Fatalf("hub-only run should prune its own history only: %v", keysOf(objects))
	}

	latest, err = VerifyLatest(quietLogger())
	if err != nil || !latest.SpokesSkipped {
		t.Fatalf("latest archive should disclose the intentional spoke omission: %+v, %v", latest, err)
	}
}

func TestHubOnlyRunRefusesUnusableTopology(t *testing.T) {
	clusterFile := filepath.Join(saasSubdir, clustersFile)
	for _, tt := range []struct{ file, contents, gap string }{
		{clusterFile, "{bad json", clusterFile},
		{clusterFile, "[]", clusterFile},
		{clusterFile, `[{"id":"hive-oke","in_cluster":true}]`, clusterFile},
		{registryFile, "{bad json", registryFile},
		{registryFile, "null", registryFile},
		{registryFile, `{}`, registryFile},
		{registryFile, `{"hives":[{"id":"hosted-x","clusterId":"missing","namespace":"hive-hosted-hosted-x"}]}`, clusterFile},
	} {
		t.Run(tt.contents, func(t *testing.T) {
			dir := seedHubData(t)
			t.Setenv(EnvDataDir, dir)
			t.Setenv(EnvBackupKey, testKey)
			t.Setenv(envOCITenancy, "")
			fakeBackupKubectl(t, "pod")
			mustWrite(t, filepath.Join(dir, tt.file), tt.contents)

			_, _, err := Run(Options{SkipSpokes: true}, quietLogger())
			if err == nil || !strings.Contains(err.Error(), "refusing to upload incomplete hub backup") ||
				!strings.Contains(err.Error(), filepath.Join(hubPrefix, tt.gap)) {
				t.Fatalf("hub-only upload should identify unusable topology, got %v", err)
			}
		})
	}
}

func TestRunRefusesRegistryRejectedByRestoreSchema(t *testing.T) {
	t.Setenv(EnvDataDir, seedHubData(t))
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(envOCITenancy, "")
	fakeBackupKubectl(t, "pod")
	_, _, err := Run(Options{
		SkipSpokes: true,
		ValidateFleetRegistry: func(data []byte) error {
			if !strings.Contains(string(data), "hosted-x") {
				t.Fatalf("validator should receive the captured registry, got %q", data)
			}
			return fmt.Errorf("restore could not parse a registry field")
		},
	}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "restore could not parse a registry field") {
		t.Fatalf("invalid restore state must block publication: %v", err)
	}
}

// If the upload fails, Run must report an error rather than claim success.
func TestRunFailsWhenUploadFails(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)
	fakeBackupKubectl(t, "pod")

	srv := newNamespaceThenStatusServer(t, 403)
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBucket, "backups")

	_, _, err := Run(Options{SkipSpokes: true}, quietLogger())
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
	fakeBackupKubectl(t, "pod")
	t.Setenv(envOCITenancy, "")
	t.Setenv(EnvBucket, "backups")

	if _, _, err := Run(Options{SkipSpokes: true}, quietLogger()); err == nil {
		t.Fatal("Run must fail when object storage credentials are incomplete")
	}
}

func TestRunRefusesRemoteBackupWithoutKubernetesSecrets(t *testing.T) {
	t.Setenv(EnvDataDir, seedHubData(t))
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(envOCITenancy, "")

	_, _, err := Run(Options{SkipSpokes: true, SkipSecrets: true}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "backup without Kubernetes Secrets") {
		t.Fatalf("an archive without Kubernetes Secrets must stay local: %v", err)
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
	verified, err := Verify(decodeTestKey(t), sealed)
	if err != nil {
		t.Fatalf("even a near-empty archive must verify: %v", err)
	}
	for _, path := range []string{"hub/saas", "hub/hub-registry.json"} {
		if verified.HubErrors[path] == "" {
			t.Errorf("verified manifest did not record missing %s", path)
		}
	}
}

func TestBuildAllowsMissingCredentialKeyWithoutStoredTokens(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	mustWrite(t, filepath.Join(dir, saasSubdir, "users", "alice.json"), `{"canonical_id":"google:123"}`)
	if err := os.Remove(filepath.Join(dir, saasSubdir, credentialKeyFile)); err != nil {
		t.Fatal(err)
	}

	_, man, err := Build(decodeTestKey(t), nil, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(man.HubErrors) > 0 {
		t.Fatalf("a hub without stored credentials does not need hmac.key: %v", man.HubErrors)
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
	if man.HubErrors["hub/saas/locked.json"] == "" {
		t.Fatal("the unreadable file must be recorded as a hub gap")
	}
	for _, f := range man.Files {
		if strings.HasSuffix(f.Path, "locked.json") {
			t.Fatal("an unreadable file must not be recorded in the manifest")
		}
	}
}

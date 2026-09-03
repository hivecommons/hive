package hubbackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// testKey is a deterministic non-secret AES-256 key used only by tests.
const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// seedHubData builds a fake hub PVC layout mirroring the real one, including
// the excluded scratch directories, and returns its path.
func seedHubData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	saas := filepath.Join(dir, "saas")

	mustWrite(t, filepath.Join(saas, "hmac.key"), strings.Repeat("k", 32))
	mustWrite(t, filepath.Join(saas, "hub-secret.key"), strings.Repeat("s", 64))
	mustWrite(t, filepath.Join(saas, "webhook-secret.key"), "webhooksecret")
	mustWrite(t, filepath.Join(saas, "app-keys", "vllm-d.pem"), "-----BEGIN RSA PRIVATE KEY-----\nfake\n")
	mustWrite(t, filepath.Join(saas, "users", "alice.json"),
		`{"github_username":"alice","encrypted_token":"ZmFrZQ==","hives":{"hosted-x":"owner"}}`)
	mustWrite(t, filepath.Join(saas, "hives", "hosted-x", "meta.json"),
		`{"id":"hosted-x","org":"acme","acmm_level":3}`)
	mustWrite(t, filepath.Join(saas, "clusters.json"),
		`[{"id":"hive-oke","in_cluster":true},{"id":"vllm-d","in_cluster":false,`+
			`"kubeconfig_path":"/etc/hive/kubeconfigs/vllm-d.yaml","context":"vllm-d"}]`)
	mustWrite(t, filepath.Join(dir, "hub-registry.json"),
		`{"hives":[{"id":"hosted-x","clusterId":"hive-oke"}],"updatedAt":"now"}`)

	// Excluded scratch directories — these must NOT appear in the archive.
	mustWrite(t, filepath.Join(dir, "nous", "big.bin"), strings.Repeat("N", 4096))
	mustWrite(t, filepath.Join(dir, "home", "agent", "cache.bin"), strings.Repeat("H", 4096))
	mustWrite(t, filepath.Join(dir, "beads", "b.db"), strings.Repeat("B", 4096))
	mustWrite(t, filepath.Join(dir, "logs", "hive.log"), strings.Repeat("L", 4096))
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeTestKey(t *testing.T) []byte {
	t.Helper()
	k, err := hex.DecodeString(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestLoadKeyRejectsUnset is the guard against silently writing plaintext.
//
// The assertion is deliberately on the MESSAGE, not merely on "an error
// happened": an unset key also trips the hex/length checks further down, so a
// test that accepts any error still passes when the explicit unset branch is
// removed. The distinct message is what tells an operator their backup would
// otherwise have been written unencrypted.
func TestLoadKeyRejectsUnset(t *testing.T) {
	t.Setenv(EnvBackupKey, "")
	_, err := LoadKey()
	if err == nil {
		t.Fatal("expected error when HIVE_BACKUP_KEY is unset, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to write an unencrypted backup") {
		t.Fatalf("an unset key must be refused explicitly, not incidentally via a "+
			"decode failure; got %v", err)
	}
}

// Whitespace-only is the same hazard as unset: a Secret mounted with just a
// newline must not be treated as a usable key.
func TestLoadKeyRejectsWhitespaceOnly(t *testing.T) {
	t.Setenv(EnvBackupKey, "   \n\t ")
	_, err := LoadKey()
	if err == nil {
		t.Fatal("a whitespace-only key must be refused")
	}
	if !strings.Contains(err.Error(), "refusing to write an unencrypted backup") {
		t.Fatalf("want the explicit unset refusal, got %v", err)
	}
}

func TestLoadKeyRejectsWrongLength(t *testing.T) {
	t.Setenv(EnvBackupKey, "abcd")
	if _, err := LoadKey(); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestLoadKeyAccceptsValid(t *testing.T) {
	t.Setenv(EnvBackupKey, testKey)
	k, err := LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != aesKeySize {
		t.Fatalf("key len = %d, want %d", len(k), aesKeySize)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	key := decodeTestKey(t)
	plain := []byte("sensitive hub state")
	sealed, err := Seal(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "sensitive") {
		t.Fatal("plaintext leaked into sealed output")
	}
	got, err := Open(key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Fatalf("round trip mismatch: %q", got)
	}
}

// TestOpenWithWrongKeyFails proves the archive is useless without the escrowed key.
func TestOpenWithWrongKeyFails(t *testing.T) {
	key := decodeTestKey(t)
	sealed, err := Seal(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := make([]byte, aesKeySize)
	if _, err := Open(wrong, sealed); err == nil {
		t.Fatal("expected decryption failure with wrong key")
	}
}

// TestTamperDetected proves GCM catches modification of a stored archive.
func TestTamperDetected(t *testing.T) {
	key := decodeTestKey(t)
	sealed, err := Seal(key, []byte("important"))
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 0xFF
	if _, err := Open(key, sealed); err == nil {
		t.Fatal("expected tamper detection")
	}
}

// TestBuildCapturesHubStateAndExcludesScratch is the core scope assertion.
func TestBuildCapturesHubStateAndExcludesScratch(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	key := decodeTestKey(t)

	sealed, man, err := Build(key, nil, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	paths := map[string]bool{}
	for _, f := range man.Files {
		paths[f.Path] = true
	}

	// Must include the unrecoverable secrets and state.
	for _, want := range []string{
		"hub/saas/hmac.key",
		"hub/saas/hub-secret.key",
		"hub/saas/webhook-secret.key",
		"hub/saas/app-keys/vllm-d.pem",
		"hub/saas/users/alice.json",
		"hub/saas/hives/hosted-x/meta.json",
		"hub/hub-registry.json",
	} {
		if !paths[want] {
			t.Errorf("archive missing required file %q", want)
		}
	}

	// Must exclude regenerable scratch.
	for p := range paths {
		for _, bad := range []string{"hub/nous/", "hub/home/", "hub/beads/", "hub/logs/"} {
			if strings.HasPrefix(p, bad) {
				t.Errorf("archive should not contain scratch path %q", p)
			}
		}
	}

	if _, err := Verify(key, sealed); err != nil {
		t.Fatalf("verify failed: %v", err)
	}
}

// TestManifestListsEveryArchivedFile is a regression guard: an earlier version
// serialized the manifest without copying the accumulated digests, so Files was
// empty and Verify passed vacuously — a backup that verified but checked nothing.
func TestManifestIsNotEmpty(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	key := decodeTestKey(t)

	_, man, err := Build(key, nil, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Files) == 0 {
		t.Fatal("manifest Files is empty: verification would be vacuous")
	}
	for _, f := range man.Files {
		if f.SHA256 == "" {
			t.Fatalf("manifest entry %q has no checksum", f.Path)
		}
	}
}

// TestVerifyDetectsCorruption proves integrity checking actually works.
func TestVerifyDetectsCorruption(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	key := decodeTestKey(t)

	sealed, _, err := Build(key, nil, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)/2] ^= 0x01
	if _, err := Verify(key, sealed); err == nil {
		t.Fatal("expected verification failure on corrupted archive")
	}
}

// fakeSpokes returns a collector producing deterministic spoke data.
type fakeSpokes struct{ spokes []SpokeConfig }

func (f fakeSpokes) Collect(_ *slog.Logger) ([]SpokeConfig, error) { return f.spokes, nil }

type fakeSecrets struct{ items []SecretItem }

func (f fakeSecrets) Collect(_ *slog.Logger) ([]SecretItem, error) { return f.items, nil }

// TestBuildIncludesSpokesAndSecrets covers the full-fleet archive shape.
func TestBuildIncludesSpokesAndSecrets(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	key := decodeTestKey(t)

	spokes := fakeSpokes{spokes: []SpokeConfig{
		{ID: "hosted-x", Files: map[string][]byte{
			"hive.yaml.runtime":   []byte("project:\n  org: acme\n"),
			"hive-id":             []byte("hosted-x"),
			"gh-app-key-5686.pem": []byte("-----BEGIN RSA PRIVATE KEY-----\n"),
		}},
		{ID: "hosted-broken", Err: "no running pod (scaled to zero?)"},
	}}
	secrets := fakeSecrets{items: []SecretItem{
		{Name: "hive-hub-secrets", JSON: []byte(`{"kind":"Secret"}`)},
		{Name: "oci-api-key", JSON: []byte(`{"kind":"Secret"}`)},
	}}

	sealed, man, err := Build(key, spokes, secrets, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	if len(man.SpokeIDs) != 1 || man.SpokeIDs[0] != "hosted-x" {
		t.Fatalf("SpokeIDs = %v, want [hosted-x]", man.SpokeIDs)
	}
	// A failed spoke must be recorded, not silently dropped.
	if _, ok := man.SpokeErrors["hosted-broken"]; !ok {
		t.Fatal("expected hosted-broken recorded in SpokeErrors")
	}
	if len(man.SecretNames) != 2 {
		t.Fatalf("SecretNames = %v", man.SecretNames)
	}

	paths := map[string]bool{}
	for _, f := range man.Files {
		paths[f.Path] = true
	}
	for _, want := range []string{
		"spokes/hosted-x/hive.yaml.runtime",
		"spokes/hosted-x/hive-id",
		"spokes/hosted-x/gh-app-key-5686.pem",
		"secrets/hive-hub-secrets.json",
		"secrets/oci-api-key.json",
	} {
		if !paths[want] {
			t.Errorf("archive missing %q", want)
		}
	}
	if _, err := Verify(key, sealed); err != nil {
		t.Fatal(err)
	}
}

// TestExtractRestoresContent is the restore-path proof: bytes written into the
// archive come back out byte-identical.
func TestExtractRestoresContent(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	key := decodeTestKey(t)

	spokeYAML := []byte("project:\n  org: acme\n  repos:\n  - thing\n")
	spokes := fakeSpokes{spokes: []SpokeConfig{
		{ID: "hosted-x", Files: map[string][]byte{
			"hive.yaml.runtime": spokeYAML,
			"hive-id":           []byte("hosted-x"),
		}},
	}}
	sealed, _, err := Build(key, spokes, nil, quietLogger())
	if err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if _, err := Extract(key, sealed, dest); err != nil {
		t.Fatal(err)
	}

	// The hmac.key must survive byte-for-byte: it is the only thing that can
	// decrypt every user's GitHub token.
	gotHMAC, err := os.ReadFile(filepath.Join(dest, "hub", "saas", "hmac.key"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotHMAC) != strings.Repeat("k", 32) {
		t.Fatalf("hmac.key corrupted through backup/restore: %q", gotHMAC)
	}

	// The authoritative spoke config must round-trip exactly.
	gotYAML, err := os.ReadFile(filepath.Join(dest, "spokes", "hosted-x", "hive.yaml.runtime"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotYAML) != string(spokeYAML) {
		t.Fatalf("hive.yaml.runtime corrupted: %q", gotYAML)
	}
}

// TestExtractRejectsPathTraversal guards restore against a malicious archive.
func TestExtractRejectsPathTraversal(t *testing.T) {
	key := decodeTestKey(t)
	evil := buildEvilArchive(t, key)
	if _, err := Extract(key, evil, t.TempDir()); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
}

func TestParseSpokeStream(t *testing.T) {
	// base64 of "hello" is aGVsbG8=
	raw := spokeStreamPrefix + "hive-id" + spokeStreamDelim + "\naGVsbG8=\n"
	files, err := parseSpokeStream([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(files["hive-id"]) != "hello" {
		t.Fatalf("got %q", files["hive-id"])
	}
}

// TestSpokeReadScriptTargetsAuthoritativeConfig locks in the single most
// dangerous detail: the backup must read hive.yaml.runtime, not hive.yaml.
// Reading the wrong file makes restore silently fall back to the stale
// ConfigMap seed with no error.
func TestSpokeReadScriptTargetsAuthoritativeConfig(t *testing.T) {
	script := buildSpokeReadScript()
	if !strings.Contains(script, "/data/hive.yaml.runtime") {
		t.Fatal("spoke read script must capture /data/hive.yaml.runtime")
	}
	if !strings.Contains(script, "gh-app-key*.pem") {
		t.Fatal("spoke read script must capture GitHub App keys")
	}
	if !strings.Contains(script, "/data/hive-id") {
		t.Fatal("spoke read script must capture hive-id")
	}
}

func TestRetentionDefaultAndOverride(t *testing.T) {
	t.Setenv(EnvRetention, "")
	if got := Retention(); got != DefaultRetention {
		t.Fatalf("Retention() = %d, want %d", got, DefaultRetention)
	}
	t.Setenv(EnvRetention, "7")
	if got := Retention(); got != 7 {
		t.Fatalf("Retention() = %d, want 7", got)
	}
}

func TestLoadTargets(t *testing.T) {
	dir := seedHubData(t)
	clusters, hives, err := LoadTargets(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("clusters = %d, want 2", len(clusters))
	}
	if clusters["vllm-d"].InCluster {
		t.Fatal("vllm-d should not be in-cluster")
	}
	if clusters["vllm-d"].KubeconfigPath == "" {
		t.Fatal("vllm-d needs a kubeconfig path to be reachable")
	}
	if hives["hosted-x"] != "hive-oke" {
		t.Fatalf("hives[hosted-x] = %q", hives["hosted-x"])
	}
}

func TestKubectlArgsForRemoteCluster(t *testing.T) {
	remote := ClusterTarget{ID: "vllm-d", InCluster: false,
		KubeconfigPath: "/etc/hive/kubeconfigs/vllm-d.yaml", Context: "vllm-d"}
	args := remote.kubectlArgs("get", "pods")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--kubeconfig /etc/hive/kubeconfigs/vllm-d.yaml") {
		t.Fatalf("missing kubeconfig flag: %v", args)
	}
	if !strings.Contains(joined, "--context vllm-d") {
		t.Fatalf("missing context flag: %v", args)
	}

	inCluster := ClusterTarget{ID: "hive-oke", InCluster: true}
	if got := strings.Join(inCluster.kubectlArgs("get", "pods"), " "); got != "get pods" {
		t.Fatalf("in-cluster args = %q, want %q", got, "get pods")
	}
}

func TestStripSecretNoise(t *testing.T) {
	raw := []byte(`{"kind":"Secret","metadata":{"name":"x","uid":"u1","resourceVersion":"9","creationTimestamp":"t"},"data":{"a":"b"}}`)
	out, err := stripSecretNoise(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{"resourceVersion", "uid", "creationTimestamp"} {
		if strings.Contains(s, bad) {
			t.Errorf("stripped output still contains %q", bad)
		}
	}
	if !strings.Contains(s, `"data"`) || !strings.Contains(s, `"name": "x"`) {
		t.Errorf("stripped output lost required fields: %s", s)
	}
}

// sealTar builds a sealed archive from raw tar members, so a test can present
// an archive the hub did NOT produce — i.e. an attacker-supplied one. Extract
// runs on operator-supplied files, so the header fields are untrusted input.
func sealTar(t *testing.T, key []byte, members []*tar.Header, bodies [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// A manifest keeps Verify happy.
	man, _ := json.Marshal(&Manifest{FormatVersion: backupFormatVersion})
	all := append([]*tar.Header{{Name: manifestName, Mode: 0o600, Size: int64(len(man))}}, members...)
	allBodies := append([][]byte{man}, bodies...)
	for i, h := range all {
		h.Size = int64(len(allBodies[i]))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(allBodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(key, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// Audit §8 (prior N18). The restore loop wrote every member with
// os.FileMode(hdr.Mode) — a field the archive controls. Verified empirically:
// under a permissive umask, a header carrying 0666/0777 restores a
// WORLD-WRITABLE file, so any local user can then rewrite restored hub state.
//
// A umask is a deployment accident, not a security control, so the mask belongs
// in the code.
func TestN18_RestoredFileIsNotGroupOrWorldWritable(t *testing.T) {
	key := decodeTestKey(t)
	sealed := sealTar(t,
		key,
		[]*tar.Header{{Name: "hub/evil.txt", Mode: 0o777}},
		[][]byte{[]byte("payload")},
	)

	// Clear the process umask for the duration of the extract. Without this the
	// test passes against the VULNERABLE code on any machine with a normal
	// umask (022), because the kernel strips the world bits before they hit
	// disk — proving nothing about the code. A umask is a deployment default,
	// not a guarantee, so the assertion must hold without one.
	oldMask := syscall.Umask(0)
	dest := t.TempDir()
	_, err := Extract(key, sealed, dest)
	syscall.Umask(oldMask)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	st, err := os.Stat(filepath.Join(dest, "hub", "evil.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm&0o022 != 0 {
		t.Fatalf("N18: archive-controlled mode produced a group/world-writable restored file: %04o", perm)
	}
}

// POSITIVE CONTROL: masking must not break restore. The file must still exist,
// still be readable, and still hold its exact bytes — otherwise the test above
// would pass against a restore that wrote nothing at all.
func TestN18_RestoreStillWritesReadableContent(t *testing.T) {
	key := decodeTestKey(t)
	const payload = "restored-bytes"
	sealed := sealTar(t,
		key,
		[]*tar.Header{{Name: "hub/ok.txt", Mode: 0o600}},
		[][]byte{[]byte(payload)},
	)

	dest := t.TempDir()
	if _, err := Extract(key, sealed, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "hub", "ok.txt"))
	if err != nil {
		t.Fatalf("test is vacuous: restore wrote no readable file: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("restored content corrupted: %q", got)
	}
	// A restrictive mode from the archive must still be honoured.
	st, _ := os.Stat(filepath.Join(dest, "hub", "ok.txt"))
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("an archive asking for 0600 should stay owner-only, got %04o", st.Mode().Perm())
	}
}

// io.ReadAll(tr) was unbounded, so a decompression bomb was read wholly into
// memory. The ceiling must reject an over-size member rather than truncate it.
func TestN18_OversizeArchiveMemberIsRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates >256MiB")
	}
	key := decodeTestKey(t)
	big := make([]byte, restoreMaxFileBytes+1)
	sealed := sealTar(t,
		key,
		[]*tar.Header{{Name: "hub/bomb.bin", Mode: 0o600}},
		[][]byte{big},
	)

	dest := t.TempDir()
	if _, err := Extract(key, sealed, dest); err == nil {
		t.Fatal("N18: an archive member larger than the restore limit was accepted — a decompression bomb can OOM the hub")
	}
}

// Governor-config key resolution (#4129). A hosted spoke owner has no
// deployment-env access, so ResolveKey must accept a key file named by
// governor config — while keeping the env fallback and the fail-closed
// refusal when neither supplies one.

func TestResolveKeyFromGovernorConfigFile(t *testing.T) {
	t.Setenv(EnvBackupKey, "")
	path := filepath.Join(t.TempDir(), "backup_encryption_key")
	if err := os.WriteFile(path, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	key, source, err := ResolveKeyWithSource(&config.BackupConfig{KeyFile: path})
	if err != nil {
		t.Fatalf("configured key must resolve without the env var: %v", err)
	}
	if len(key) != aesKeySize {
		t.Fatalf("key len = %d, want %d", len(key), aesKeySize)
	}
	if source != "file:"+path {
		t.Fatalf("source = %q, want file:%s", source, path)
	}
	// The safe source label is what gets logged and returned by APIs.
	if strings.Contains(source, testKey) {
		t.Fatal("source label leaked the key value")
	}
}

// Fail-closed with a config present but empty: the refusal must still be the
// explicit one, so an operator learns their backup would have been plaintext.
func TestResolveKeyRefusesWhenNeitherSourceSet(t *testing.T) {
	t.Setenv(EnvBackupKey, "")
	_, _, err := ResolveKeyWithSource(&config.BackupConfig{
		KeyFile: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if err == nil {
		t.Fatal("expected a refusal when no source supplies a key")
	}
	if !strings.Contains(err.Error(), "refusing to write an unencrypted backup") {
		t.Fatalf("want the explicit refusal, got %v", err)
	}
}

// A malformed configured key must fail closed too, and the error must not
// echo the (bad) key material back to a caller or a log.
func TestResolveKeyRejectsMalformedConfiguredKeyWithoutEcho(t *testing.T) {
	t.Setenv(EnvBackupKey, "")
	path := filepath.Join(t.TempDir(), "backup_encryption_key")
	const bad = "zz23456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveKeyWithSource(&config.BackupConfig{KeyFile: path})
	if err == nil {
		t.Fatal("a non-hex key must be rejected")
	}
	if strings.Contains(err.Error(), bad) {
		t.Fatalf("error echoed key material: %v", err)
	}
}

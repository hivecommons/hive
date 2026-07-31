package hubbackup

import (
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
			"hive.yaml.runtime":       []byte("project:\n  org: acme\n"),
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
			"hive-id":       []byte("hosted-x"),
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
	raw := []byte(`{"kind":"Secret","metadata":{"name":"x","uid":"u1',","resourceVersion":"9","creationTimestamp":"t"},"data":{"a":"b"}}`)
	// Use a well-formed input.
	raw = []byte(`{"kind":"Secret","metadata":{"name":"x","uid":"u1","resourceVersion":"9","creationTimestamp":"t"},"data":{"a":"b"}}`)
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

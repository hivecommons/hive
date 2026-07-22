package integrated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

func TestManagedPathPreimagesRestoreRepositoryOwnedVisualHiveFiles(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	if _, err := git(ctx, root, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "config", "user.name", "Hive Test"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "config", "user.email", "hive-test@example.invalid"); err != nil {
		t.Fatal(err)
	}
	original := map[string][]byte{
		"visual-hive.config.yaml":                     []byte("project:\n  name: repository-owned\n"),
		"docs/visual-hive.md":                         []byte("repository Visual Hive guide\n"),
		".github/workflows/visual-hive-pr.yml":        []byte("name: repository visual checks\n"),
		".github/workflows/visual-hive-lifecycle.yml": []byte("name: standalone lifecycle\npermissions:\n  issues: write\n"),
	}
	for relative, data := range original {
		if err := writeSetupCheckoutFile(root, relative, data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git(ctx, root, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "commit", "-m", "initial repository files"); err != nil {
		t.Fatal(err)
	}
	preimages, err := captureManagedPathPreimages(ctx, root, managedSetupFiles(true))
	if err != nil {
		t.Fatal(err)
	}
	config := Config{VisualHive: true, ManagedPreimagesVersion: managedPreimagesVersion, ManagedPathPreimages: preimages}
	for relative := range original {
		if err := writeSetupCheckoutFile(root, relative, []byte("Hive-managed replacement\n")); err != nil {
			t.Fatal(err)
		}
	}
	for _, relative := range standaloneVisualHiveWriterWorkflowPaths() {
		if err := removeSetupCheckoutFile(root, relative); err != nil {
			t.Fatal(err)
		}
	}
	if err := restoreManagedPathPreimages(root, config); err != nil {
		t.Fatal(err)
	}
	if err := stageManagedPaths(ctx, root, managedSetupFiles(true)); err != nil {
		t.Fatal(err)
	}
	if err := applyManagedPathPreimageModes(ctx, root, config); err != nil {
		t.Fatal(err)
	}
	for relative, want := range original {
		got, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("restored %s = %q, err=%v; want %q", relative, got, readErr, want)
		}
	}
	for _, relative := range managedSetupFiles(true) {
		if _, repositoryOwned := original[relative]; repositoryOwned {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); !os.IsNotExist(statErr) {
			t.Fatalf("Hive-owned path %s was not removed: %v", relative, statErr)
		}
	}
}

func TestManagedPathPreimagesRejectTamperedContent(t *testing.T) {
	data := []byte("repository config\n")
	digest := sha256.Sum256(data)
	preimages := make(map[string]ManagedPathPreimage, len(managedSetupFiles(true)))
	for _, relative := range managedSetupFiles(true) {
		preimages[relative] = ManagedPathPreimage{Existed: false}
	}
	preimages["visual-hive.config.yaml"] = ManagedPathPreimage{
		Existed: true, GitMode: "100644", Content: data, ContentSHA256: hex.EncodeToString(digest[:]),
	}
	if err := validateManagedPathPreimages(managedPreimagesVersion, preimages, managedSetupFiles(true)); err != nil {
		t.Fatalf("valid preimage ledger rejected: %v", err)
	}
	tampered := cloneManagedPathPreimages(preimages)
	record := tampered["visual-hive.config.yaml"]
	record.Content = []byte("changed\n")
	tampered["visual-hive.config.yaml"] = record
	if err := validateManagedPathPreimages(managedPreimagesVersion, tampered, managedSetupFiles(true)); err == nil {
		t.Fatal("tampered managed preimage ledger was accepted")
	}
	if _, err := managedPathPolicyDigest(Config{VisualHive: true, ManagedPreimagesVersion: managedPreimagesVersion, ManagedPathPreimages: tampered}); err == nil {
		t.Fatal("tampered managed preimage ledger degraded to legacy delete policy")
	}
}

func TestVerifyUninstalledSetupRequiresExactRestoredPreimages(t *testing.T) {
	original := []byte("project:\n  name: repository-owned\n")
	digest := sha256.Sum256(original)
	preimages := make(map[string]ManagedPathPreimage, len(managedSetupFiles(true)))
	for _, relative := range managedSetupFiles(true) {
		preimages[relative] = ManagedPathPreimage{Existed: false}
	}
	preimages["visual-hive.config.yaml"] = ManagedPathPreimage{Existed: true, GitMode: "100644", Content: original, ContentSHA256: hex.EncodeToString(digest[:])}
	config := Config{Repository: "owner/repo", VisualHive: true, ManagedPreimagesVersion: managedPreimagesVersion, ManagedPathPreimages: preimages}
	commit := strings.Repeat("a", 40)
	serve := func(content []byte) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Query().Get("ref") != commit {
				http.Error(writer, "wrong ref", http.StatusBadRequest)
				return
			}
			if request.URL.Path == "/repos/owner/repo/contents/visual-hive.config.yaml" {
				_, _ = io.WriteString(writer, `{"type":"file","encoding":"base64","content":"`+base64.StdEncoding.EncodeToString(content)+`"}`)
				return
			}
			http.Error(writer, "missing", http.StatusNotFound)
		}))
	}
	server := serve(original)
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := VerifyUninstalledSetupAtCommit(context.Background(), client, config, commit); err != nil {
		t.Fatalf("exact restored preimage was rejected: %v", err)
	}
	server.Close()

	server = serve([]byte("tampered\n"))
	defer server.Close()
	client = hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := VerifyUninstalledSetupAtCommit(context.Background(), client, config, commit); err == nil || !strings.Contains(err.Error(), "byte-for-byte") {
		t.Fatalf("tampered restored preimage was accepted: %v", err)
	}
}

func TestManagedPathPreimagesPersistLocallyButAreRedactedFromCommandJSON(t *testing.T) {
	data := []byte("private repository documentation\n")
	digest := sha256.Sum256(data)
	preimages := make(map[string]ManagedPathPreimage, len(managedSetupFiles(true)))
	for _, relative := range managedSetupFiles(true) {
		preimages[relative] = ManagedPathPreimage{Existed: false}
	}
	preimages["docs/visual-hive.md"] = ManagedPathPreimage{Existed: true, GitMode: "100644", Content: data, ContentSHA256: hex.EncodeToString(digest[:])}
	config := Config{Repository: "owner/repo", RepositoryID: "123", VisualHive: true, ManagedPreimagesVersion: managedPreimagesVersion, ManagedPathPreimages: preimages}
	public, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(public, data) || !bytes.Contains(public, []byte("managed_path_preimages")) {
		t.Fatalf("command JSON exposed managed preimages: %s", public)
	}
	var disclosed Config
	if err := json.Unmarshal(public, &disclosed); err != nil {
		t.Fatal(err)
	}
	if err := validateManagedPathOwnership(disclosed.ManagedPreimagesVersion, disclosed.ManagedPathPreimages, managedSetupFiles(true)); err != nil {
		t.Fatalf("command JSON lost managed path ownership metadata: %v", err)
	}
	if len(disclosed.ManagedPathPreimages["docs/visual-hive.md"].Content) != 0 || hasValidManagedPathPreimages(disclosed) {
		t.Fatal("command JSON exposed content bytes or retained destructive restoration authority")
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.ManagedPathPreimages["docs/visual-hive.md"].Content, data) || !hasValidManagedPathPreimages(loaded) {
		t.Fatal("durable state did not retain the exact validated managed preimage")
	}
}

func TestResolveManagedPathPreimagesLegacyVisualHiveRerunRetainsDeletionPolicy(t *testing.T) {
	root := trackedManagedPathFixture(t, map[string][]byte{
		".hive/integrated.json":                  []byte("legacy Hive policy\n"),
		".github/workflows/hive-visual-hive.yml": []byte("legacy Hive workflow\n"),
		"docs/hive-quickstart.md":                []byte("legacy Hive docs\n"),
		"visual-hive.config.yaml":                []byte("legacy Hive Visual config\n"),
	})
	version, preimages, updated, err := resolveManagedPathPreimages(context.Background(), root, Config{VisualHive: true}, true, true, ExecutionLocal)
	if err != nil {
		t.Fatal(err)
	}
	if version != "" || preimages != nil || updated {
		t.Fatalf("legacy Visual Hive rerun manufactured repository ownership: version=%q preimages=%+v updated=%t", version, preimages, updated)
	}
	config := Config{VisualHive: true, ManagedPreimagesVersion: version, ManagedPathPreimages: preimages}
	present, absent := managedUninstallRequiredPaths(config)
	if len(present) != 0 || len(absent) != len(managedSetupFiles(true)) {
		t.Fatalf("legacy Visual Hive rerun changed deletion semantics: present=%v absent=%v", present, absent)
	}
}

func TestResolveManagedPathPreimagesLegacyHiveCapturesOnlyNewVisualHivePaths(t *testing.T) {
	standaloneConfig := []byte("project:\n  name: standalone\n")
	standaloneWorkflow := []byte("name: standalone lifecycle\npermissions:\n  issues: write\n")
	root := trackedManagedPathFixture(t, map[string][]byte{
		".hive/integrated.json":                       []byte("legacy Hive policy\n"),
		".github/workflows/hive-visual-hive.yml":      []byte("legacy Hive workflow\n"),
		"docs/hive-quickstart.md":                     []byte("legacy Hive docs\n"),
		"visual-hive.config.yaml":                     standaloneConfig,
		".github/workflows/visual-hive-lifecycle.yml": standaloneWorkflow,
	})
	version, preimages, updated, err := resolveManagedPathPreimages(context.Background(), root, Config{VisualHive: false}, true, true, ExecutionLocal)
	if err != nil {
		t.Fatal(err)
	}
	if version != managedPreimagesVersion || !updated {
		t.Fatalf("legacy core-to-Visual migration did not create one bounded ledger: version=%q updated=%t", version, updated)
	}
	if err := validateManagedPathPreimages(version, preimages, managedSetupFiles(true)); err != nil {
		t.Fatalf("migration ledger is invalid: %v", err)
	}
	for _, relative := range managedSetupFiles(false) {
		if preimages[relative].Existed || len(preimages[relative].Content) != 0 {
			t.Fatalf("legacy Hive-owned core path %s was recaptured as repository-owned: %+v", relative, preimages[relative])
		}
	}
	for relative, want := range map[string][]byte{
		"visual-hive.config.yaml":                     standaloneConfig,
		".github/workflows/visual-hive-lifecycle.yml": standaloneWorkflow,
	} {
		if got := preimages[relative]; !got.Existed || !bytes.Equal(got.Content, want) {
			t.Fatalf("newly entering Visual Hive path %s was not preserved: %+v", relative, got)
		}
	}
}

func trackedManagedPathFixture(t *testing.T, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	ctx := context.Background()
	if _, err := git(ctx, root, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "config", "user.name", "Hive Test"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "config", "user.email", "hive-test@example.invalid"); err != nil {
		t.Fatal(err)
	}
	for relative, content := range files {
		if err := writeSetupCheckoutFile(root, relative, content); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git(ctx, root, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "commit", "-m", "tracked managed paths"); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestUninstallCleanupCommitRestoresPreExistingVisualHiveCapabilities(t *testing.T) {
	fixture := newUninstallFixture(t)
	defer fixture.server.Close()
	store, err := NewStore(filepath.Join(fixture.stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	preimages := make(map[string]ManagedPathPreimage, len(managedSetupFiles(true)))
	for _, relative := range managedSetupFiles(true) {
		preimages[relative] = ManagedPathPreimage{Existed: false}
	}
	original := map[string][]byte{
		"visual-hive.config.yaml":                     []byte("project:\n  name: pre-existing\n"),
		"docs/visual-hive.md":                         []byte("# Pre-existing Visual Hive guide\n"),
		".github/workflows/visual-hive-lifecycle.yml": []byte("name: pre-existing lifecycle\npermissions:\n  issues: write\n"),
	}
	for relative, content := range original {
		digest := sha256.Sum256(content)
		preimages[relative] = ManagedPathPreimage{Existed: true, GitMode: "100644", Content: content, ContentSHA256: hex.EncodeToString(digest[:])}
	}
	config.ManagedPreimagesVersion, config.ManagedPathPreimages = managedPreimagesVersion, preimages
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	required, absent := managedUninstallRequiredPaths(config)
	requiredJSON, _ := json.Marshal(required)
	absentJSON, _ := json.Marshal(absent)
	hostedAuthorization := setupAuthorizationWorkflowJob(config)
	if !strings.Contains(hostedAuthorization, "HIVE_UNINSTALL_REQUIRED_FILES: '"+string(requiredJSON)+"'") ||
		!strings.Contains(hostedAuthorization, "HIVE_UNINSTALL_REQUIRED_ABSENT: '"+string(absentJSON)+"'") {
		t.Fatal("hosted uninstall authorization did not bind restored and removed managed paths separately")
	}
	result, err := RunManagement(context.Background(), ManagementOptions{Operation: OperationUninstall, StateDir: fixture.stateDir, GitHub: fixture.client})
	if err != nil {
		t.Fatalf("prepare restoring uninstall: %v", err)
	}
	for relative, want := range original {
		got := integratedGitOutput(t, config.CheckoutDir, "cat-file", "blob", result.CommitSHA+":"+relative)
		if got != string(want) {
			t.Fatalf("cleanup commit restored %s as %q, want %q", relative, got, want)
		}
	}
	if _, err := integratedGitOutputError(config.CheckoutDir, "cat-file", "-e", result.CommitSHA+":.hive/integrated.json"); err == nil {
		t.Fatal("cleanup commit retained a Hive-owned managed file")
	}
}

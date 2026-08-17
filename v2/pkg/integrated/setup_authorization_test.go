package integrated

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

var setupAuthorizationRequiredFiles = []string{
	".github/workflows/hive-visual-hive.yml",
	".github/workflows/visual-hive-pr.yml",
	".hive/integrated.json",
	"docs/hive-quickstart.md",
	"docs/visual-hive.md",
	"visual-hive.config.yaml",
}

var setupAuthorizationRequiredAbsent = standaloneVisualHiveWriterWorkflowPaths()

func TestGeneratedUninstallPublisherCreatesAndRecoversOnlyExactAuthorizedCheck(t *testing.T) {
	bash := workflowBash(t)
	workflow := pullRequestWorkflow(Config{
		RepositoryID: "123", DefaultBranch: "main", SetupBranch: "hive/setup-123", SetupAuthorizationActorID: 456,
		VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40),
	})
	var document struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &document); err != nil {
		t.Fatal(err)
	}
	job, exists := document.Jobs["uninstall-required-check"]
	if !exists || len(job.Steps) != 1 || strings.TrimSpace(job.Steps[0].Run) == "" {
		t.Fatalf("generated uninstall publisher job is absent or ambiguous: %+v", job)
	}

	head, digest := strings.Repeat("b", 40), strings.Repeat("c", 64)
	details := "https://github.test/owner/repo/pull/17"
	external := "hive-uninstall-" + digest
	exact := map[string]any{
		"id": 91, "name": "visual-hive", "head_sha": head, "status": "completed", "conclusion": "success",
		// GitHub Actions owns this response field and rewrites a requested PR
		// URL to the created check-run URL. The authorization digest already
		// binds the exact PR URL before this publisher runs.
		"details_url": "https://github.test/owner/repo/runs/91", "external_id": external, "app": map[string]any{"slug": "github-actions"},
	}
	var mu sync.Mutex
	created, posts := false, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(writer, `{"message":"missing token"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/"+head+"/check-runs":
			runs := []any{}
			if created {
				runs = append(runs, exact)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"check_runs": runs})
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/check-runs":
			var input map[string]any
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
				http.Error(writer, `{"message":"bad json"}`, http.StatusBadRequest)
				return
			}
			if input["name"] != "visual-hive" || input["head_sha"] != head || input["external_id"] != external || input["details_url"] != details || input["status"] != "completed" || input["conclusion"] != "success" {
				t.Errorf("publisher input is not exact: %+v", input)
			}
			created, posts = true, posts+1
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(exact)
		default:
			http.Error(writer, request.Method+" "+request.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	run := func(authorized string) ([]byte, error) {
		command := exec.Command(bash, "-c", job.Steps[0].Run)
		command.Env = append(os.Environ(),
			"HIVE_CHECK_TOKEN=test-token", "HIVE_API_URL="+server.URL, "HIVE_REPOSITORY=owner/repo", "HIVE_HEAD_SHA="+head,
			"HIVE_PULL_REQUEST_URL="+details, "HIVE_SETUP_AUTHORIZED="+authorized, "HIVE_SETUP_BINDING_DIGEST="+digest,
		)
		return command.CombinedOutput()
	}
	if output, err := run("true"); err != nil {
		t.Fatalf("exact authorized publisher failed: %v\n%s", err, output)
	}
	if output, err := run("true"); err != nil {
		t.Fatalf("exact publisher recovery failed: %v\n%s", err, output)
	}
	mu.Lock()
	if posts != 1 {
		t.Fatalf("idempotent publisher created %d checks", posts)
	}
	mu.Unlock()
	if output, err := run("false"); err == nil || !strings.Contains(string(output), "authorization is absent") {
		t.Fatalf("publisher accepted missing authorization: err=%v output=%s", err, output)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("unauthorized publisher mutated checks: posts=%d", posts)
	}
}

func TestComputeSetupDiffEvidenceUsesBinaryNoRenamesObjectEncoding(t *testing.T) {
	repository := newSetupAuthorizationGitRepository(t)
	writeSetupAuthorizationFixture(t, repository, "old.txt", []byte("same contents\n"))
	writeSetupAuthorizationFixture(t, repository, "binary.bin", []byte{0, 1, 2, 3, 0xff})
	setupAuthorizationGit(t, repository, "add", "--", "old.txt", "binary.bin")
	setupAuthorizationGit(t, repository, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(setupAuthorizationGit(t, repository, "rev-parse", "HEAD"))

	if err := os.Rename(filepath.Join(repository, "old.txt"), filepath.Join(repository, "new.txt")); err != nil {
		t.Fatal(err)
	}
	writeSetupAuthorizationFixture(t, repository, "binary.bin", []byte{0, 1, 9, 3, 0xff, 0})
	setupAuthorizationGit(t, repository, "add", "-A", "--", "old.txt", "new.txt", "binary.bin")
	setupAuthorizationGit(t, repository, "commit", "-m", "head")
	headSHA := strings.TrimSpace(setupAuthorizationGit(t, repository, "rev-parse", "HEAD"))

	first, err := ComputeSetupDiffEvidence(context.Background(), repository, baseSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ComputeSetupDiffEvidence(context.Background(), repository, baseSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"binary.bin", "new.txt", "old.txt"}
	if first.Algorithm != SetupAuthorizationDiffAlgorithm || first.ChangeCount != 3 || !reflect.DeepEqual(first.ChangedPaths, wantPaths) {
		t.Fatalf("canonical no-renames evidence = %+v, want paths %v", first, wantPaths)
	}
	if first.SHA256 != second.SHA256 || !setupAuthorizationDigest.MatchString(first.SHA256) {
		t.Fatalf("canonical diff digest is unstable or malformed: first=%+v second=%+v", first, second)
	}
	// This is a cross-Git-version golden: the digest hashes raw diff-tree
	// metadata plus raw old/new blob bytes, not textual patch output.
	const expected = "e3ef8719ff1b9cafb2ba4f8776edbe088a81b82404a19c61c70d1b7ef64aceb0"
	if first.SHA256 != expected {
		t.Fatalf("canonical diff golden changed: got %s want %s", first.SHA256, expected)
	}
}

func TestBuildSetupAuthorizationBindingBindsExactPullAndRequiredContents(t *testing.T) {
	repository := newSetupAuthorizationGitRepository(t)
	writeSetupAuthorizationFixture(t, repository, "README.md", []byte("base\n"))
	for _, forbidden := range setupAuthorizationRequiredAbsent {
		writeSetupAuthorizationFixture(t, repository, forbidden, []byte("standalone writer\n"))
	}
	setupAuthorizationGit(t, repository, "add", "-A")
	setupAuthorizationGit(t, repository, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(setupAuthorizationGit(t, repository, "rev-parse", "HEAD"))

	for _, required := range setupAuthorizationRequiredFiles {
		writeSetupAuthorizationFixture(t, repository, required, []byte("managed "+required+"\n"))
	}
	for _, forbidden := range setupAuthorizationRequiredAbsent {
		if err := os.Remove(filepath.Join(repository, filepath.FromSlash(forbidden))); err != nil {
			t.Fatal(err)
		}
	}
	setupAuthorizationGit(t, repository, "add", "-A")
	setupAuthorizationGit(t, repository, "commit", "-m", "managed setup")
	headSHA := strings.TrimSpace(setupAuthorizationGit(t, repository, "rev-parse", "HEAD"))

	binding, err := BuildSetupAuthorizationBinding(context.Background(), SetupAuthorizationRequest{
		CheckoutDir: repository, Repository: "Owner/Repo", RepositoryID: "123", PullRequest: 7,
		HeadRef: "hive/setup-123", HeadSHA: headSHA, BaseRef: "main", BaseSHA: baseSHA, AuthorizerID: 456,
		RequiredPresent: append([]string(nil), setupAuthorizationRequiredFiles...), RequiredAbsent: append([]string(nil), setupAuthorizationRequiredAbsent...),
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding.Repository != "owner/repo" || binding.AuthorizerID != "456" || binding.PullRequest != 7 || len(binding.RequiredFiles) != len(setupAuthorizationRequiredFiles) {
		t.Fatalf("binding lost exact identity or files: %+v", binding)
	}
	for index, required := range setupAuthorizationRequiredFiles {
		if binding.RequiredFiles[index].Path != required || binding.RequiredFiles[index].Mode != "100644" || binding.RequiredFiles[index].Size <= 0 {
			t.Fatalf("required file evidence %d = %+v", index, binding.RequiredFiles[index])
		}
	}
	contextValue, err := binding.StatusContext()
	if err != nil || !regexpSetupAuthorizationContext(contextValue) {
		t.Fatalf("status context = %q, err %v", contextValue, err)
	}

	mutations := map[string]func(*SetupAuthorizationBinding){
		"repository": func(value *SetupAuthorizationBinding) { value.Repository = "owner/other" },
		"pull":       func(value *SetupAuthorizationBinding) { value.PullRequest++ },
		"base":       func(value *SetupAuthorizationBinding) { value.BaseSHA = strings.Repeat("b", 40) },
		"head":       func(value *SetupAuthorizationBinding) { value.HeadSHA = strings.Repeat("c", 40) },
		"authorizer": func(value *SetupAuthorizationBinding) { value.AuthorizerID = "457" },
		"content": func(value *SetupAuthorizationBinding) {
			value.RequiredFiles = append([]SetupAuthorizationFile(nil), value.RequiredFiles...)
			value.RequiredFiles[0].ContentSHA256 = strings.Repeat("d", 64)
		},
	}
	for name, mutate := range mutations {
		candidate := binding
		mutate(&candidate)
		got, candidateErr := candidate.StatusContext()
		if candidateErr != nil {
			t.Fatalf("valid %s mutation unexpectedly failed validation: %v", name, candidateErr)
		}
		if got == contextValue {
			t.Fatalf("%s mutation did not change authorization context", name)
		}
	}

	if err := os.Remove(filepath.Join(repository, filepath.FromSlash(setupAuthorizationRequiredFiles[0]))); err != nil {
		t.Fatal(err)
	}
	setupAuthorizationGit(t, repository, "add", "-A")
	setupAuthorizationGit(t, repository, "commit", "-m", "delete required workflow")
	deletedHead := strings.TrimSpace(setupAuthorizationGit(t, repository, "rev-parse", "HEAD"))
	_, err = BuildSetupAuthorizationBinding(context.Background(), SetupAuthorizationRequest{
		CheckoutDir: repository, Repository: "owner/repo", RepositoryID: "123", PullRequest: 7,
		HeadRef: "hive/setup-123", HeadSHA: deletedHead, BaseRef: "main", BaseSHA: baseSHA, AuthorizerID: 456,
		RequiredPresent: setupAuthorizationRequiredFiles, RequiredAbsent: setupAuthorizationRequiredAbsent,
	})
	if err == nil || !strings.Contains(err.Error(), "is absent") {
		t.Fatalf("deleted required workflow was accepted: %v", err)
	}
}

func TestSetupAuthorizationCanonicalJSONGolden(t *testing.T) {
	binding := SetupAuthorizationBinding{
		SchemaVersion: SetupAuthorizationSchema, Repository: "owner/repo", RepositoryID: "123", PullRequest: 7,
		HeadRef: "hive/setup-123", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40),
		AuthorizerID: "456", DiffAlgorithm: SetupAuthorizationDiffAlgorithm, DiffSHA256: strings.Repeat("c", 64),
		RequiredFiles: []SetupAuthorizationFile{{
			Path: ".hive/integrated.json", Mode: "100644", BlobOID: strings.Repeat("d", 40), Size: 12, ContentSHA256: strings.Repeat("e", 64),
		}},
		RequiredAbsent: []string{".github/workflows/visual-hive-issue-lifecycle.yml"},
	}
	data, err := binding.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	const expectedJSON = `{"schema_version":"hive.setup-pr-authorization.v1","repository":"owner/repo","repository_id":"123","pull_request":7,"head_ref":"hive/setup-123","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_ref":"main","base_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","authorizer_id":"456","diff_algorithm":"hive.setup-diff-tree-blobs.v1+sha256","diff_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","required_files":[{"path":".hive/integrated.json","mode":"100644","blob_oid":"dddddddddddddddddddddddddddddddddddddddd","size":12,"content_sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}],"required_absent":[".github/workflows/visual-hive-issue-lifecycle.yml"]}`
	if string(data) != expectedJSON {
		t.Fatalf("canonical JSON changed:\n got %s\nwant %s", data, expectedJSON)
	}
	digest, err := binding.Digest()
	if err != nil {
		t.Fatal(err)
	}
	const expectedDigest = "337ef60e6987f66830f57807648f830a05c00686f7fec8ad531e85dd0c25df1a"
	if digest != expectedDigest {
		t.Fatalf("canonical binding digest changed: got %s want %s", digest, expectedDigest)
	}
}

func TestGeneratedNodeSetupVerifierMatchesGoBindingAndFailsClosed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the cross-language setup authorization verifier")
	}
	repository := newSetupAuthorizationGitRepository(t)
	writeSetupAuthorizationFixture(t, repository, "old-name.bin", []byte{0, 1, 2, 0xff})
	for _, forbidden := range setupAuthorizationRequiredAbsent {
		writeSetupAuthorizationFixture(t, repository, forbidden, []byte("standalone writer\n"))
	}
	setupAuthorizationGit(t, repository, "add", "-A")
	setupAuthorizationGit(t, repository, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(setupAuthorizationGit(t, repository, "rev-parse", "HEAD"))

	if err := os.Rename(filepath.Join(repository, "old-name.bin"), filepath.Join(repository, "renamed.bin")); err != nil {
		t.Fatal(err)
	}
	writeSetupAuthorizationFixture(t, repository, ".visual-hive/snapshots/home/chromium.png", []byte{0x89, 'P', 'N', 'G', 0, 3, 9})
	for _, required := range setupAuthorizationRequiredFiles {
		writeSetupAuthorizationFixture(t, repository, required, []byte("managed "+required+"\n"))
	}
	for _, forbidden := range setupAuthorizationRequiredAbsent {
		if err := os.Remove(filepath.Join(repository, filepath.FromSlash(forbidden))); err != nil {
			t.Fatal(err)
		}
	}
	setupAuthorizationGit(t, repository, "add", "-A")
	setupAuthorizationGit(t, repository, "commit", "-m", "managed setup")
	headSHA := strings.TrimSpace(setupAuthorizationGit(t, repository, "rev-parse", "HEAD"))
	binding, diff, err := BuildSetupAuthorizationBindingWithDiff(context.Background(), SetupAuthorizationRequest{
		CheckoutDir: repository, Repository: "owner/repo", RepositoryID: "123", PullRequest: 7,
		HeadRef: "hive/setup-123", HeadSHA: headSHA, BaseRef: "main", BaseSHA: baseSHA, AuthorizerID: 456,
		RequiredPresent: setupAuthorizationRequiredFiles, RequiredAbsent: setupAuthorizationRequiredAbsent,
	})
	if err != nil {
		t.Fatal(err)
	}
	contextValue, err := binding.StatusContext()
	if err != nil {
		t.Fatal(err)
	}
	bindingDigest, _ := binding.Digest()

	statusState, statusTarget := "success", "https://github.test/owner/repo/pull/7"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/repos/owner/repo/commits/"+headSHA+"/statuses" {
			http.Error(writer, request.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(writer, `[{"id":900,"state":%q,"context":%q,"target_url":%q,"creator":{"id":456,"login":"setup-operator","type":"User"}}]`, statusState, contextValue, statusTarget)
	}))
	defer server.Close()
	requiredJSON, _ := json.Marshal(setupAuthorizationRequiredFiles)
	absentJSON, _ := json.Marshal(setupAuthorizationRequiredAbsent)
	allowedJSON, _ := json.Marshal(diff.ChangedPaths)
	baseEnvironment := map[string]string{
		"HIVE_STATUS_TOKEN": "test", "HIVE_API_URL": server.URL, "HIVE_REPOSITORY": "owner/repo", "HIVE_REPOSITORY_ID": "123",
		"HIVE_PULL_REQUEST": "7", "HIVE_PULL_REQUEST_URL": statusTarget, "HIVE_HEAD_REPOSITORY": "owner/repo", "HIVE_HEAD_REF": "hive/setup-123",
		"HIVE_HEAD_SHA": headSHA, "HIVE_BASE_REPOSITORY": "owner/repo", "HIVE_BASE_REF": "main", "HIVE_BASE_SHA": baseSHA,
		"HIVE_EXPECTED_HEAD_REF": "hive/setup-123", "HIVE_EXPECTED_BASE_REF": "main", "HIVE_AUTHORIZER_ID": "456",
		"HIVE_EXPECTED_UPGRADE_REF": "hive/upgrade-123", "HIVE_EXPECTED_ROLLBACK_REF": "hive/rollback-123", "HIVE_EXPECTED_AUTHORIZER_TRANSFER_REF": "hive/authorizer-transfer-123", "HIVE_EXPECTED_UNINSTALL_REF": "hive/uninstall-123",
		"HIVE_TRANSFER_AUTHORIZER_ID": "456",
		"HIVE_REQUIRED_FILES":         string(requiredJSON), "HIVE_REQUIRED_ABSENT": string(absentJSON), "HIVE_ALLOWED_FILES": string(allowedJSON),
		"HIVE_UNINSTALL_REQUIRED_FILES": "[]", "HIVE_UNINSTALL_REQUIRED_ABSENT": "[]", "HIVE_UNINSTALL_ALLOWED_FILES": "[]",
		"HIVE_SETUP_STATUS_POLL_ATTEMPTS": "1", "HIVE_SETUP_STATUS_POLL_DELAY_MS": "0",
	}
	proof := runGeneratedSetupAuthorizationVerifier(t, node, repository, baseEnvironment)
	if proof["authorized"] != "true" || proof["operation"] != "setup" || proof["context"] != contextValue || proof["binding_digest"] != bindingDigest || proof["diff_digest"] != diff.SHA256 {
		t.Fatalf("generated Node verifier drifted from Go binding: proof=%+v binding=%+v diff=%+v", proof, binding, diff)
	}

	statusState = "failure"
	if got := runGeneratedSetupAuthorizationVerifier(t, node, repository, baseEnvironment); got["authorized"] != "false" {
		t.Fatalf("failed exact status was accepted: %+v", got)
	}
	statusState = "success"
	statusTarget = "https://github.test/owner/repo/pull/99"
	if got := runGeneratedSetupAuthorizationVerifier(t, node, repository, baseEnvironment); got["authorized"] != "false" {
		t.Fatalf("wrong-target exact status was accepted: %+v", got)
	}
	statusTarget = baseEnvironment["HIVE_PULL_REQUEST_URL"]
	actorDrift := cloneStringMap(baseEnvironment)
	actorDrift["HIVE_AUTHORIZER_ID"] = "457"
	if got := runGeneratedSetupAuthorizationVerifier(t, node, repository, actorDrift); got["authorized"] != "false" {
		t.Fatalf("actor drift was accepted: %+v", got)
	}
	baseDrift := cloneStringMap(baseEnvironment)
	baseDrift["HIVE_BASE_SHA"] = headSHA
	if got := runGeneratedSetupAuthorizationVerifier(t, node, repository, baseDrift); got["authorized"] != "false" {
		t.Fatalf("base/head drift was accepted: %+v", got)
	}

	uninstallBaseSHA := headSHA
	uninstallPaths := managedSetupFiles(true)
	for _, filePath := range uninstallPaths {
		removeErr := os.Remove(filepath.Join(repository, filepath.FromSlash(filePath)))
		if removeErr != nil && !os.IsNotExist(removeErr) {
			t.Fatal(removeErr)
		}
	}
	if err := os.Remove(filepath.Join(repository, ".visual-hive", "snapshots", "home", "chromium.png")); err != nil {
		t.Fatal(err)
	}
	setupAuthorizationGit(t, repository, "add", "-A")
	setupAuthorizationGit(t, repository, "commit", "-m", "managed uninstall")
	headSHA = strings.TrimSpace(setupAuthorizationGit(t, repository, "rev-parse", "HEAD"))
	uninstallBinding, uninstallDiff, err := BuildSetupAuthorizationBindingWithDiff(context.Background(), SetupAuthorizationRequest{
		CheckoutDir: repository, Repository: "owner/repo", RepositoryID: "123", PullRequest: 7,
		HeadRef: "hive/uninstall-123", HeadSHA: headSHA, BaseRef: "main", BaseSHA: uninstallBaseSHA, AuthorizerID: 456,
		RequiredAbsent: uninstallPaths,
	})
	if err != nil {
		t.Fatal(err)
	}
	contextValue, err = uninstallBinding.StatusContext()
	if err != nil {
		t.Fatal(err)
	}
	bindingDigest, _ = uninstallBinding.Digest()
	uninstallAbsentJSON, _ := json.Marshal(uninstallPaths)
	uninstallAllowedJSON, _ := json.Marshal(uninstallPaths)
	uninstallEnvironment := cloneStringMap(baseEnvironment)
	uninstallEnvironment["HIVE_HEAD_REF"] = "hive/uninstall-123"
	uninstallEnvironment["HIVE_HEAD_SHA"] = headSHA
	uninstallEnvironment["HIVE_BASE_SHA"] = uninstallBaseSHA
	uninstallEnvironment["HIVE_UNINSTALL_REQUIRED_ABSENT"] = string(uninstallAbsentJSON)
	uninstallEnvironment["HIVE_UNINSTALL_ALLOWED_FILES"] = string(uninstallAllowedJSON)
	uninstallProof := runGeneratedSetupAuthorizationVerifier(t, node, repository, uninstallEnvironment)
	if uninstallProof["authorized"] != "true" || uninstallProof["operation"] != "uninstall" || uninstallProof["context"] != contextValue || uninstallProof["binding_digest"] != bindingDigest || uninstallProof["diff_digest"] != uninstallDiff.SHA256 {
		t.Fatalf("generated Node verifier drifted from Go uninstall binding: proof=%+v binding=%+v diff=%+v", uninstallProof, uninstallBinding, uninstallDiff)
	}
	refusedUninstall := cloneStringMap(uninstallEnvironment)
	refusedUninstall["HIVE_UNINSTALL_ALLOWED_FILES"] = "[]"
	if got := runGeneratedSetupAuthorizationVerifier(t, node, repository, refusedUninstall); got["authorized"] != "false" || got["operation"] != "uninstall" {
		t.Fatalf("uninstall with an unmanaged deletion was accepted or lost its fail-closed operation: %+v", got)
	}
}

func runGeneratedSetupAuthorizationVerifier(t *testing.T, node, repository string, environment map[string]string) map[string]string {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "setup-verifier.cjs")
	if err := os.WriteFile(scriptPath, []byte(setupAuthorizationWorkflowVerifierScript()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "github-output.txt")
	command := exec.Command(node, scriptPath)
	command.Dir = repository
	command.Env = append([]string(nil), os.Environ()...)
	command.Env = append(command.Env, "GITHUB_OUTPUT="+outputPath)
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated setup verifier failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			result[key] = value
		}
	}
	return result
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func TestSetupAuthorizationRejectsExecutableRequiredFileAndPresentForbiddenFile(t *testing.T) {
	repository := newSetupAuthorizationGitRepository(t)
	writeSetupAuthorizationFixture(t, repository, "README.md", []byte("base\n"))
	setupAuthorizationGit(t, repository, "add", "-A")
	setupAuthorizationGit(t, repository, "commit", "-m", "base")
	baseSHA := strings.TrimSpace(setupAuthorizationGit(t, repository, "rev-parse", "HEAD"))
	for _, required := range setupAuthorizationRequiredFiles {
		writeSetupAuthorizationFixture(t, repository, required, []byte("managed\n"))
	}
	writeSetupAuthorizationFixture(t, repository, setupAuthorizationRequiredAbsent[0], []byte("forbidden\n"))
	setupAuthorizationGit(t, repository, "add", "-A")
	setupAuthorizationGit(t, repository, "update-index", "--chmod=+x", "--", setupAuthorizationRequiredFiles[0])
	setupAuthorizationGit(t, repository, "commit", "-m", "invalid setup")
	headSHA := strings.TrimSpace(setupAuthorizationGit(t, repository, "rev-parse", "HEAD"))
	_, err := BuildSetupAuthorizationBinding(context.Background(), SetupAuthorizationRequest{
		CheckoutDir: repository, Repository: "owner/repo", RepositoryID: "123", PullRequest: 7,
		HeadRef: "hive/setup-123", HeadSHA: headSHA, BaseRef: "main", BaseSHA: baseSHA, AuthorizerID: 456,
		RequiredPresent: setupAuthorizationRequiredFiles, RequiredAbsent: setupAuthorizationRequiredAbsent,
	})
	if err == nil || (!strings.Contains(err.Error(), "mode 100644") && !strings.Contains(err.Error(), "requires")) {
		t.Fatalf("invalid required file modes/absence were accepted: %v", err)
	}
}

func newSetupAuthorizationGitRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	setupAuthorizationGit(t, repository, "init", "-b", "main")
	setupAuthorizationGit(t, repository, "config", "user.name", "Hive Setup Test")
	setupAuthorizationGit(t, repository, "config", "user.email", "hive-setup-test@example.com")
	setupAuthorizationGit(t, repository, "config", "core.autocrlf", "false")
	setupAuthorizationGit(t, repository, "config", "core.filemode", "true")
	return repository
}

func writeSetupAuthorizationFixture(t *testing.T, repository, relative string, content []byte) {
	t.Helper()
	target := filepath.Join(repository, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func setupAuthorizationGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repository
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func regexpSetupAuthorizationContext(value string) bool {
	return strings.HasPrefix(value, SetupAuthorizationContextPrefix) && len(value) == len(SetupAuthorizationContextPrefix)+64 && setupAuthorizationDigest.MatchString(strings.TrimPrefix(value, SetupAuthorizationContextPrefix))
}

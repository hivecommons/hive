package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kubestellar/hive/pkg/automation"
	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func TestVerifyVisualHiveWorkflowCommitsRequiresAuditedPullRequestProducer(t *testing.T) {
	productionRef := strings.Repeat("f", 40)
	for _, test := range []struct {
		name          string
		productionRef string
		available     map[string]bool
		wantRequests  []string
		wantError     string
	}{
		{
			name:          "distinct production and audited refs",
			productionRef: productionRef,
			available:     map[string]bool{productionRef: true, visualHivePullRequestProducerCommit: true},
			wantRequests:  []string{productionRef, visualHivePullRequestProducerCommit},
		},
		{
			name:          "audited ref missing",
			productionRef: productionRef,
			available:     map[string]bool{productionRef: true},
			wantRequests:  []string{productionRef, visualHivePullRequestProducerCommit},
			wantError:     "verify audited Visual Hive pull request producer",
		},
		{
			name:          "shared production and audited ref",
			productionRef: visualHivePullRequestProducerCommit,
			available:     map[string]bool{visualHivePullRequestProducerCommit: true},
			wantRequests:  []string{visualHivePullRequestProducerCommit},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requested []string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				const prefix = "/repos/owner/visual-hive/commits/"
				if request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, prefix) {
					http.NotFound(writer, request)
					return
				}
				ref := strings.TrimPrefix(request.URL.Path, prefix)
				requested = append(requested, ref)
				if !test.available[ref] {
					http.Error(writer, `{"message":"missing commit"}`, http.StatusNotFound)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(writer, `{"sha":%q}`, ref)
			}))
			defer server.Close()

			client := hivegithub.NewClientForTest(server.URL, "owner", []string{"visual-hive"}, slog.Default())
			err := VerifyVisualHiveWorkflowCommits(context.Background(), client, "owner/visual-hive", test.productionRef)
			if test.wantError == "" && err != nil {
				t.Fatalf("verify workflow commits: %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
			if !reflect.DeepEqual(requested, test.wantRequests) {
				t.Fatalf("requested commits = %v, want %v", requested, test.wantRequests)
			}
		})
	}
}

func TestInstalledVisualHiveBundleSetupApplyAndMergedRerunIsIdempotent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required to exercise the installed Visual Hive bundle")
	}
	const visualRef = "3e8e02bf676d702a658cf2426f78229126175578"
	root := t.TempDir()
	entrypoint := writeSetupAcceptanceVisualBundle(t, filepath.Join(root, "visual-hive"), visualRef)
	manifest, err := ValidateVisualHiveRelease(entrypoint, "")
	if err != nil {
		t.Fatalf("validate installed Visual Hive bundle: %v", err)
	}
	if manifest.GitCommit != visualRef {
		t.Fatalf("installed bundle pin = %s, want %s", manifest.GitCommit, visualRef)
	}

	remote := filepath.Join(root, "remote.git")
	bindTestRepositoryCloneURL(t, remote)
	seed := filepath.Join(root, "seed")
	runIntegratedGit(t, root, "init", "--bare", remote)
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("setup acceptance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "package.json"), []byte("{\"scripts\":{\"test\":\"node --test\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const repositoryGuide = "# Repository Visual Hive guide\n"
	const standaloneLifecycle = "name: Visual Hive lifecycle\npermissions:\n  issues: write\n"
	writeFixture(t, seed, "docs/visual-hive.md", repositoryGuide)
	writeFixture(t, seed, ".github/workflows/visual-hive-lifecycle.yml", standaloneLifecycle)
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	api := newSetupAcceptanceGitHub(t, remote, visualRef)
	defer api.server.Close()
	client := hivegithub.NewClientForTest(api.server.URL, "owner", []string{"repo"}, slog.Default())
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureStateOwnershipMarker(stateDir, Config{Repository: "owner/repo", RepositoryID: "123"}); err != nil {
		t.Fatalf("bind acceptance state ownership: %v", err)
	}
	// Production Git commands deliberately strip ambient GIT_CONFIG_* URL
	// rewrites. Seed the already state-owned checkout explicitly so this test
	// exercises that security boundary instead of depending on a process hook.
	checkout := filepath.Join(stateDir, "integrated", "checkout")
	if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, root, "clone", "--origin", "origin", remote, checkout)
	if err := writeManagedCheckoutOwner(checkout, "owner/repo"); err != nil {
		t.Fatalf("bind acceptance checkout ownership: %v", err)
	}
	options := SetupOptions{
		Repository: "owner/repo", Coverage: CoverageComprehensive, Automation: AutomationAdvisory,
		Provider: "test", ProviderCommand: os.Args[0], VisualHive: true, StateDir: stateDir, Apply: true,
		VisualHiveCommand: node, VisualHiveArgs: []string{entrypoint},
		VisualHiveRepo: "DavidDiaz0317/visual-hive", VisualHiveRef: manifest.GitCommit,
		MaxActiveIssues: 5, MaxRepairAttempts: 4, GitHub: client,
		Policy: automation.Policy{ACMMLevel: 2, Mode: automation.ModeAdvisory, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 4},
	}

	first, err := RunSetup(context.Background(), options)
	if err != nil {
		t.Fatalf("apply setup from installed bundle: %v", err)
	}
	if !first.Applied || first.Idempotent || first.Config == nil || first.PRNumber != 7 {
		t.Fatalf("first setup result = %+v", first)
	}
	if first.Config.SetupAuthorizationActorID != 456 || first.SetupAuthorizationCreatorID != 456 || first.SetupAuthorizationStatusID != 900 || !strings.HasPrefix(first.SetupAuthorizationContext, SetupAuthorizationContextPrefix) {
		t.Fatalf("first setup did not bind an exact out-of-band human authorization: %+v config=%+v", first, first.Config)
	}
	if first.Plan.VisualHiveRepository != options.VisualHiveRepo || first.Plan.VisualHiveRef != visualRef || first.Config.VisualHiveRef != visualRef {
		t.Fatalf("first setup did not preserve disclosed bundle identity: plan=%s@%s config=%s", first.Plan.VisualHiveRepository, first.Plan.VisualHiveRef, first.Config.VisualHiveRef)
	}
	if err := validateManagedPathOwnership(first.Config.ManagedPreimagesVersion, first.Config.ManagedPathPreimages, managedSetupFiles(true)); err != nil {
		t.Fatalf("setup result lost non-secret managed-path ownership metadata: %v", err)
	}
	if hasValidManagedPathPreimages(*first.Config) {
		t.Fatal("setup result exposed private managed-path preimage bytes")
	}
	stored, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	storedConfig, err := stored.Load()
	if err != nil || !hasValidManagedPathPreimages(storedConfig) {
		t.Fatalf("setup did not durably bind managed-path ownership before publication: %v", err)
	}
	for relative, want := range map[string]string{"docs/visual-hive.md": repositoryGuide, ".github/workflows/visual-hive-lifecycle.yml": standaloneLifecycle} {
		preimage := storedConfig.ManagedPathPreimages[relative]
		if !preimage.Existed || string(preimage.Content) != want {
			t.Fatalf("setup preimage for %s = %+v, want exact repository bytes", relative, preimage)
		}
	}
	setupRef := "refs/heads/" + first.Branch
	setupSHA := strings.TrimSpace(integratedGitOutput(t, remote, "rev-parse", setupRef))
	if setupSHA != first.CommitSHA {
		t.Fatalf("pushed setup SHA = %s, result = %s", setupSHA, first.CommitSHA)
	}

	// Simulate merging the exact reviewed setup PR. The next setup invocation
	// must recognize the installed bytes and make no new commit, push, or PR API call.
	runIntegratedGit(t, remote, "update-ref", "refs/heads/main", setupSHA)
	if err := VerifyInstalledSetup(context.Background(), client, *first.Config); err != nil {
		t.Fatalf("merged installed setup was not accepted: %v", err)
	}
	api.mu.Lock()
	createCalls, listCalls, statusCalls := api.createCalls, api.listCalls, api.statusCalls
	api.userID = 789
	api.mu.Unlock()

	second, err := RunSetup(context.Background(), options)
	if err != nil {
		t.Fatalf("idempotent setup rerun: %v", err)
	}
	if !second.Applied || !second.Idempotent || second.Config == nil || second.CommitSHA != setupSHA {
		t.Fatalf("second setup result = %+v", second)
	}
	if second.Config.SetupAuthorizationActorID != 456 {
		t.Fatalf("cross-user identical rerun rotated the installed authorizer instead of remaining a no-op: %+v", second.Config)
	}
	if second.Plan.VisualHiveRepository != first.Plan.VisualHiveRepository || second.Plan.VisualHiveRef != first.Plan.VisualHiveRef {
		t.Fatalf("idempotent plan dependency drifted: first=%s@%s second=%s@%s", first.Plan.VisualHiveRepository, first.Plan.VisualHiveRef, second.Plan.VisualHiveRepository, second.Plan.VisualHiveRef)
	}
	if got := strings.TrimSpace(integratedGitOutput(t, remote, "rev-parse", "refs/heads/main")); got != setupSHA {
		t.Fatalf("idempotent setup changed main from %s to %s", setupSHA, got)
	}
	if got := strings.TrimSpace(integratedGitOutput(t, remote, "rev-parse", setupRef)); got != setupSHA {
		t.Fatalf("idempotent setup changed setup branch from %s to %s", setupSHA, got)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.createCalls != createCalls || api.listCalls != listCalls || api.statusCalls != statusCalls {
		t.Fatalf("idempotent installed setup touched PR/status APIs: creates %d->%d lists %d->%d statuses %d->%d", createCalls, api.createCalls, listCalls, api.listCalls, statusCalls, api.statusCalls)
	}
}

func writeSetupAcceptanceVisualBundle(t *testing.T, root, commit string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(root, "visual-hive.mjs")
	program := `import fs from "node:fs";
import path from "node:path";
const command = process.argv[2] || "";
const args = process.argv.slice(3);
const value = (name) => { const index = args.indexOf(name); return index >= 0 ? args[index + 1] : ""; };
if (command === "recommend") {
  const repo = value("--repo") || process.cwd();
  fs.mkdirSync(path.join(repo, ".visual-hive"), { recursive: true });
  fs.writeFileSync(path.join(repo, ".visual-hive", "recommendations.json"), JSON.stringify({ recommendedConfig: { targets: [] } }) + "\n");
  if (args.includes("--write-config")) fs.writeFileSync(path.join(repo, "visual-hive.config.yaml"), "project:\n  name: setup-acceptance\n  setupProfile: complex-app\ntargets: []\n");
  if (args.includes("--write-docs")) {
    fs.mkdirSync(path.join(repo, "docs"), { recursive: true });
    fs.writeFileSync(path.join(repo, "docs", "visual-hive.md"), "# Visual Hive\n\nInstalled acceptance fixture.\n");
  }
}
`
	contents := []byte(program)
	if err := os.WriteFile(entrypoint, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := VisualHiveReleaseManifest{
		SchemaVersion: VisualHiveReleaseSchema, Name: "visual-hive", Version: "0.3.0", GitCommit: commit,
		Node: ">=22", Entrypoint: "visual-hive.mjs", PlaywrightVersion: "1.60.0",
		Files: []VisualHiveReleaseFile{{Path: "visual-hive.mjs", SHA256: fmt.Sprintf("%x", sha256.Sum256(contents)), Size: int64(len(contents))}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return entrypoint
}

type setupAcceptanceGitHub struct {
	server        *httptest.Server
	remote        string
	visualRef     string
	mu            sync.Mutex
	createCalls   int
	listCalls     int
	pullTitle     string
	pullBody      string
	pullHead      string
	pullBase      string
	statusContext string
	statusTarget  string
	statusHead    string
	statusCalls   int
	userID        int64
}

func newSetupAcceptanceGitHub(t *testing.T, remote, visualRef string) *setupAcceptanceGitHub {
	t.Helper()
	api := &setupAcceptanceGitHub{remote: remote, visualRef: visualRef, userID: 456}
	api.server = httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	return api
}

func (api *setupAcceptanceGitHub) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/user":
		api.mu.Lock()
		userID := api.userID
		api.mu.Unlock()
		_, _ = fmt.Fprintf(writer, `{"id":%d,"login":"setup-operator","type":"User"}`, userID)
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
		_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo","default_branch":"main","permissions":{"admin":true,"push":true,"pull":true}}`)
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/languages":
		_, _ = io.WriteString(writer, `{"JavaScript":100}`)
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
		http.Error(writer, `{"message":"not protected"}`, http.StatusNotFound)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/DavidDiaz0317/visual-hive/commits/"):
		ref := strings.TrimPrefix(request.URL.Path, "/repos/DavidDiaz0317/visual-hive/commits/")
		if ref != api.visualRef && ref != visualHivePullRequestProducerCommit {
			http.Error(writer, `{"message":"missing commit"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(writer, `{"sha":"`+ref+`"}`)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/owner/repo/git/ref/heads/"):
		branch := strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/git/ref/heads/")
		sha, err := setupAcceptanceGitOutput(api.remote, "rev-parse", "refs/heads/"+branch)
		if err != nil {
			http.Error(writer, `{"message":"missing ref"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"type": "commit", "sha": strings.TrimSpace(sha)}})
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
		api.mu.Lock()
		api.listCalls++
		created := api.createCalls > 0
		api.mu.Unlock()
		if !created {
			_, _ = io.WriteString(writer, `[]`)
			return
		}
		_ = json.NewEncoder(writer).Encode([]any{api.pullResponse()})
	case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
		var input struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  string `json:"head"`
			Base  string `json:"base"`
		}
		_ = json.NewDecoder(request.Body).Decode(&input)
		api.mu.Lock()
		api.createCalls++
		api.pullTitle, api.pullBody, api.pullHead, api.pullBase = input.Title, input.Body, input.Head, input.Base
		api.mu.Unlock()
		_, _ = io.WriteString(writer, `{"number":7}`)
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
		_ = json.NewEncoder(writer).Encode(api.pullResponse())
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/owner/repo/commits/") && strings.HasSuffix(request.URL.Path, "/statuses"):
		head := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/commits/"), "/statuses")
		api.mu.Lock()
		defer api.mu.Unlock()
		if api.statusContext == "" || api.statusHead != head {
			_, _ = io.WriteString(writer, `[]`)
			return
		}
		_ = json.NewEncoder(writer).Encode([]any{map[string]any{"id": 900, "state": "success", "context": api.statusContext, "target_url": api.statusTarget, "creator": map[string]any{"id": 456, "login": "setup-operator", "type": "User"}}})
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/repos/owner/repo/statuses/"):
		head := strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/statuses/")
		var input struct {
			State     string `json:"state"`
			Context   string `json:"context"`
			TargetURL string `json:"target_url"`
		}
		_ = json.NewDecoder(request.Body).Decode(&input)
		api.mu.Lock()
		api.statusContext, api.statusTarget, api.statusHead = input.Context, input.TargetURL, head
		api.statusCalls++
		api.mu.Unlock()
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": 900, "state": input.State, "context": input.Context, "target_url": input.TargetURL, "creator": map[string]any{"id": 456, "login": "setup-operator", "type": "User"}})
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/owner/repo/contents/"):
		path := strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/contents/")
		ref := request.URL.Query().Get("ref")
		if ref == "" {
			ref = "main"
		}
		contents, err := setupAcceptanceGitOutput(api.remote, "show", ref+":"+path)
		if err != nil {
			http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(contents))})
	default:
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}
}

func (api *setupAcceptanceGitHub) pullResponse() map[string]any {
	api.mu.Lock()
	defer api.mu.Unlock()
	sha, _ := setupAcceptanceGitOutput(api.remote, "rev-parse", "refs/heads/"+api.pullHead)
	baseSHA, _ := setupAcceptanceGitOutput(api.remote, "rev-parse", "refs/heads/"+api.pullBase)
	repository := map[string]any{"id": 123, "full_name": "owner/repo"}
	return map[string]any{
		"number": 7, "html_url": "https://example.test/owner/repo/pull/7", "state": "open",
		"title": api.pullTitle, "body": api.pullBody,
		"head": map[string]any{"ref": api.pullHead, "sha": strings.TrimSpace(sha), "repo": repository},
		"base": map[string]any{"ref": api.pullBase, "sha": strings.TrimSpace(baseSHA), "repo": repository},
	}
}

func setupAcceptanceGitOutput(remote string, args ...string) (string, error) {
	commandArgs := append([]string{"--git-dir", remote}, args...)
	output, err := exec.Command("git", commandArgs...).CombinedOutput()
	return string(output), err
}

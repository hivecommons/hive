package integrated

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func TestPlanOrphanedSetupResetBindsExactMergedSetup(t *testing.T) {
	fixture := newOrphanSetupFixture(t)
	defer fixture.server.Close()
	plan, err := PlanOrphanedSetupReset(context.Background(), OrphanSetupResetOptions{
		Repository: "owner/repo", StateDir: t.TempDir(), GitHub: fixture.client,
	})
	if err != nil {
		t.Fatalf("plan orphaned setup reset: %v", err)
	}
	if !plan.RecoveryAvailable || plan.RepositoryID != "123" || plan.SetupPRNumber != 7 || plan.CurrentHeadSHA != fixture.mergeSHA ||
		plan.SetupHeadSHA != fixture.setupHead || plan.SetupBaseSHA != fixture.setupBase || len(plan.ManagedPaths) != len(managedSetupFiles(true)) || len(plan.PlanSHA256) != 64 {
		t.Fatalf("unexpected orphaned setup plan: %+v", plan)
	}
	for _, binding := range plan.ManagedPaths {
		if binding.Path == ".hive/integrated.json" && (binding.CurrentContentSHA256 == "" || binding.SetupContentSHA256 != binding.CurrentContentSHA256 || binding.PreimageExists) {
			t.Fatalf("repository contract binding is incomplete: %+v", binding)
		}
	}
}

func TestPlanOrphanedSetupResetRejectsManagedDriftAndPostSetupLifecycle(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*orphanSetupFixture)
		want   string
	}{
		{name: "managed drift", mutate: func(f *orphanSetupFixture) { f.drift = true }, want: "no longer matches"},
		{name: "post setup issue", mutate: func(f *orphanSetupFixture) { f.postSetupIssue = true }, want: "post-setup lifecycle issue"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOrphanSetupFixture(t)
			defer fixture.server.Close()
			test.mutate(fixture)
			_, err := PlanOrphanedSetupReset(context.Background(), OrphanSetupResetOptions{
				Repository: "owner/repo", StateDir: t.TempDir(), GitHub: fixture.client,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestOrphanSetupResetPlanDigestBindsPreimages(t *testing.T) {
	plan := OrphanSetupResetPlan{
		SchemaVersion: orphanSetupResetPlanSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		CurrentHeadSHA: strings.Repeat("a", 40), SetupPRNumber: 7, SetupBaseSHA: strings.Repeat("b", 40),
		SetupHeadSHA: strings.Repeat("c", 40), SetupMergeSHA: strings.Repeat("a", 40), SetupAuthorizerID: 456,
		ManagedPaths: []OrphanSetupPathBinding{{Path: ".hive/integrated.json", PreimageExists: false, CurrentContentSHA256: strings.Repeat("d", 64), SetupContentSHA256: strings.Repeat("d", 64)}},
	}
	first, err := orphanSetupResetPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.ManagedPaths[0].PreimageExists = true
	plan.ManagedPaths[0].PreimageContentSHA256 = strings.Repeat("e", 64)
	second, err := orphanSetupResetPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("changing the exact preimage did not change the reset plan digest")
	}
}

func TestOrphanSetupResetCheckoutUsesManagedIntegratedLeaf(t *testing.T) {
	stateDir := t.TempDir()
	checkout := orphanSetupResetCheckoutDir(stateDir)
	want := path.Join(stateDir, "integrated", "checkout")
	if filepath.Clean(checkout) != filepath.Clean(want) {
		t.Fatalf("checkout = %q, want %q", checkout, want)
	}
	exists, err := validateManagedCheckoutBeforeGit(checkout, "owner/repo")
	if err != nil || exists {
		t.Fatalf("managed checkout validation = %t, %v", exists, err)
	}
	preparationRoot := t.TempDir()
	preparationCheckout := orphanSetupResetCheckoutDir(preparationRoot)
	exists, err = validateManagedCheckoutBeforeGit(preparationCheckout, "owner/repo")
	if err != nil || exists {
		t.Fatalf("managed preparation checkout validation = %t, %v", exists, err)
	}
}

type orphanSetupFixture struct {
	t              *testing.T
	server         *httptest.Server
	client         *hivegithub.Client
	setupBase      string
	setupHead      string
	mergeSHA       string
	mergedAt       time.Time
	files          map[string][]byte
	drift          bool
	postSetupIssue bool
}

func newOrphanSetupFixture(t *testing.T) *orphanSetupFixture {
	t.Helper()
	fixture := &orphanSetupFixture{
		t: t, setupBase: strings.Repeat("b", 40), setupHead: strings.Repeat("c", 40), mergeSHA: strings.Repeat("a", 40),
		mergedAt: time.Date(2026, 8, 3, 21, 12, 49, 0, time.UTC), files: map[string][]byte{},
	}
	installed := installedRepositoryConfig{
		SchemaVersion: managedRepositoryConfigSchema, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		Coverage: CoverageComprehensive, Automation: AutomationRepairPR, Provider: "codex", ExecutionMode: ExecutionLocal,
		RunIntervalSeconds: 900, ACMMLevel: 5, MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
		VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("d", 40), SetupAuthorizationActorID: 456,
	}
	contract, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	fixture.files[".hive/integrated.json"] = contract
	fixture.files[".github/workflows/hive-visual-hive.yml"] = []byte("name: production\n")
	fixture.files["docs/hive-quickstart.md"] = []byte("# Hive\n")
	fixture.files["docs/visual-hive.md"] = []byte("# Visual Hive\n")
	fixture.files["visual-hive.config.yaml"] = []byte("version: 1\n")
	fixture.files[".github/workflows/visual-hive-pr.yml"] = []byte("name: pull request\n")
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serve))
	fixture.client = hivegithub.NewClientForTest(fixture.server.URL, "owner", []string{"repo"}, slog.Default())
	return fixture
}

func (fixture *orphanSetupFixture) serve(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	switch {
	case request.URL.Path == "/repos/owner/repo":
		writeOrphanJSON(fixture.t, writer, map[string]any{"id": 123, "full_name": "owner/repo", "default_branch": "main"})
	case request.URL.Path == "/repos/owner/repo/branches/main":
		writeOrphanJSON(fixture.t, writer, map[string]any{"name": "main", "commit": map[string]any{"sha": fixture.mergeSHA}})
	case strings.HasPrefix(request.URL.Path, "/repos/owner/repo/contents/"):
		fixture.serveContent(writer, request)
	case request.URL.Path == "/repos/owner/repo/pulls" && request.URL.Query().Get("state") == "closed":
		writeOrphanJSON(fixture.t, writer, []any{map[string]any{"number": 7}})
	case request.URL.Path == "/repos/owner/repo/pulls/7":
		writeOrphanJSON(fixture.t, writer, fixture.pull())
	case request.URL.Path == "/repos/owner/repo/pulls/7/files":
		changed := []any{}
		for _, relative := range []string{".hive/integrated.json", ".github/workflows/hive-visual-hive.yml", "docs/hive-quickstart.md", "docs/visual-hive.md", "visual-hive.config.yaml", ".github/workflows/visual-hive-pr.yml"} {
			changed = append(changed, map[string]any{"filename": relative})
		}
		writeOrphanJSON(fixture.t, writer, changed)
	case request.URL.Path == "/repos/owner/repo/pulls":
		writeOrphanJSON(fixture.t, writer, []any{})
	case request.URL.Path == "/repos/owner/repo/issues":
		if fixture.postSetupIssue {
			writeOrphanJSON(fixture.t, writer, []any{map[string]any{
				"number": 9, "title": "post setup", "created_at": fixture.mergedAt.Add(time.Minute).Format(time.RFC3339),
				"labels": []any{map[string]any{"name": "hive/managed"}},
			}})
		} else {
			writeOrphanJSON(fixture.t, writer, []any{})
		}
	default:
		http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
	}
}

func (fixture *orphanSetupFixture) serveContent(writer http.ResponseWriter, request *http.Request) {
	relative, err := url.PathUnescape(strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/contents/"))
	if err != nil {
		fixture.t.Fatal(err)
	}
	if relative == ".visual-hive/snapshots" {
		http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	ref := request.URL.Query().Get("ref")
	if ref == fixture.setupBase {
		http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	content, exists := fixture.files[relative]
	if !exists {
		http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	if fixture.drift && ref == fixture.mergeSHA && relative == "visual-hive.config.yaml" {
		content = []byte("version: drift\n")
	}
	writeOrphanJSON(fixture.t, writer, map[string]any{
		"type": "file", "name": path.Base(relative), "path": relative, "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString(content), "sha": strings.Repeat("f", 40), "size": len(content),
	})
}

func (fixture *orphanSetupFixture) pull() map[string]any {
	return map[string]any{
		"number": 7, "html_url": "https://example.test/owner/repo/pull/7", "state": "closed", "merged": true,
		"merged_at": fixture.mergedAt.Format(time.RFC3339), "merge_commit_sha": fixture.mergeSHA,
		"head": map[string]any{"ref": "hive/setup-123", "sha": fixture.setupHead, "repo": map[string]any{"id": 123, "full_name": "owner/repo"}},
		"base": map[string]any{"ref": "main", "sha": fixture.setupBase, "repo": map[string]any{"id": 123, "full_name": "owner/repo"}},
	}
}

func writeOrphanJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func TestConfigFromInstalledRepositoryPreservesRuntimePolicy(t *testing.T) {
	installed := installedRepositoryConfig{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", Coverage: CoverageComprehensive,
		Automation: AutomationRepairPR, Provider: "codex", ExecutionMode: ExecutionLocal, RunIntervalSeconds: 900,
		ACMMLevel: 5, MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
		VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("d", 40), SetupAuthorizationActorID: 456,
		AllowedRepairPaths: []string{"src/**"},
	}
	config := configFromInstalledRepository(installed)
	if config.Repository != installed.Repository || config.Automation != AutomationRepairPR || config.VisualHiveRef != installed.VisualHiveRef ||
		config.SetupAuthorizationActorID != 456 || fmt.Sprint(config.MaxActiveIssues) != "5" {
		t.Fatalf("reconstructed runtime policy drifted: %+v", config)
	}
}

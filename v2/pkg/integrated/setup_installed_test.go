package integrated

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

func TestVerifyInstalledSetupMatchesExactManagedPolicyNotHistoricalPR(t *testing.T) {
	config := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		Coverage: CoverageComprehensive, Automation: AutomationAutoMerge, Provider: "codex", ACMMLevel: 6,
		MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
		VisualHiveRepo: "owner/visual", VisualHiveRef: strings.Repeat("a", 40),
		TestCommands: [][]string{{"npm", "test"}}, AllowedRepairPaths: []string{"tests/**"},
		AllowedAutoMergePaths: []string{"tests/**"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskAutomatic},
	}
	server := installedSetupTestServer(t, config)
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := VerifyInstalledSetup(context.Background(), client, config); err != nil {
		t.Fatalf("exact target setup was rejected: %v", err)
	}
	config.Coverage = CoverageStandard
	if err := VerifyInstalledSetup(context.Background(), client, config); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("target/local policy drift was accepted: %v", err)
	}
}

func TestVerifyInstalledSetupRejectsTamperedPinnedWorkflow(t *testing.T) {
	config := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		Coverage: CoverageComprehensive, Automation: AutomationAutoMerge, Provider: "codex", ACMMLevel: 6,
		MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
		VisualHiveRepo: "owner/visual", VisualHiveRef: strings.Repeat("a", 40),
	}
	server := installedSetupTestServerWithWorkflow(t, config, workflow(config)+"# changed ref or command\n")
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := VerifyInstalledSetup(context.Background(), client, config); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered production workflow was accepted: %v", err)
	}
}

func TestVerifyInstalledSetupAtCommitBindsEveryManagedRead(t *testing.T) {
	commitSHA := strings.Repeat("c", 40)
	config := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		Coverage: CoverageComprehensive, Automation: AutomationIssues, Provider: "codex", ACMMLevel: 4,
		MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
		VisualHiveRepo: "owner/visual", VisualHiveRef: strings.Repeat("a", 40),
	}
	server := installedSetupTestServerWithWorkflowAndRef(t, config, workflow(config), commitSHA)
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if err := VerifyInstalledSetupAtCommit(context.Background(), client, config, commitSHA); err != nil {
		t.Fatalf("exact commit setup was rejected: %v", err)
	}
	if err := VerifyInstalledSetupAtCommit(context.Background(), client, config, "main"); err == nil || !strings.Contains(err.Error(), "40-character") {
		t.Fatalf("mutable setup ref was accepted: %v", err)
	}
}

func TestRequireLiveInstalledWorkflowHeadRejectsPostDispatchPolicyChange(t *testing.T) {
	commitSHA := strings.Repeat("d", 40)
	config := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		Coverage: CoverageComprehensive, Automation: AutomationIssues, Provider: "codex", ACMMLevel: 4,
		MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
		VisualHiveRepo: "owner/visual", VisualHiveRef: strings.Repeat("a", 40),
	}
	server := installedSetupTestServerWithWorkflowAndRef(t, config, workflow(config)+"# out-of-band change\n", commitSHA)
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if _, err := requireLiveInstalledWorkflowHead(context.Background(), client, config, commitSHA); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("post-dispatch managed workflow change was accepted for lifecycle application: %v", err)
	}
}

func installedSetupTestServer(t *testing.T, config Config) *httptest.Server {
	return installedSetupTestServerWithWorkflow(t, config, workflow(config))
}

func installedSetupTestServerWithWorkflow(t *testing.T, config Config, productionWorkflow string) *httptest.Server {
	return installedSetupTestServerWithWorkflowAndRef(t, config, productionWorkflow, "")
}

func installedSetupTestServerWithWorkflowAndRef(t *testing.T, config Config, productionWorkflow, expectedRef string) *httptest.Server {
	t.Helper()
	installed := installedRepositoryConfig{
		SchemaVersion: managedRepositoryConfigSchema, Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: config.DefaultBranch,
		Coverage: config.Coverage, Automation: config.Automation, Provider: config.Provider, ACMMLevel: config.ACMMLevel,
		ExecutionMode: config.ExecutionMode, RunIntervalSeconds: config.RunIntervalSeconds, HostedSchedule: config.HostedSchedule,
		HostedStateBranch: config.HostedStateBranch, HiveReleaseRepository: config.HiveReleaseRepository,
		HiveReleaseVersion: config.HiveReleaseVersion, HiveCommit: config.HiveCommit,
		DistributionManifestSHA256: config.DistributionManifestSHA256, PreviousHostedRelease: cloneHostedReleaseIdentity(config.PreviousHostedRelease),
		MaxActiveIssues: config.MaxActiveIssues, MaxRepairAttempts: config.MaxRepairAttempts, VisualHive: config.VisualHive,
		VisualHiveRepo: config.VisualHiveRepo, VisualHiveRef: config.VisualHiveRef, VisualHiveConfigDigest: config.VisualHiveConfigDigest, TestCommands: config.TestCommands,
		AllowedRepairPaths: config.AllowedRepairPaths, AllowedAutoMergePaths: config.AllowedAutoMergePaths, AllowedAutoMergeRisk: config.AllowedAutoMergeRisk,
		SetupAuthorizationActorID: config.SetupAuthorizationActorID, SetupAuthorizationPreviousActorID: config.SetupAuthorizationPreviousActorID,
	}
	data, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if expectedRef != "" && strings.Contains(request.URL.Path, "/contents/") && request.URL.Query().Get("ref") != expectedRef {
			http.Error(writer, "managed read was not bound to exact commit", http.StatusBadRequest)
			return
		}
		visualCommitPrefix := "/repos/" + config.VisualHiveRepo + "/commits/"
		if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, visualCommitPrefix) {
			ref := strings.TrimPrefix(request.URL.Path, visualCommitPrefix)
			if ref == config.VisualHiveRef || ref == visualHivePullRequestProducerCommit {
				_, _ = io.WriteString(writer, `{"sha":"`+ref+`"}`)
				return
			}
		}
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/branches/main":
			head := expectedRef
			if head == "" {
				head = strings.Repeat("b", 40)
			}
			_, _ = io.WriteString(writer, `{"name":"main","commit":{"sha":"`+head+`"}}`)
		case "/repos/owner/repo/contents/.hive/integrated.json":
			_, _ = io.WriteString(writer, `{"type":"file","encoding":"base64","content":"`+base64.StdEncoding.EncodeToString(data)+`"}`)
		case "/repos/owner/repo/contents/.github/workflows/hive-visual-hive.yml":
			_, _ = io.WriteString(writer, `{"type":"file","encoding":"base64","content":"`+base64.StdEncoding.EncodeToString([]byte(productionWorkflow))+`"}`)
		case "/repos/owner/repo/contents/.github/workflows/visual-hive-pr.yml":
			_, _ = io.WriteString(writer, `{"type":"file","encoding":"base64","content":"`+base64.StdEncoding.EncodeToString([]byte(pullRequestWorkflow(config)))+`"}`)
		case "/repos/owner/repo/contents/visual-hive.config.yaml":
			_, _ = io.WriteString(writer, `{"type":"file","encoding":"base64","content":"`+base64.StdEncoding.EncodeToString([]byte("project:\n  setupProfile: complex-app\nintegrations:\n  hive:\n    enabled: true\n"))+`"}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
}

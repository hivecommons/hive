package integrated

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestRefreshOwnedRepairBranchPrunesStaleStateAndKeepsAttempt(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	remote, worktree, oldHead, baseSHA, githubMerge := setupIntegratedRefreshRepository(t)
	branch := "hive/repair-finding-a10"
	remoteHead := func() string {
		value, err := os.ReadFile(filepath.Join(remote, "refs", "heads", filepath.FromSlash(branch)))
		if err != nil {
			t.Fatal(err)
		}
		head := strings.TrimSpace(string(value))
		if len(head) != 40 {
			t.Fatalf("unexpected remote head: %q", head)
		}
		return head
	}
	mutationCalls := 0
	server := newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		head := remoteHead()
		mergeableState := "clean"
		if head == oldHead {
			mergeableState = "behind"
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/apps/github-actions":
			_, _ = io.WriteString(writer, `{"id":42,"slug":"github-actions"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/19":
			_, _ = fmt.Fprintf(writer, `{"number":19,"changed_files":1,"html_url":"https://github.com/owner/repo/pull/19","state":"open","merged":false,"body":"<!-- hive-repair: finding -->","mergeable":true,"mergeable_state":%q,"head":{"ref":%q,"sha":%q,"repo":{"id":123,"full_name":"owner/repo"}},"base":{"ref":"main","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}}}`, mergeableState, branch, head, baseSHA)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/19/files":
			_, _ = io.WriteString(writer, `[{"filename":"visual-hive.config.yaml"}]`)
		case request.Method == http.MethodPut && request.URL.Path == "/repos/owner/repo/pulls/19/update-branch":
			mutationCalls++
			http.Error(writer, "GitHub update-branch mutation is forbidden", http.StatusInternalServerError)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/"+githubMerge:
			_, _ = fmt.Fprintf(writer, `{"sha":%q,"parents":[{"sha":%q},{"sha":%q}]}`, githubMerge, oldHead, baseSHA)
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/owner/repo/commits/") && strings.HasSuffix(request.URL.Path, "/check-runs"):
			_, _ = fmt.Fprintf(writer, `{"total_count":1,"check_runs":[{"id":1,"name":"visual-hive","app":{"id":42},"head_sha":%q,"status":"completed","conclusion":"success"}]}`, head)
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/owner/repo/commits/") && strings.HasSuffix(request.URL.Path, "/status"):
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "finding", Recurrences: 1,
		Status: visualhive.StatusReady, IssueNumber: 16, IssueURL: "https://github.com/owner/repo/issues/16",
		PRNumber: 19, PRURL: "https://github.com/owner/repo/pull/19", Branch: branch, RepairCommitSHA: oldHead,
		RepairAttempts: 10, OwningAgentHint: "quality", HumanReviewRequired: true, ManualReviewKind: "merge_policy", ManualReviewReason: "config review",
	}
	lifecycle := writeRefreshLifecycle(t, stateDir, finding)
	repairStore, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repairStore.Put(repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: "finding", Recurrence: 1, Attempt: 10, AttemptCounted: true,
		Branch: branch, Worktree: worktree, Stage: repair.StagePROpen, Provider: "codex", CommitSHA: oldHead,
		PRNumber: 19, PRURL: finding.PRURL, LifecyclePROpen: true,
		ChangedFiles: []string{"dashboard/src/components/ProjectDashboardView.tsx", "visual-hive.config.yaml"}, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	integratedStore, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	approval := exactTestMergeApproval(strings.Repeat("d", 64))
	approval.PRNumber, approval.HeadSHA, approval.BaseSHA = 19, oldHead, baseSHA
	if err := integratedStore.SaveMergeApproval(approval); err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	config := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir, CheckoutDir: worktree,
		Automation: AutomationAutoMerge, ACMMLevel: 6, MaxRepairAttempts: 4,
		AllowedRepairPaths: []string{"**"}, AllowedAutoMergePaths: []string{"tests/**"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskAutomatic},
		TestCommands: [][]string{{"git", "diff", "--check", "HEAD"}},
	}
	updated, refreshed, err := refreshOwnedRepairBranchIfBehind(ctx, stateDir, config, finding, lifecycle, client, integratedPolicy(config))
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed || mutationCalls != 0 || updated.RepairCommitSHA == oldHead || updated.RepairCommitSHA == githubMerge || updated.RepairAttempts != 10 || updated.Status != visualhive.StatusPROpen || updated.HumanReviewRequired {
		t.Fatalf("unexpected refreshed lifecycle: refreshed=%t mutations=%d finding=%+v", refreshed, mutationCalls, updated)
	}
	repairStore, err = repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, exists := repairStore.Get("finding")
	if !exists || checkpoint.CommitSHA != updated.RepairCommitSHA || checkpoint.Attempt != 10 || checkpoint.CountedModelAttempts() != 10 || len(checkpoint.ChangedFiles) != 1 || checkpoint.ChangedFiles[0] != "visual-hive.config.yaml" {
		t.Fatalf("repair checkpoint was not safely refreshed/pruned: %+v", checkpoint)
	}
	if _, exists, err := integratedStore.LoadMergeApproval(); err != nil || exists {
		t.Fatalf("stale approval was not invalidated: exists=%t err=%v", exists, err)
	}
	if _, exists, err := integratedStore.LoadRepairRefreshIntent(); err != nil || exists {
		t.Fatalf("completed refresh WAL remains: exists=%t err=%v", exists, err)
	}
	if remoteHead() != updated.RepairCommitSHA {
		t.Fatalf("remote tip does not match final owned checkpoint")
	}
	message, err := git(ctx, worktree, "show", "-s", "--format=%B", updated.RepairCommitSHA)
	if err != nil || !hasExactCommitTrailer(message, "Hive-Repository-ID", "123") || !hasExactCommitTrailer(message, "Hive-Operation", "repair") {
		t.Fatalf("final refreshed head lacks ownership: %s err=%v", message, err)
	}
	observation, err := client.InspectRepairPullRequestRefreshExact(ctx, hivegithub.RepairBranchRefreshRequest{
		Repository: config.Repository, RepositoryID: config.RepositoryID, RepositoryFingerprint: finding.RepositoryFingerprint,
		PRNumber: finding.PRNumber, Branch: finding.Branch, ExpectedHeadSHA: updated.RepairCommitSHA,
		ExpectedBaseBranch: config.DefaultBranch, ExpectedBaseSHA: baseSHA, ExpectedChangedFiles: []string{"visual-hive.config.yaml"},
	})
	if err != nil || observation.HeadSHA != updated.RepairCommitSHA || observation.BaseMoved {
		t.Fatalf("final exact PR refresh observation failed: observation=%+v err=%v", observation, err)
	}
	audit, err := os.ReadFile(filepath.Join(stateDir, "integrated", "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "reconcile_repair_changed_files") || !strings.Contains(string(audit), "refresh_repair_branch_complete") {
		t.Fatalf("refresh audit is incomplete: %s err=%v", audit, err)
	}
}

func TestReconcileLiveRepairFilesRejectsAddedPath(t *testing.T) {
	live, stale, err := reconcileLiveRepairFiles([]string{"visual-hive.config.yaml", "stale.ts"}, []string{"visual-hive.config.yaml"})
	if err != nil || len(live) != 1 || len(stale) != 1 || stale[0] != "stale.ts" {
		t.Fatalf("safe stale pruning failed: live=%v stale=%v err=%v", live, stale, err)
	}
	if _, _, err := reconcileLiveRepairFiles([]string{"visual-hive.config.yaml"}, []string{"visual-hive.config.yaml", "src/added.ts"}); err == nil {
		t.Fatal("live added path was accepted outside durable authorization")
	}
}

func TestRepairRefreshConflictTransitionsToSupportedResumableHold(t *testing.T) {
	stateDir := t.TempDir()
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "finding", Recurrences: 1,
		Status: visualhive.StatusReady, IssueNumber: 16, PRNumber: 19, PRURL: "https://github.com/owner/repo/pull/19",
		Branch: "hive/repair-finding-a10", RepairCommitSHA: head, RepairAttempts: 10,
	}
	lifecycle := writeRefreshLifecycle(t, stateDir, finding)
	repairStore, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		t.Fatal(err)
	}
	attempt := repair.Attempt{
		Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint, Recurrence: 1, Attempt: 10,
		AttemptCounted: true, Branch: finding.Branch, Worktree: filepath.Join(stateDir, "worktree"), Stage: repair.StagePROpen,
		Provider: "codex", CommitSHA: head, PRNumber: finding.PRNumber, PRURL: finding.PRURL, LifecyclePROpen: true,
		ChangedFiles: []string{"shared.txt"}, StartedAt: time.Now().UTC(),
	}
	if err := repairStore.Put(attempt); err != nil {
		t.Fatal(err)
	}
	integratedStore, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	intent := RepairRefreshIntent{
		SchemaVersion: RepairRefreshSchema, Repository: finding.Repository, RepositoryID: finding.RepositoryID,
		RepositoryFingerprint: finding.RepositoryFingerprint, Recurrence: 1, Attempt: 10, PRNumber: finding.PRNumber,
		Branch: finding.Branch, OldHeadSHA: head, CurrentHeadSHA: head, BaseBranch: "main", BaseSHA: base,
		ChangedFiles: []string{"shared.txt"}, AuthorizedAt: time.Now().UTC(),
	}
	updated, refreshed, conflictErr := checkpointRepairRefreshConflict(stateDir, Config{Repository: finding.Repository}, finding, lifecycle, integratedStore, repairStore, attempt, intent, errors.New("content conflict"))
	if conflictErr == nil || !repair.IsResumableFailureError(conflictErr) || refreshed || updated.Status != visualhive.StatusNeedsRevision ||
		!updated.HumanReviewRequired || updated.ManualReviewKind != "repair_base_conflict" || !strings.Contains(conflictErr.Error(), "hive retry-repair") {
		t.Fatalf("conflict did not produce actionable resumable hold: refreshed=%t finding=%+v err=%v", refreshed, updated, conflictErr)
	}
	paused, exists := repairStore.Get(finding.RepositoryFingerprint)
	if !exists || paused.Stage != repair.StageFailed || paused.ResumeStage != repair.StagePROpen || paused.LastFailureClass != repair.FailureInfrastructure || paused.Attempt != 10 || paused.CountedModelAttempts() != 10 {
		t.Fatalf("conflict did not preserve exact model attempt in failed PR checkpoint: %+v", paused)
	}
	if _, exists, err := integratedStore.LoadRepairRefreshIntent(); err != nil || exists {
		t.Fatalf("conflict WAL was not safely transitioned after durable hold: exists=%t err=%v", exists, err)
	}
	resumed, err := repairStore.ResumeRetry(repair.RetryRequest{
		RepositoryFingerprint: finding.RepositoryFingerprint, ExpectedRecurrence: 1, ExpectedAttempt: 10,
		ExpectedFailureClass: repair.FailureInfrastructure, ExpectedFailureID: paused.LastFailureID,
		Actor: "operator", Reason: "reviewed exact base conflict",
	})
	if err != nil || resumed.Stage != repair.StagePROpen || resumed.Attempt != 10 || resumed.CountedModelAttempts() != 10 || resumed.PRNumber != finding.PRNumber || resumed.Branch != finding.Branch {
		t.Fatalf("supported retry did not resume same PR without a model attempt: %+v err=%v", resumed, err)
	}
}

func TestRepairRefreshWALRecoversBeforeAndAfterExactLeasePush(t *testing.T) {
	for _, pushedBeforeRecovery := range []bool{false, true} {
		name := "before_push"
		if pushedBeforeRecovery {
			name = "after_push"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			stateDir := t.TempDir()
			remote, worktree, oldHead, baseSHA, _ := setupIntegratedRefreshRepository(t)
			branch := "hive/repair-finding-a10"
			finding := visualhive.FindingLifecycle{
				Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: "finding", Recurrences: 1,
				Status: visualhive.StatusPROpen, IssueNumber: 16, PRNumber: 19, PRURL: "https://github.com/owner/repo/pull/19",
				Branch: branch, RepairCommitSHA: oldHead, RepairAttempts: 10,
			}
			lifecycle := writeRefreshLifecycle(t, stateDir, finding)
			repairStore, err := repair.NewStore(filepath.Join(stateDir, "repair"))
			if err != nil {
				t.Fatal(err)
			}
			attempt := repair.Attempt{
				Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint, Recurrence: 1, Attempt: 10,
				AttemptCounted: true, Branch: branch, Worktree: worktree, Stage: repair.StagePROpen, Provider: "codex",
				CommitSHA: oldHead, PRNumber: 19, PRURL: finding.PRURL, LifecyclePROpen: true,
				ChangedFiles: []string{"visual-hive.config.yaml"}, StartedAt: time.Now().UTC(),
			}
			if err := repairStore.Put(attempt); err != nil {
				t.Fatal(err)
			}
			baselineProtection, err := repair.InspectVisualBaselineProtection(ctx, worktree)
			if err != nil {
				t.Fatal(err)
			}
			request := repair.RefreshedBranchCheckpointRequest{
				Worktree: worktree, Branch: branch, RepositoryID: "123", ExpectedRemoteURL: remote, RemoteHeadSHA: oldHead, BaseBranch: "main", BaseSHA: baseSHA,
				BaselineProtection:   baselineProtection,
				ExpectedChangedFiles: []string{"visual-hive.config.yaml"}, ValidationCommands: []repair.Command{{Name: "git", Args: []string{"diff", "--check", "HEAD"}}},
			}
			checkpoint, err := repair.PrepareRefreshedRepairBranch(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			integratedStore, err := NewStore(filepath.Join(stateDir, "integrated"))
			if err != nil {
				t.Fatal(err)
			}
			intent := RepairRefreshIntent{
				SchemaVersion: RepairRefreshSchema, Repository: finding.Repository, RepositoryID: finding.RepositoryID,
				RepositoryFingerprint: finding.RepositoryFingerprint, Recurrence: 1, Attempt: 10, PRNumber: 19, Branch: branch,
				OldHeadSHA: oldHead, CurrentHeadSHA: oldHead, BaseBranch: "main", BaseSHA: baseSHA,
				ChangedFiles: []string{"visual-hive.config.yaml"}, ContributionPatchID: checkpoint.ContributionPatchID,
				CandidateHeadSHA: checkpoint.HeadSHA, AuthorizedAt: time.Now().UTC(), LocalValidationComplete: time.Now().UTC(),
			}
			if err := integratedStore.SaveRepairRefreshIntent(intent); err != nil {
				t.Fatal(err)
			}
			if pushedBeforeRecovery {
				if err := repair.PushRefreshedRepairBranchExact(ctx, request, checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			remoteHead := func() string {
				data, err := os.ReadFile(filepath.Join(remote, "refs", "heads", filepath.FromSlash(branch)))
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(data))
			}
			server := newIntegratedGateTestServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/repos/owner/repo":
					_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
				case "/repos/owner/repo/branches/main/protection":
					_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"checks":[{"context":"visual-hive","app_id":42}]},"enforce_admins":{"enabled":true}}`)
				case "/repos/owner/repo/pulls/19":
					_, _ = fmt.Fprintf(writer, `{"number":19,"changed_files":1,"state":"open","merged":false,"body":"<!-- hive-repair: finding -->","mergeable_state":"clean","head":{"ref":%q,"sha":%q,"repo":{"id":123,"full_name":"owner/repo"}},"base":{"ref":"main","sha":%q,"repo":{"id":123,"full_name":"owner/repo"}}}`, branch, remoteHead(), baseSHA)
				case "/repos/owner/repo/pulls/19/files":
					_, _ = io.WriteString(writer, `[{"filename":"visual-hive.config.yaml"}]`)
				default:
					http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			config := Config{Repository: finding.Repository, RepositoryID: finding.RepositoryID, DefaultBranch: "main", StateDir: stateDir, CheckoutDir: worktree, Automation: AutomationAutoMerge, ACMMLevel: 6, TestCommands: [][]string{{"git", "diff", "--check", "HEAD"}}}
			updated, refreshed, err := refreshOwnedRepairBranchIfBehind(ctx, stateDir, config, finding, lifecycle, client, integratedPolicy(config))
			if err != nil || !refreshed || updated.RepairCommitSHA != checkpoint.HeadSHA || remoteHead() != checkpoint.HeadSHA || updated.RepairAttempts != 10 {
				t.Fatalf("WAL recovery failed: pushed_before=%t refreshed=%t finding=%+v remote=%s err=%v", pushedBeforeRecovery, refreshed, updated, remoteHead(), err)
			}
			if _, exists, err := integratedStore.LoadRepairRefreshIntent(); err != nil || exists {
				t.Fatalf("completed crash recovery retained WAL: exists=%t err=%v", exists, err)
			}
		})
	}
}

func setupIntegratedRefreshRepository(t *testing.T) (remote, worktree, oldHead, baseSHA, githubMerge string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote, worktree = filepath.Join(root, "remote.git"), filepath.Join(root, "worktree")
	oldCloneURL := setupRepositoryCloneURL
	setupRepositoryCloneURL = func(string) string { return remote }
	t.Cleanup(func() { setupRepositoryCloneURL = oldCloneURL })
	if _, err := git(ctx, root, "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, worktree, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, worktree, "config", "--local", "user.name", "Hive Integrated Test"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, worktree, "config", "--local", "user.email", "hive-integrated-test@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, worktree, "remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	writeRefreshRepoFile(t, worktree, "README.md", "base\n")
	commitRefreshRepo(t, worktree, "initial")
	if _, err := git(ctx, worktree, "checkout", "-b", "hive/repair-finding-a10"); err != nil {
		t.Fatal(err)
	}
	writeRefreshRepoFile(t, worktree, "visual-hive.config.yaml", "project: {}\n")
	commitRefreshRepo(t, worktree, "fix: visual config\n\nHive-Repository-ID: 123\nHive-Operation: repair")
	oldHead = refreshRepoOutput(t, worktree, "rev-parse", "HEAD")
	runIntegratedGit(t, worktree, "push", "origin", oldHead+":refs/heads/hive/repair-finding-a10")
	if _, err := git(ctx, worktree, "checkout", "main"); err != nil {
		t.Fatal(err)
	}
	writeRefreshRepoFile(t, worktree, "base.txt", "advanced\n")
	commitRefreshRepo(t, worktree, "advance base")
	baseSHA = refreshRepoOutput(t, worktree, "rev-parse", "HEAD")
	runIntegratedGit(t, worktree, "push", "origin", baseSHA+":refs/heads/main")
	if _, err := git(ctx, worktree, "checkout", "hive/repair-finding-a10"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, worktree, "merge", "--no-ff", "main", "-m", "GitHub Update Branch merge"); err != nil {
		t.Fatal(err)
	}
	githubMerge = refreshRepoOutput(t, worktree, "rev-parse", "HEAD")
	if _, err := git(ctx, worktree, "reset", "--hard", oldHead); err != nil {
		t.Fatal(err)
	}
	return remote, worktree, oldHead, baseSHA, githubMerge
}

func writeRefreshLifecycle(t *testing.T, stateDir string, finding visualhive.FindingLifecycle) *visualhive.LifecycleStore {
	t.Helper()
	dir := filepath.Join(stateDir, "visual-hive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := visualhive.LifecycleState{SchemaVersion: visualhive.LifecycleSchema, Findings: map[string]*visualhive.FindingLifecycle{finding.RepositoryFingerprint: &finding}, ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := visualhive.NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func writeRefreshRepoFile(t *testing.T, root, relative, value string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitRefreshRepo(t *testing.T, root, message string) {
	t.Helper()
	if _, err := git(context.Background(), root, "add", "--all"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(context.Background(), root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", message); err != nil {
		t.Fatal(err)
	}
}

func refreshRepoOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	value, err := git(context.Background(), root, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(value)
}

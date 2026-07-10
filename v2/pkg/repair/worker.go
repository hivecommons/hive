package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

type Lifecycle interface {
	MarkRepairStarted(repositoryFingerprint, branch string) error
	MarkPROpen(repositoryFingerprint, commitSHA string, number int, prURL string) error
	RecordAuthorization(repositoryFingerprint, action string, allowed bool, detail string)
}

type PullRequestClient interface {
	UpsertRepairPullRequest(ctx context.Context, repository, branch, base, title, body, marker string) (hivegithub.RepairPullRequest, error)
}

type Command struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
}

type Config struct {
	RepositoryDir      string
	WorktreeRoot       string
	BaseBranch         string
	Policy             automation.Policy
	AllowedRepairPaths []string
	ValidationCommands []Command
	ModelTimeout       time.Duration
	CommandTimeout     time.Duration
}

type Result struct {
	RepositoryFingerprint string   `json:"repository_fingerprint"`
	Branch                string   `json:"branch"`
	CommitSHA             string   `json:"commit_sha"`
	PRNumber              int      `json:"pr_number"`
	PRURL                 string   `json:"pr_url"`
	ChangedFiles          []string `json:"changed_files"`
	Resumed               bool     `json:"resumed"`
}

type Worker struct {
	Config    Config
	Provider  Provider
	State     *Store
	Lifecycle Lifecycle
	GitHub    PullRequestClient
}

func (w *Worker) Run(ctx context.Context, finding visualhive.FindingLifecycle) (Result, error) {
	if err := w.validate(finding); err != nil {
		return Result{}, err
	}
	attempt, resumed := w.State.Get(finding.RepositoryFingerprint)
	startNewAttempt := !resumed
	if resumed && attempt.Stage == StagePROpen && finding.Status == visualhive.StatusNeedsRevision {
		// A failed check iterates on the same Hive branch and PR. Creating a new
		// branch here would violate the one-active-PR invariant and strand review
		// history. Reset only the durable worker stage.
		attempt.Attempt = finding.RepairAttempts + 1
		attempt.Stage = StagePrepared
		attempt.StartedAt = time.Now().UTC()
		if err := w.State.Put(attempt); err != nil {
			return Result{}, err
		}
	} else if resumed && attempt.Stage == StagePROpen && finding.RepairAttempts >= attempt.Attempt &&
		(finding.Status == visualhive.StatusIssueOpen || finding.Status == visualhive.StatusFixQueued) {
		startNewAttempt = true
	}
	if startNewAttempt {
		attemptNumber := finding.RepairAttempts + 1
		branch := fmt.Sprintf("hive/repair-%s-a%d", shortFingerprint(finding.RepositoryFingerprint), attemptNumber)
		attempt = Attempt{
			Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint, Attempt: attemptNumber,
			Branch: branch, Worktree: filepath.Join(w.Config.WorktreeRoot, shortFingerprint(finding.RepositoryFingerprint)),
			Stage: StagePrepared, Provider: w.Provider.Name(), StartedAt: time.Now().UTC(),
		}
		if err := w.authorize(finding, automation.ActionCreateBranch, nil); err != nil {
			return Result{}, err
		}
		if err := prepareWorktree(ctx, w.Config.RepositoryDir, attempt.Worktree, attempt.Branch, w.Config.BaseBranch); err != nil {
			return Result{}, err
		}
		if err := w.State.Put(attempt); err != nil {
			return Result{}, err
		}
	}

	if attempt.Stage == StagePrepared {
		if err := w.authorize(finding, automation.ActionRepairModel, nil); err != nil {
			return Result{}, err
		}
		if finding.Status == visualhive.StatusIssueOpen || finding.Status == visualhive.StatusFixQueued || finding.Status == visualhive.StatusNeedsRevision {
			if err := w.Lifecycle.MarkRepairStarted(finding.RepositoryFingerprint, attempt.Branch); err != nil {
				return Result{}, err
			}
		}
		healthCtx, cancelHealth := context.WithTimeout(ctx, 45*time.Second)
		err := w.Provider.Health(healthCtx)
		cancelHealth()
		if err != nil {
			return Result{}, err
		}
		modelTimeout := w.Config.ModelTimeout
		if modelTimeout <= 0 {
			modelTimeout = 20 * time.Minute
		}
		modelCtx, cancelModel := context.WithTimeout(ctx, modelTimeout)
		err = w.Provider.Run(modelCtx, attempt.Worktree, repairPrompt(finding))
		cancelModel()
		if err != nil {
			return Result{}, err
		}
		attempt.Stage = StageModelComplete
		if err := w.State.Put(attempt); err != nil {
			return Result{}, err
		}
	}

	if attempt.Stage == StageModelComplete {
		files, err := changedFiles(ctx, attempt.Worktree)
		if err != nil {
			return Result{}, err
		}
		if len(files) == 0 {
			status, _ := runGit(ctx, attempt.Worktree, "status", "--short", "--untracked-files=all")
			return Result{}, fmt.Errorf("model completed without a source or test change (git status: %s)", safeExcerpt(status))
		}
		if err := validateChangedFiles(files, w.Config.AllowedRepairPaths); err != nil {
			return Result{}, err
		}
		for _, command := range w.Config.ValidationCommands {
			if err := runValidation(ctx, attempt.Worktree, command, w.Config.CommandTimeout); err != nil {
				return Result{}, err
			}
		}
		attempt.ChangedFiles, attempt.Stage = files, StageValidated
		if err := w.State.Put(attempt); err != nil {
			return Result{}, err
		}
	}

	if attempt.Stage == StageValidated {
		if err := w.authorize(finding, automation.ActionCommit, attempt.ChangedFiles); err != nil {
			return Result{}, err
		}
		sha, err := commitRepair(ctx, attempt.Worktree, attempt.ChangedFiles, finding.Title, finding.IssueNumber)
		if err != nil {
			return Result{}, err
		}
		attempt.CommitSHA, attempt.Stage = sha, StageCommitted
		if err := w.State.Put(attempt); err != nil {
			return Result{}, err
		}
	}

	if attempt.Stage == StageCommitted {
		if err := w.authorize(finding, automation.ActionPush, attempt.ChangedFiles); err != nil {
			return Result{}, err
		}
		if _, err := runGit(ctx, attempt.Worktree, "push", "--force-with-lease", "origin", "HEAD:refs/heads/"+attempt.Branch); err != nil {
			return Result{}, fmt.Errorf("push repair branch: %w", err)
		}
		attempt.Stage = StagePushed
		if err := w.State.Put(attempt); err != nil {
			return Result{}, err
		}
	}

	if attempt.Stage == StagePushed {
		if err := w.authorize(finding, automation.ActionCreatePR, attempt.ChangedFiles); err != nil {
			return Result{}, err
		}
		marker := fmt.Sprintf("<!-- hive-repair: %s -->", finding.RepositoryFingerprint)
		body := repairPRBody(marker, finding, attempt)
		pull, err := w.GitHub.UpsertRepairPullRequest(ctx, finding.Repository, attempt.Branch, w.Config.BaseBranch, "Hive repair: "+finding.Title, body, marker)
		if err != nil {
			return Result{}, err
		}
		if pull.HeadSHA != "" && pull.HeadSHA != attempt.CommitSHA {
			return Result{}, fmt.Errorf("GitHub repair PR head %s does not match pushed commit %s", pull.HeadSHA, attempt.CommitSHA)
		}
		attempt.PRNumber, attempt.PRURL, attempt.Stage = pull.Number, pull.URL, StagePROpen
		if err := w.State.Put(attempt); err != nil {
			return Result{}, err
		}
		if err := w.Lifecycle.MarkPROpen(finding.RepositoryFingerprint, attempt.CommitSHA, pull.Number, pull.URL); err != nil {
			return Result{}, err
		}
	}

	return Result{
		RepositoryFingerprint: finding.RepositoryFingerprint, Branch: attempt.Branch, CommitSHA: attempt.CommitSHA,
		PRNumber: attempt.PRNumber, PRURL: attempt.PRURL, ChangedFiles: append([]string(nil), attempt.ChangedFiles...), Resumed: resumed,
	}, nil
}

func (w *Worker) validate(finding visualhive.FindingLifecycle) error {
	if w.Provider == nil || w.State == nil || w.Lifecycle == nil || w.GitHub == nil {
		return fmt.Errorf("provider, state, lifecycle, and GitHub client are required")
	}
	if strings.TrimSpace(w.Config.RepositoryDir) == "" || strings.TrimSpace(w.Config.WorktreeRoot) == "" || strings.TrimSpace(w.Config.BaseBranch) == "" {
		return fmt.Errorf("repository directory, worktree root, and base branch are required")
	}
	if finding.Repository == "" || finding.RepositoryFingerprint == "" || finding.IssueNumber <= 0 || finding.IssueURL == "" {
		return fmt.Errorf("repair requires a persisted finding and GitHub issue")
	}
	return os.MkdirAll(w.Config.WorktreeRoot, 0o700)
}

func (w *Worker) authorize(finding visualhive.FindingLifecycle, action automation.Action, files []string) error {
	decision := w.Config.Policy.Authorize(automation.ActionRequest{
		Action: action, Agent: repairActor(finding.OwningAgentHint), Repository: finding.Repository,
		RepairAttempts: finding.RepairAttempts, Risk: riskForFiles(files), ChangedFiles: files,
	})
	w.Lifecycle.RecordAuthorization(finding.RepositoryFingerprint, string(action), decision.Allowed, strings.Join(decision.Reasons, "; "))
	if !decision.Allowed {
		return fmt.Errorf("%s denied: %s", action, strings.Join(decision.Reasons, "; "))
	}
	return nil
}

func prepareWorktree(ctx context.Context, repositoryDir, worktree, branch, base string) error {
	if _, err := os.Stat(filepath.Join(worktree, ".git")); err == nil {
		current, gitErr := runGit(ctx, worktree, "branch", "--show-current")
		if gitErr != nil || strings.TrimSpace(current) != branch {
			return fmt.Errorf("existing repair worktree is not on expected branch %s", branch)
		}
		return nil
	}
	if _, err := runGit(ctx, repositoryDir, "fetch", "--prune", "origin", base); err != nil {
		return fmt.Errorf("fetch repair base: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		return err
	}
	if _, err := runGit(ctx, repositoryDir, "worktree", "add", "-B", branch, worktree, "origin/"+base); err != nil {
		return fmt.Errorf("create repair worktree: %w", err)
	}
	return nil
}

func changedFiles(ctx context.Context, worktree string) ([]string, error) {
	output, err := runGit(ctx, worktree, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	values := strings.Split(output, "\x00")
	files := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) < 4 {
			continue
		}
		name := strings.TrimSpace(value[3:])
		if arrow := strings.LastIndex(name, " -> "); arrow >= 0 {
			name = name[arrow+4:]
		}
		name = filepath.ToSlash(name)
		if name != "" {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	return uniqueStrings(files), nil
}

func validateChangedFiles(files, allowedPatterns []string) error {
	if len(allowedPatterns) == 0 {
		allowedPatterns = []string{"src/**", "test/**", "tests/**", "**/*.test.*", "**/*.spec.*", "**/*_test.go"}
	}
	for _, file := range files {
		normalized := strings.TrimPrefix(filepath.ToSlash(file), "./")
		lower := strings.ToLower(normalized)
		if strings.HasPrefix(lower, ".github/") || strings.Contains(lower, "baseline") || strings.Contains(lower, "secret") ||
			strings.Contains(lower, "auth") || strings.HasPrefix(lower, "deploy") || strings.Contains(lower, "terraform") {
			return fmt.Errorf("repair changed restricted path %s; explicit human authority is required", normalized)
		}
		allowed := false
		for _, pattern := range allowedPatterns {
			if matchPathPattern(pattern, normalized) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("repair changed %s outside the configured repair allowlist", normalized)
		}
	}
	return nil
}

func matchPathPattern(pattern, file string) bool {
	pattern = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(pattern)), "./")
	if strings.HasPrefix(pattern, "**/") {
		if matched, _ := path.Match(strings.TrimPrefix(pattern, "**/"), path.Base(file)); matched {
			return true
		}
	}
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(file, strings.TrimSuffix(pattern, "**"))
	}
	matched, _ := path.Match(pattern, file)
	return matched
}

func runValidation(ctx context.Context, worktree string, command Command, timeout time.Duration) error {
	if strings.TrimSpace(command.Name) == "" {
		return fmt.Errorf("validation command executable is required")
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	process := exec.CommandContext(commandCtx, command.Name, command.Args...)
	process.Dir = worktree
	process.Env = providerEnvironment()
	var output limitedBuffer
	process.Stdout, process.Stderr = &output, &output
	if err := process.Run(); err != nil {
		return fmt.Errorf("validation %s failed: %w: %s", command.Name, err, safeExcerpt(output.String()))
	}
	return nil
}

func commitRepair(ctx context.Context, worktree string, files []string, title string, issue int) (string, error) {
	args := append([]string{"add", "--"}, files...)
	if _, err := runGit(ctx, worktree, args...); err != nil {
		return "", fmt.Errorf("stage repair: %w", err)
	}
	message := fmt.Sprintf("fix: %s\n\nRefs #%d", strings.TrimSpace(title), issue)
	if _, err := runGit(ctx, worktree, "-c", "user.name=Hive Repair Agent", "-c", "user.email=hive-repair@users.noreply.github.com", "commit", "-m", message); err != nil {
		return "", fmt.Errorf("commit repair: %w", err)
	}
	sha, err := runGit(ctx, worktree, "rev-parse", "HEAD")
	return strings.TrimSpace(sha), err
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = append(providerEnvironment(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.interactive", "GIT_CONFIG_VALUE_0=false")
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return output.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, safeExcerpt(output.String()))
	}
	return output.String(), nil
}

func repairPrompt(finding visualhive.FindingLifecycle) string {
	return fmt.Sprintf(`You are a Hive repair worker in an isolated Git worktree. Make the smallest production-quality source or test change that resolves the confirmed finding below.

Finding: %s
Issue: %s
Kind: %s
Severity: %s
Affected contracts: %s
Required narrow validation: %s
Prior hosted check result: %s

Evidence:
%s

Rules:
- Do not run git, commit, push, open a pull request, or access GitHub. Hive owns those operations.
- Do not edit workflows, authentication, authorization, secrets, deployment, infrastructure, dependencies, or visual baselines.
- Do not weaken assertions, thresholds, coverage, mutation requirements, security checks, or ignore failures.
- Do not create or approve a new visual baseline.
- Inspect the repository and implement a real fix, not a hardcoded proof fixture.
- Run the narrow reproduction when practical. Hive will independently run the required commands afterward.
- If the safe fix exceeds this authority, make no changes and explain why.
`, finding.Title, finding.IssueURL, finding.IssueKind, finding.Severity, strings.Join(finding.AffectedContracts, ", "), finding.ValidationCommand, finding.LastCheckSummary, finding.Body)
}

func repairPRBody(marker string, finding visualhive.FindingLifecycle, attempt Attempt) string {
	return fmt.Sprintf("%s\n\nAutomated Hive repair for %s.\n\nRefs #%d\n\n- Finding: `%s`\n- Commit: `%s`\n- Provider: `%s`\n- Changed files: %d\n\nHive intentionally uses `Refs` rather than a closing keyword. The issue remains open until a complete authoritative target-branch Visual Hive run confirms the finding is absent.",
		marker, finding.IssueURL, finding.IssueNumber, finding.RepositoryFingerprint, attempt.CommitSHA, attempt.Provider, len(attempt.ChangedFiles))
}

func shortFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func repairActor(hint string) string {
	lower := strings.ToLower(hint)
	if strings.Contains(lower, "security") || strings.Contains(lower, "sec-check") {
		return "sec-check"
	}
	if strings.Contains(lower, "ci") {
		return "ci-maintainer"
	}
	return "quality"
}

func riskForFiles(files []string) automation.RiskTier {
	if len(files) == 0 {
		return automation.RiskAutomatic
	}
	for _, file := range files {
		if strings.HasPrefix(file, "src/") {
			return automation.RiskLow
		}
	}
	return automation.RiskAutomatic
}

func uniqueStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

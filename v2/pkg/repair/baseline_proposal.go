package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

type ReviewPullRequestClient interface {
	UpsertReviewPullRequest(ctx context.Context, repository, branch, expectedHeadSHA, base, title, body, marker string) (hivegithub.RepairPullRequest, error)
}

type BaselineProposalConfig struct {
	RepositoryDir      string
	WorktreeRoot       string
	BaseBranch         string
	ExpectedRemoteURL  string
	ValidationCommands []Command
	Environment        map[string]string
	CommandTimeout     time.Duration
}

type BaselineProposalSource struct {
	WorkflowRunID int64
	ArtifactID    int64
	RunURL        string
}

// CreateBaselineProposal copies review candidates into a separate draft PR.
// It never alters the repair PR, marks a candidate approved, or merges either
// PR. Checkpoints in repair state make ambiguous push/PR responses retry-safe.
func CreateBaselineProposal(ctx context.Context, config BaselineProposalConfig, finding visualhive.FindingLifecycle, source BaselineProposalSource, hosted *BaselineReview, state *Store, github ReviewPullRequestClient) (Attempt, error) {
	if !positiveRepositoryID(finding.RepositoryID) {
		return Attempt{}, fmt.Errorf("baseline proposal requires a trusted positive repository ID")
	}
	attempt, ok := state.Get(finding.RepositoryFingerprint)
	if !ok || attempt.Stage != StagePROpen || attempt.BaselineReview == nil || attempt.PRNumber <= 0 || attempt.CommitSHA == "" {
		return Attempt{}, fmt.Errorf("baseline proposal requires a persisted open repair PR and candidate review")
	}
	if attempt.BaselineReview.Status == BaselineReviewProposalOpen {
		return attempt, nil
	}
	if attempt.BaselineReview.Status != BaselineReviewCandidateReady || hosted == nil || hosted.Status != BaselineReviewCandidateReady {
		return Attempt{}, fmt.Errorf("baseline proposal requires local and hosted candidate-ready evidence")
	}
	candidates, err := combineBaselineCandidates(attempt.BaselineReview.Candidates, hosted.Candidates)
	if err != nil {
		return Attempt{}, err
	}
	review := cloneBaselineReview(attempt.BaselineReview)
	review.Candidates = candidates
	review.RepairHeadSHA = attempt.CommitSHA
	review.SourceWorkflowRun = source.WorkflowRunID
	review.SourceArtifactID = source.ArtifactID
	review.SourceRunURL = source.RunURL
	if review.ProposalBranch == "" {
		review.ProposalBranch = fmt.Sprintf("hive/baseline-%s-a%d", shortFingerprint(finding.RepositoryFingerprint), attempt.Attempt)
	}
	attempt.BaselineReview = review
	if err := state.Put(attempt); err != nil {
		return Attempt{}, err
	}
	worktree := filepath.Join(config.WorktreeRoot, shortFingerprint(finding.RepositoryFingerprint))
	if err := prepareWorktree(ctx, config.RepositoryDir, worktree, review.ProposalBranch, config.BaseBranch, "", config.ExpectedRemoteURL); err != nil {
		return Attempt{}, err
	}
	expectedFiles := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if err := verifyCandidateFile(candidate); err != nil {
			return Attempt{}, err
		}
		destination := filepath.Join(worktree, filepath.FromSlash(candidate.BaselinePath))
		if !insideRoot(worktree, destination) {
			return Attempt{}, fmt.Errorf("baseline destination escaped proposal worktree")
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return Attempt{}, err
		}
		data, err := os.ReadFile(candidate.ActualPath)
		if err != nil {
			return Attempt{}, err
		}
		if err := os.WriteFile(destination, data, 0o600); err != nil {
			return Attempt{}, err
		}
		expectedFiles = append(expectedFiles, candidate.BaselinePath)
	}
	sort.Strings(expectedFiles)
	files, err := changedFiles(ctx, worktree)
	if err != nil {
		return Attempt{}, err
	}
	if len(files) == 0 && review.ProposalCommitSHA == "" {
		head, headErr := runGit(ctx, worktree, "rev-parse", "HEAD")
		if headErr != nil {
			return Attempt{}, headErr
		}
		headFiles, showErr := runGit(ctx, worktree, "show", "--pretty=format:", "--name-only", strings.TrimSpace(head))
		if showErr != nil || !equalStringSets(nonEmptyLines(headFiles), expectedFiles) {
			return Attempt{}, fmt.Errorf("baseline proposal has no candidate changes")
		}
		review.ProposalCommitSHA = strings.TrimSpace(head)
	}
	if len(files) > 0 {
		if !equalStringSets(files, expectedFiles) || !baselineFilesOnly(files) {
			return Attempt{}, fmt.Errorf("baseline proposal changed unexpected files: %s", strings.Join(files, ", "))
		}
		args := append([]string{"add", "--"}, files...)
		if _, err := runGit(ctx, worktree, args...); err != nil {
			return Attempt{}, fmt.Errorf("stage baseline proposal: %w", err)
		}
		message := fmt.Sprintf("test(visual): propose baseline review\n\nRefs #%d\n\nHive-Repository-ID: %s\nHive-Operation: baseline", finding.IssueNumber, strings.TrimSpace(finding.RepositoryID))
		if _, err := runGit(ctx, worktree, "-c", "user.name=Hive Baseline Review", "-c", "user.email=hive-review@users.noreply.github.com", "commit", "-m", message); err != nil {
			return Attempt{}, fmt.Errorf("commit baseline proposal: %w", err)
		}
		head, err := runGit(ctx, worktree, "rev-parse", "HEAD")
		if err != nil {
			return Attempt{}, err
		}
		review.ProposalCommitSHA = strings.TrimSpace(head)
	}
	attempt.BaselineReview = review
	if err := state.Put(attempt); err != nil {
		return Attempt{}, err
	}
	if err := pushRepairBranchExact(ctx, worktree, config.ExpectedRemoteURL, review.ProposalBranch, review.ProposalCommitSHA, finding.RepositoryID, "baseline"); err != nil {
		return Attempt{}, fmt.Errorf("push baseline proposal: %w", err)
	}
	marker := fmt.Sprintf("<!-- hive-baseline-review: %s:%s -->", finding.RepositoryFingerprint, review.RepairHeadSHA)
	pull, err := github.UpsertReviewPullRequest(ctx, finding.Repository, review.ProposalBranch, review.ProposalCommitSHA, config.BaseBranch,
		fmt.Sprintf("Review Visual Hive baselines for #%d", finding.IssueNumber), baselineProposalBody(marker, finding, attempt, review), marker)
	if err != nil {
		return Attempt{}, err
	}
	if strings.TrimSpace(pull.HeadSHA) == "" || !strings.EqualFold(strings.TrimSpace(pull.HeadSHA), review.ProposalCommitSHA) {
		return Attempt{}, fmt.Errorf("baseline proposal PR head %s does not match pushed commit %s", pull.HeadSHA, review.ProposalCommitSHA)
	}
	review.ProposalPRNumber, review.ProposalPRURL, review.Status = pull.Number, pull.URL, BaselineReviewProposalOpen
	attempt.BaselineReview = review
	if err := state.Put(attempt); err != nil {
		return Attempt{}, err
	}
	return attempt, nil
}

// ResumeAfterBaselineApproval accepts only an already verified, manually
// merged proposal. It syncs the approved target-branch baseline into the
// original repair branch, reruns every required local command, pushes the new
// exact head, and updates the existing repair PR without creating a new one.
func ResumeAfterBaselineApproval(ctx context.Context, config BaselineProposalConfig, finding visualhive.FindingLifecycle, proposalMergeSHA string, state *Store, github PullRequestClient) (Attempt, error) {
	if !positiveRepositoryID(finding.RepositoryID) {
		return Attempt{}, fmt.Errorf("baseline approval requires a trusted positive repository ID")
	}
	attempt, ok := state.Get(finding.RepositoryFingerprint)
	if !ok || attempt.Stage != StagePROpen || attempt.BaselineReview == nil || attempt.BaselineReview.Status != BaselineReviewProposalOpen {
		return Attempt{}, fmt.Errorf("baseline approval requires a pending proposal and open repair PR")
	}
	if strings.TrimSpace(proposalMergeSHA) == "" {
		return Attempt{}, fmt.Errorf("baseline proposal merge SHA is required")
	}
	expectedRemoteURL, err := validateExpectedRemoteURL(ctx, attempt.Worktree, config.ExpectedRemoteURL)
	if err != nil {
		return Attempt{}, err
	}
	baseRefspec := "+refs/heads/" + config.BaseBranch + ":refs/remotes/origin/" + config.BaseBranch
	if _, err := runGitTransport(ctx, attempt.Worktree, expectedRemoteURL, "fetch", "--prune", expectedRemoteURL, baseRefspec); err != nil {
		return Attempt{}, fmt.Errorf("fetch approved baseline: %w", err)
	}
	mergeMessage := fmt.Sprintf("test(visual): sync approved baselines\n\nHive-Repository-ID: %s\nHive-Operation: repair", strings.TrimSpace(finding.RepositoryID))
	if _, err := runGit(ctx, attempt.Worktree, "-c", "user.name=Hive Repair Agent", "-c", "user.email=hive-repair@users.noreply.github.com", "merge", "--no-ff", "-m", mergeMessage, "origin/"+config.BaseBranch); err != nil {
		_, _ = runGit(context.Background(), attempt.Worktree, "merge", "--abort")
		return Attempt{}, fmt.Errorf("merge approved baseline into repair branch: %w", err)
	}
	for _, command := range config.ValidationCommands {
		if err := runRepairCommand(ctx, attempt.Worktree, command, config.CommandTimeout, config.Environment, "post-baseline validation"); err != nil {
			return Attempt{}, err
		}
	}
	sha, err := runGit(ctx, attempt.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return Attempt{}, err
	}
	sha = strings.TrimSpace(sha)
	if err := pushRepairBranchExact(ctx, attempt.Worktree, expectedRemoteURL, attempt.Branch, sha, finding.RepositoryID, "repair"); err != nil {
		return Attempt{}, fmt.Errorf("push repair after baseline approval: %w", err)
	}
	attempt.CommitSHA = sha
	attempt.BaselineReview.ProposalMergeSHA = proposalMergeSHA
	attempt.BaselineReview.Status = BaselineReviewApproved
	marker := fmt.Sprintf("<!-- hive-repair: %s -->", finding.RepositoryFingerprint)
	pull, err := github.UpsertRepairPullRequest(ctx, finding.Repository, attempt.Branch, attempt.CommitSHA, config.BaseBranch, "Hive repair: "+finding.Title, repairPRBody(marker, finding, attempt), marker)
	if err != nil {
		return Attempt{}, err
	}
	if pull.Number != attempt.PRNumber || strings.TrimSpace(pull.HeadSHA) == "" || !strings.EqualFold(strings.TrimSpace(pull.HeadSHA), sha) {
		return Attempt{}, fmt.Errorf("updated repair PR does not match the persisted PR and exact head")
	}
	attempt.PRURL = pull.URL
	if err := state.Put(attempt); err != nil {
		return Attempt{}, err
	}
	return attempt, nil
}

func positiveRepositoryID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func combineBaselineCandidates(local, hosted []BaselineCandidate) ([]BaselineCandidate, error) {
	if len(local) == 0 || len(hosted) == 0 {
		return nil, fmt.Errorf("local and hosted baseline candidates are required")
	}
	localIdentities := map[string]bool{}
	hostedIdentities := map[string]bool{}
	byPath := map[string]BaselineCandidate{}
	for _, candidate := range local {
		localIdentities[baselineIdentity(candidate)] = true
		byPath[candidate.BaselinePath] = candidate
	}
	for _, candidate := range hosted {
		hostedIdentities[baselineIdentity(candidate)] = true
		byPath[candidate.BaselinePath] = candidate // hosted evidence wins for the CI platform
	}
	if len(localIdentities) != len(hostedIdentities) {
		return nil, fmt.Errorf("hosted baseline candidates do not match local repair evidence")
	}
	for identity := range localIdentities {
		if !hostedIdentities[identity] {
			return nil, fmt.Errorf("hosted baseline candidate %q is missing", identity)
		}
	}
	result := make([]BaselineCandidate, 0, len(byPath))
	for _, candidate := range byPath {
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].BaselinePath < result[j].BaselinePath })
	return result, nil
}

func baselineIdentity(candidate BaselineCandidate) string {
	return strings.Join([]string{candidate.ContractID, candidate.ScreenshotName, candidate.Route, candidate.Viewport}, "\x00")
}

func baselineFilesOnly(files []string) bool {
	for _, file := range files {
		normalized := filepath.ToSlash(strings.TrimPrefix(file, "./"))
		if !strings.HasPrefix(normalized, "visual-hive.baselines/") || !strings.HasSuffix(strings.ToLower(normalized), ".png") {
			return false
		}
		if _, _, err := safeBaselinePath(normalized); err != nil {
			return false
		}
	}
	return len(files) > 0
}

func verifyCandidateFile(candidate BaselineCandidate) error {
	info, err := os.Lstat(candidate.ActualPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != candidate.Bytes || info.Size() <= 8 || info.Size() > maxBaselineCandidateBytes {
		return fmt.Errorf("baseline candidate %s changed after inspection", candidate.ActualPath)
	}
	data, err := os.ReadFile(candidate.ActualPath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != candidate.SHA256 {
		return fmt.Errorf("baseline candidate %s digest changed after inspection", candidate.ActualPath)
	}
	return nil
}

func baselineProposalBody(marker string, finding visualhive.FindingLifecycle, attempt Attempt, review *BaselineReview) string {
	rows := make([]string, 0, len(review.Candidates))
	for _, candidate := range review.Candidates {
		rows = append(rows, fmt.Sprintf("- `%s` — %s/%s, platform `%s`, SHA-256 `%s`", candidate.BaselinePath, candidate.ContractID, candidate.ScreenshotName, candidate.Platform, candidate.SHA256))
	}
	return fmt.Sprintf(`%s

This draft contains only proposed Visual Hive baseline images for the config repair in %s.

Refs #%d

## Required review

Inspect every image below at full resolution. If each image represents the intended UI, mark this PR ready and merge it manually. If any image is wrong, reject or replace the proposal. Hive cannot auto-merge this PR and will keep the original repair on hold.

%s

- Repair PR: %s
- Exact repair head: `+"`%s`"+`
- Candidate source run: %s
- Source artifact ID: `+"`%d`"+`

Merging this proposal records human approval of these exact image digests. It does not resolve the issue; Hive will resync the original repair, rerun all checks, and wait for authoritative target-branch verification.`,
		marker, attempt.PRURL, finding.IssueNumber, strings.Join(rows, "\n"), attempt.PRURL, review.RepairHeadSHA, review.SourceRunURL, review.SourceArtifactID)
}

func nonEmptyLines(value string) []string {
	result := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			result = append(result, filepath.ToSlash(trimmed))
		}
	}
	sort.Strings(result)
	return result
}

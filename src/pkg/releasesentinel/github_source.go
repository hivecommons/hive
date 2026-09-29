package releasesentinel

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	// githubPerPage is the page size for every list call (GitHub's maximum).
	githubPerPage = 100
	// maxTagPages bounds the tag scan. Tags come back newest-first in
	// practice; three pages (300 tags) is far more than any release line
	// needs to find its highest version.
	maxTagPages = 3
	// maxRunPages bounds the workflow-run scan for one commit.
	maxRunPages = 3
	// maxAnnotationsPerJob bounds the evidence read from one failed job.
	maxAnnotationsPerJob = 20
	// maxEvidenceRunes bounds one evidence line.
	maxEvidenceRunes = 300
	// annotationLevelFailure is the check-run annotation level ::error lines
	// are recorded at.
	annotationLevelFailure = "failure"
	// maxWorkflowPages bounds the workflow listing used to resolve release
	// workflow names.
	maxWorkflowPages = 2
	// latestRunPageSize is the page size when only a workflow's newest
	// completed run is wanted.
	latestRunPageSize = 1
	// prStateClosed, prSortUpdated and prDirectionDesc select recently
	// closed PRs, newest first, when looking for a merged fix PR. One page
	// is enough: the fix merged after its round started, and rounds are
	// bounded to hours.
	prStateClosed   = "closed"
	prSortUpdated   = "updated"
	prDirectionDesc = "desc"
)

// releaseTagPattern matches the tags tagged-release.yml cuts: v<MAJOR>.<MINOR>.<PATCH>
// with no pre-release or build suffix.
var releaseTagPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// semver is a parsed release tag.
type semver [3]int

func parseReleaseTag(name string) (semver, bool) {
	m := releaseTagPattern.FindStringSubmatch(name)
	if m == nil {
		return semver{}, false
	}
	var v semver
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return semver{}, false
		}
		v[i] = n
	}
	return v, true
}

func (a semver) less(b semver) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// GitHubSource implements Source over the GitHub REST API. It only reads.
type GitHubSource struct {
	client *gh.Client
	owner  string
	repo   string
}

// NewGitHubSource returns a Source for owner/repo.
func NewGitHubSource(client *gh.Client, owner, repo string) *GitHubSource {
	return &GitHubSource{client: client, owner: owner, repo: repo}
}

// SplitRepo splits "owner/name" into its halves.
func SplitRepo(full string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(strings.TrimSpace(full), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}

// CurrentTag returns the highest release tag.
func (s *GitHubSource) CurrentTag(ctx context.Context) (Tag, bool, error) {
	var best Tag
	var bestV semver
	found := false
	opts := &gh.ListOptions{PerPage: githubPerPage}
	for page := 0; page < maxTagPages; page++ {
		tags, resp, err := s.client.Repositories.ListTags(ctx, s.owner, s.repo, opts)
		if err != nil {
			return Tag{}, false, err
		}
		for _, t := range tags {
			v, ok := parseReleaseTag(t.GetName())
			if !ok || t.GetCommit().GetSHA() == "" {
				continue
			}
			if !found || bestV.less(v) {
				best, bestV, found = Tag{Name: t.GetName(), SHA: t.GetCommit().GetSHA()}, v, true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return best, found, nil
}

type runKey struct {
	workflowID int64
	event      string
}

// Runs lists the latest run per (workflow, event) for sha. A re-run of the
// same run keeps its ID and reports its latest attempt, and a second run of
// the same workflow on the same commit (a re-dispatch) supersedes the first,
// so only the newest one per workflow speaks for it.
func (s *GitHubSource) Runs(ctx context.Context, sha string) ([]Run, error) {
	latest := map[runKey]*gh.WorkflowRun{}
	var order []runKey
	opts := &gh.ListWorkflowRunsOptions{HeadSHA: sha, ListOptions: gh.ListOptions{PerPage: githubPerPage}}
	for page := 0; page < maxRunPages; page++ {
		runs, resp, err := s.client.Actions.ListRepositoryWorkflowRuns(ctx, s.owner, s.repo, opts)
		if err != nil {
			return nil, err
		}
		if runs != nil {
			for _, r := range runs.WorkflowRuns {
				if r == nil {
					continue
				}
				k := runKey{workflowID: r.GetWorkflowID(), event: r.GetEvent()}
				prev, seen := latest[k]
				if !seen {
					order = append(order, k)
				}
				if !seen || r.GetID() > prev.GetID() {
					latest[k] = r
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	out := make([]Run, 0, len(order))
	for _, k := range order {
		r := latest[k]
		out = append(out, Run{
			ID:         r.GetID(),
			Name:       r.GetName(),
			HeadSHA:    r.GetHeadSHA(),
			Status:     r.GetStatus(),
			Conclusion: r.GetConclusion(),
			URL:        r.GetHTMLURL(),
		})
	}
	return out, nil
}

// Details reads the failed jobs of a run's latest attempt, the step each one
// failed at, and the failure annotations (::error lines) GitHub recorded.
func (s *GitHubSource) Details(ctx context.Context, runID int64) (RunDetails, error) {
	jobs, _, err := s.client.Actions.ListWorkflowJobs(ctx, s.owner, s.repo, runID, &gh.ListWorkflowJobsOptions{
		Filter:      "latest",
		ListOptions: gh.ListOptions{PerPage: githubPerPage},
	})
	if err != nil {
		return RunDetails{}, err
	}
	var d RunDetails
	if jobs == nil {
		return d, nil
	}
	d.JobCount = jobs.GetTotalCount()
	if d.JobCount < len(jobs.Jobs) {
		d.JobCount = len(jobs.Jobs)
	}
	for _, j := range jobs.Jobs {
		if j == nil || !IsBlockingConclusion(j.GetConclusion()) {
			continue
		}
		label := j.GetName()
		for _, st := range j.Steps {
			if st != nil && IsBlockingConclusion(st.GetConclusion()) {
				label = fmt.Sprintf("%s / %s", j.GetName(), st.GetName())
				break
			}
		}
		d.FailedJobs = append(d.FailedJobs, label)
		// A job's ID is its check-run ID, which is where GitHub files the
		// annotations. Missing annotations are not fatal: the failed step
		// name alone still tells the agent where to look.
		anns, _, aerr := s.client.Checks.ListCheckRunAnnotations(ctx, s.owner, s.repo, j.GetID(), &gh.ListOptions{PerPage: maxAnnotationsPerJob})
		if aerr != nil {
			continue
		}
		for _, a := range anns {
			if a == nil || a.GetAnnotationLevel() != annotationLevelFailure {
				continue
			}
			if msg := truncateRunes(strings.TrimSpace(a.GetMessage()), maxEvidenceRunes); msg != "" {
				d.Evidence = append(d.Evidence, msg)
			}
		}
	}
	return d, nil
}

// Release looks up the GitHub Release for tag. A 404 is "no release yet".
func (s *GitHubSource) Release(ctx context.Context, tag string) (ReleaseInfo, error) {
	rel, resp, err := s.client.Repositories.GetReleaseByTag(ctx, s.owner, s.repo, tag)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return ReleaseInfo{}, nil
		}
		return ReleaseInfo{}, err
	}
	return ReleaseInfo{Exists: rel != nil, Draft: rel.GetDraft(), URL: rel.GetHTMLURL()}, nil
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// DefaultBranch returns the repository's default branch.
func (s *GitHubSource) DefaultBranch(ctx context.Context) (string, error) {
	repo, _, err := s.client.Repositories.Get(ctx, s.owner, s.repo)
	if err != nil {
		return "", err
	}
	return repo.GetDefaultBranch(), nil
}

// MergedFixPR finds the most recently merged PR into branch that carries
// FixMarker(tag) and merged at or after since, with its commits.
func (s *GitHubSource) MergedFixPR(ctx context.Context, branch, tag string, since time.Time) (FixPR, bool, error) {
	prs, _, err := s.client.PullRequests.List(ctx, s.owner, s.repo, &gh.PullRequestListOptions{
		State:       prStateClosed,
		Base:        branch,
		Sort:        prSortUpdated,
		Direction:   prDirectionDesc,
		ListOptions: gh.ListOptions{PerPage: githubPerPage},
	})
	if err != nil {
		return FixPR{}, false, err
	}
	var best *gh.PullRequest
	for _, pr := range prs {
		if pr == nil || pr.MergedAt == nil || pr.GetMergeCommitSHA() == "" || pr.GetBase().GetRef() != branch {
			continue
		}
		merged := pr.GetMergedAt().Time
		if merged.Before(since) || !HasFixMarker(pr.GetBody(), tag) {
			continue
		}
		if best == nil || merged.After(best.GetMergedAt().Time) {
			best = pr
		}
	}
	if best == nil {
		return FixPR{}, false, nil
	}
	commits, _, err := s.client.PullRequests.ListCommits(ctx, s.owner, s.repo, best.GetNumber(), &gh.ListOptions{PerPage: githubPerPage})
	if err != nil {
		return FixPR{}, false, err
	}
	fix := FixPR{
		Number:   best.GetNumber(),
		URL:      best.GetHTMLURL(),
		MergeSHA: best.GetMergeCommitSHA(),
		MergedAt: best.GetMergedAt().Time,
	}
	for _, c := range commits {
		if sha := c.GetSHA(); sha != "" {
			fix.Commits = append(fix.Commits, sha)
		}
	}
	return fix, true, nil
}

// LatestReleaseWorkflowRuns returns the newest completed run of each
// workflow whose name or file name is in names. Names that match no
// workflow are skipped.
func (s *GitHubSource) LatestReleaseWorkflowRuns(ctx context.Context, names []string) ([]Run, error) {
	want := map[string]bool{}
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			want[n] = true
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	var matched []*gh.Workflow
	opts := &gh.ListOptions{PerPage: githubPerPage}
	for page := 0; page < maxWorkflowPages; page++ {
		wfs, resp, err := s.client.Actions.ListWorkflows(ctx, s.owner, s.repo, opts)
		if err != nil {
			return nil, err
		}
		if wfs != nil {
			for _, w := range wfs.Workflows {
				if w != nil && (want[strings.ToLower(w.GetName())] || want[strings.ToLower(path.Base(w.GetPath()))]) {
					matched = append(matched, w)
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	var out []Run
	for _, w := range matched {
		runs, _, err := s.client.Actions.ListWorkflowRunsByID(ctx, s.owner, s.repo, w.GetID(), &gh.ListWorkflowRunsOptions{
			Status:      statusCompleted,
			ListOptions: gh.ListOptions{PerPage: latestRunPageSize},
		})
		if err != nil {
			return nil, err
		}
		if runs == nil || len(runs.WorkflowRuns) == 0 || runs.WorkflowRuns[0] == nil {
			continue
		}
		r := runs.WorkflowRuns[0]
		out = append(out, Run{
			ID:         r.GetID(),
			Name:       w.GetName(),
			HeadSHA:    r.GetHeadSHA(),
			Status:     r.GetStatus(),
			Conclusion: r.GetConclusion(),
			URL:        r.GetHTMLURL(),
		})
	}
	return out, nil
}

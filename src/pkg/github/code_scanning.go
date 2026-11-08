package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/codescanning"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/issueshape"
	"github.com/hivecommons/hive/pkg/writeback"
)

// CodeScanningLabel marks the issues the code scanning sweep files. It is
// how the sweep lists its own issues (with the hidden marker and the App
// author); it is not a gate.
const CodeScanningLabel = "hive/code-scanning"

// codeScanningPerPage is the alert page size (GitHub's maximum); a var so a
// test can page a small fixture.
var codeScanningPerPage = 100

// codeScanningIssuePages bounds the listing of the sweep's own issues
// (open and closed, 100 a page).
const codeScanningIssuePages = 5

// codeScanningNoAccessBackoff is how long a repo whose alerts GitHub
// refused (missing App permission, or no analysis) is left alone before the
// sweep asks again.
const codeScanningNoAccessBackoff = 6 * time.Hour

// codeScanningMaxFileBytes bounds a file read for an excerpt or the fork
// check.
const codeScanningMaxFileBytes = 512 << 10

type codeScanningState struct {
	mu      sync.Mutex
	enabled bool
	status  map[string]codescanning.Status
	backoff map[string]time.Time
	// logged is the notice kind last logged per repo, so a refusal is
	// logged once rather than every pass.
	logged map[string]string
}

// CodeScanningSweepResult summarises one pass over every enabled repo.
type CodeScanningSweepResult struct {
	Repos   int
	Filed   int
	Updated int
	Closed  int
	Errors  int
	// BudgetSkipped is true when the GitHub API budget was not normal, so
	// the pass made no calls.
	BudgetSkipped bool
}

// CodeScanningStatusBlock is /api/status code_scanning.
type CodeScanningStatusBlock struct {
	// Enabled is true when the sweep runs on at least one repo.
	Enabled bool                  `json:"enabled"`
	Repos   []codescanning.Status `json:"repos"`
	// Notices are the operator-facing problems, one per repo (a missing
	// App permission, code scanning not set up).
	Notices []string `json:"notices"`
}

// CodeScanningStatus returns the last pass's per-repo picture.
func (c *Client) CodeScanningStatus() CodeScanningStatusBlock {
	out := CodeScanningStatusBlock{Repos: []codescanning.Status{}, Notices: []string{}}
	if c == nil {
		return out
	}
	s := &c.codeScanning
	s.mu.Lock()
	defer s.mu.Unlock()
	out.Enabled = s.enabled
	for _, st := range s.status {
		cp := st
		cp.BySeverity = make(map[string]int, len(st.BySeverity))
		for k, v := range st.BySeverity {
			cp.BySeverity[k] = v
		}
		out.Repos = append(out.Repos, cp)
	}
	sort.Slice(out.Repos, func(i, j int) bool { return out.Repos[i].Repo < out.Repos[j].Repo })
	for _, st := range out.Repos {
		if st.Notice != "" {
			out.Notices = append(out.Notices, st.Notice)
		}
	}
	return out
}

// RunCodeScanningSweep makes one pass of the opt-in code scanning sweep
// (code_scanning in hive.yaml, src/docs/code-scanning.md) over every repo
// it is enabled on. It makes no GitHub call while the API budget is not
// normal, and leaves a repo whose alerts GitHub refused alone for
// codeScanningNoAccessBackoff, keeping the notice on the dashboard.
func (c *Client) RunCodeScanningSweep(ctx context.Context, cfg *config.Config, now time.Time) CodeScanningSweepResult {
	var res CodeScanningSweepResult
	if c == nil || c.client == nil || cfg == nil {
		return res
	}
	var repos []string
	for _, r := range c.activeRepos() {
		if _, on := cfg.CodeScanning.For(cfg.Project.Org, r); on {
			repos = append(repos, r)
		}
	}
	s := &c.codeScanning
	s.mu.Lock()
	s.enabled = len(repos) > 0
	if s.status == nil {
		s.status, s.backoff, s.logged = map[string]codescanning.Status{}, map[string]time.Time{}, map[string]string{}
	}
	keep := map[string]bool{}
	for _, r := range repos {
		keep[c.fullRepoName(r)] = true
	}
	for r := range s.status {
		if !keep[r] {
			delete(s.status, r)
		}
	}
	s.mu.Unlock()
	f := codeScanningForge{c: c}
	for _, repo := range repos {
		if ctx.Err() != nil {
			return res
		}
		if mode, _ := c.APIBudgetMode(); mode != APIBudgetNormal {
			res.BudgetSkipped = true
			return res
		}
		full := c.fullRepoName(repo)
		s.mu.Lock()
		until := s.backoff[full]
		s.mu.Unlock()
		if now.Before(until) {
			continue
		}
		policy, _ := cfg.CodeScanning.For(cfg.Project.Org, repo)
		lane := cfg.WorkflowsCapableLane(repo)
		st, err := codescanning.Run(ctx, f, codescanning.Options{
			Repo:               full,
			Policy:             policy,
			Labels:             []string{config.OriginLabel(config.OriginSourceGovernor), CodeScanningLabel},
			WorkflowsLane:      lane,
			WorkflowsLaneLabel: WorkflowsLaneLabel,
			Now:                now,
		})
		res.Repos++
		res.Filed += st.Filed
		res.Updated += st.Updated
		res.Closed += st.Closed
		if err != nil && st.NoticeKind == "" {
			res.Errors++
			st.Error = err.Error()
			if c.logger != nil {
				c.logger.Warn("code scanning sweep: repo pass failed", "repo", full, "filed", st.Filed, "error", err)
			}
		}
		s.mu.Lock()
		if st.NoticeKind != "" {
			s.backoff[full] = now.Add(codeScanningNoAccessBackoff)
			if s.logged[full] != st.NoticeKind && c.logger != nil {
				c.logger.Warn("code scanning sweep: cannot read alerts; backing off", "repo", full, "notice", st.NoticeKind, "retry_after", codeScanningNoAccessBackoff, "detail", st.Notice)
			}
		} else {
			delete(s.backoff, full)
		}
		s.logged[full] = st.NoticeKind
		s.status[full] = st
		s.mu.Unlock()
		if c.logger != nil && (st.Filed > 0 || st.Updated > 0 || st.Closed > 0) {
			c.logger.Info("code scanning sweep", "repo", full, "open_alerts", st.OpenAlerts, "qualifying", st.Qualifying,
				"filed", st.Filed, "updated", st.Updated, "closed", st.Closed, "untracked", st.Untracked, "truncated", st.Truncated)
		}
	}
	return res
}

func (c *Client) fullRepoName(repo string) string {
	owner, name := c.splitRepo(repo)
	return owner + "/" + name
}

// codeScanningForge is codescanning.Forge over the App client. Reads go
// through the shared ETag cache (stable URLs, no since= filter); every write
// asks the write-back gate as an issue_file write on the hive's own issue.
type codeScanningForge struct{ c *Client }

func (f codeScanningForge) ListAlerts(ctx context.Context, repo, state string, maxPages int) ([]codescanning.Alert, bool, error) {
	owner, name := f.c.splitRepo(repo)
	var out []codescanning.Alert
	for page := 1; page <= maxPages; page++ {
		u := fmt.Sprintf("repos/%s/%s/code-scanning/alerts?state=%s&per_page=%d&page=%d", owner, name, state, codeScanningPerPage, page)
		req, err := f.c.client.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, false, err
		}
		var batch []codescanning.Alert
		resp, err := f.c.client.Do(ctx, req, &batch)
		if err != nil {
			return nil, false, classifyCodeScanningError(err)
		}
		out = append(out, batch...)
		if resp == nil || resp.NextPage == 0 {
			return out, true, nil
		}
	}
	return out, false, nil
}

// classifyCodeScanningError maps GitHub's refusals onto the notice errors.
// "Advanced Security must be enabled" (a private repo without it) and "no
// analysis found" mean nothing to read; any other 403 or 404 means the App
// cannot read alerts (no security_events grant, or no access to the repo).
func classifyCodeScanningError(err error) error {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return err
	}
	msg := strings.ToLower(ghErr.Message)
	switch ghErr.Response.StatusCode {
	case http.StatusForbidden, http.StatusNotFound:
		if strings.Contains(msg, "no analysis found") || strings.Contains(msg, "must be enabled") || strings.Contains(msg, "not enabled") {
			return fmt.Errorf("%w: %s", codescanning.ErrNotEnabled, ghErr.Message)
		}
		return fmt.Errorf("%w: %d %s", codescanning.ErrNoAccess, ghErr.Response.StatusCode, ghErr.Message)
	}
	return err
}

func (f codeScanningForge) ListIssues(ctx context.Context, repo string) ([]codescanning.TrackedIssue, error) {
	owner, name := f.c.splitRepo(repo)
	own := f.c.knowsOwnAccounts()
	opts := &gh.IssueListByRepoOptions{State: "all", Labels: []string{CodeScanningLabel}, Sort: "created", Direction: "asc", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []codescanning.TrackedIssue
	for page := 0; page < codeScanningIssuePages; page++ {
		issues, resp, err := f.c.client.Issues.ListByRepo(ctx, owner, name, opts)
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if is == nil || is.IsPullRequest() {
				continue
			}
			// Only the hive's own issues count, so a squatted marker
			// cannot suppress a filing.
			if own && !f.c.isHiveAccount(is.GetUser()) && !f.c.IsHiveAuthorLogin(safeGetLogin(is.GetUser())) {
				continue
			}
			m, ok := codescanning.ParseMarker(is.GetBody())
			if !ok {
				continue
			}
			out = append(out, codescanning.TrackedIssue{Number: is.GetNumber(), Open: is.GetState() != "closed", Body: is.GetBody(), Marker: m})
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
	// Past the bound the dedupe state would be partial: file nothing.
	return nil, fmt.Errorf("more than %d pages of %s issues in %s", codeScanningIssuePages, CodeScanningLabel, repo)
}

// prepare runs the checks every hive-authored text gets before it is
// posted: the canary scan, log scrubbing and @-mention neutralising (alert
// messages are analyser text and may quote handles).
func (f codeScanningForge) prepare(repo, text string) (string, error) {
	if leak, ok := f.c.scanCanaryText(text, "hive-code-scanning:"+repo); ok && f.c.canaryFailClosed {
		return "", fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
	}
	return logscrub.ScrubString(advisory.NeutralizeMentions(text)), nil
}

func (f codeScanningForge) gate(repo string, number int, preview string) error {
	if d := f.c.WritebackAllowed(repo, writeback.Item{Number: number, HiveAuthored: true}, writeback.KindIssueFile, preview); !d.Allowed {
		return fmt.Errorf("code scanning %s: %w: %s %s", repo, writeback.ErrSuppressed, d.Reason, d.Detail)
	}
	return nil
}

func (f codeScanningForge) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (int, error) {
	title = issueshape.AgentIssueTitle(config.OriginSourceGovernor, title)
	body, err := f.prepare(repo, title+"\n"+body)
	if err != nil {
		return 0, err
	}
	title, body, _ = strings.Cut(body, "\n")
	if err := f.gate(repo, 0, title); err != nil {
		return 0, err
	}
	for _, l := range labels {
		if err := f.c.EnsureIssueLabel(ctx, repo, l, codeScanningLabelColor(l), codeScanningLabelDescription(l)); err != nil {
			return 0, fmt.Errorf("ensuring label %s: %w", l, err)
		}
	}
	owner, name := f.c.splitRepo(repo)
	ls := append([]string(nil), labels...)
	created, resp, err := f.c.client.Issues.Create(ctx, owner, name, &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Labels: &ls}) // writeback:gated — WritebackAllowed consulted above
	f.c.noteWritebackOutcome(repo, resp, err)
	if err != nil {
		return 0, err
	}
	return created.GetNumber(), nil
}

func codeScanningLabelColor(label string) string {
	if label == CodeScanningLabel {
		return "b60205"
	}
	return ""
}

func codeScanningLabelDescription(label string) string {
	if label == CodeScanningLabel {
		return "Filed by the hive from GitHub code scanning alerts"
	}
	return ""
}

func (f codeScanningForge) EditIssueBody(ctx context.Context, repo string, number int, body string) error {
	body, err := f.prepare(repo, body)
	if err != nil {
		return err
	}
	if err := f.gate(repo, number, "code scanning alert table"); err != nil {
		return err
	}
	owner, name := f.c.splitRepo(repo)
	_, resp, err := f.c.client.Issues.Edit(ctx, owner, name, number, &gh.IssueRequest{Body: gh.Ptr(body)}) // writeback:gated — WritebackAllowed consulted above
	f.c.noteWritebackOutcome(repo, resp, err)
	return err
}

func (f codeScanningForge) CloseIssue(ctx context.Context, repo string, number int, comment, stateReason string) error {
	comment, err := f.prepare(repo, comment)
	if err != nil {
		return err
	}
	if err := f.gate(repo, number, comment); err != nil {
		return err
	}
	// The closing note carries codescanning.CloseMarker: when an earlier
	// pass posted it and the close itself failed, close without a second note.
	owner, name := f.c.splitRepo(repo)
	if _, posted, err := f.c.findOwnMarkerComment(ctx, owner, name, number, codescanning.CloseMarker); err != nil {
		return err
	} else if !posted {
		if err := f.c.CreateIssueComment(ctx, repo, number, comment); err != nil { // writeback:gated — WritebackAllowed consulted above
			return err
		}
	}
	return f.c.CloseIssue(ctx, repo, number, IssueCloseOptions{StateReason: stateReason}) // writeback:gated — WritebackAllowed consulted above
}

func (f codeScanningForge) FileAt(ctx context.Context, repo, path, ref string) (string, bool, error) {
	owner, name := f.c.splitRepo(repo)
	var opts *gh.RepositoryContentGetOptions
	if ref != "" {
		opts = &gh.RepositoryContentGetOptions{Ref: ref}
	}
	file, _, _, err := f.c.client.Repositories.GetContents(ctx, owner, name, path, opts)
	if err != nil {
		if githubStatusError(err, http.StatusNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	if file == nil || file.GetSize() > codeScanningMaxFileBytes {
		return "", false, nil
	}
	content, err := file.GetContent()
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

func (f codeScanningForge) Upstream(ctx context.Context, repo string) (string, error) {
	owner, name := f.c.splitRepo(repo)
	r, _, err := f.c.client.Repositories.Get(ctx, owner, name)
	if err != nil {
		return "", err
	}
	if !r.GetFork() || r.GetParent() == nil {
		return "", nil
	}
	return r.GetParent().GetFullName(), nil
}

func (f codeScanningForge) LastCommit(ctx context.Context, repo, path string) (string, error) {
	owner, name := f.c.splitRepo(repo)
	commits, _, err := f.c.client.Repositories.ListCommits(ctx, owner, name, &gh.CommitsListOptions{Path: path, ListOptions: gh.ListOptions{PerPage: 1}})
	if err != nil || len(commits) == 0 {
		return "", err
	}
	return commits[0].GetSHA(), nil
}

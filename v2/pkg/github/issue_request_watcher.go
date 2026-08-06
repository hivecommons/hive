package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	gh "github.com/google/go-github/v72/github"
)

// IssueRequestDir is the UID-owned handoff between ordinary agents and Hive's
// sole GitHub lifecycle writer. Direct agent POST /issues calls are denied by
// the proxy, so every issue creation reaches this duplicate guard.
const IssueRequestDir = "/var/run/hive-metrics/issue-requests"

var issueRequestDirForTest string

func issueRequestDir() string {
	if issueRequestDirForTest != "" {
		return issueRequestDirForTest
	}
	return IssueRequestDir
}

const issueRequestPollInterval = 10 * time.Second

type IssueRequest struct {
	Repo   string   `json:"repo"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
	Agent  string   `json:"agent"`
}

type IssueResponse struct {
	OK                         bool   `json:"ok"`
	Number                     int    `json:"number,omitempty"`
	URL                        string `json:"url,omitempty"`
	AlreadyExisted             bool   `json:"already_existed,omitempty"`
	DeduplicatedAgainstManaged bool   `json:"deduplicated_against_managed,omitempty"`
	Error                      string `json:"error,omitempty"`
	At                         string `json:"at"`
}

type IssueRequestAuthorizer func(agent string, fileUID int) error

func (c *Client) StartIssueRequestWatcher(ctx context.Context, authz IssueRequestAuthorizer, nowFn func() time.Time) {
	if c == nil {
		return
	}
	c.issueAuthz = authz
	if nowFn == nil {
		nowFn = time.Now
	}
	if err := os.MkdirAll(issueRequestDir(), 0o777); err != nil {
		c.logger.Warn("issue-request watcher: cannot create request dir; disabled",
			slog.String("dir", issueRequestDir()), slog.String("error", err.Error()))
		return
	}
	if err := os.Chmod(issueRequestDir(), 0o2775); err != nil {
		c.logger.Warn("issue-request watcher: could not set group-writable perms",
			slog.String("dir", issueRequestDir()), slog.String("error", err.Error()))
	}
	go func() {
		t := time.NewTicker(issueRequestPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.processIssueRequests(ctx, nowFn)
			}
		}
	}()
	c.logger.Info("issue-request watcher started", slog.String("dir", issueRequestDir()))
}

func (c *Client) ProcessIssueRequestsOnce(ctx context.Context) {
	if c != nil {
		c.processIssueRequests(ctx, time.Now)
	}
}

func (c *Client) processIssueRequests(ctx context.Context, nowFn func() time.Time) {
	entries, err := os.ReadDir(issueRequestDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		c.handleOneIssueRequest(ctx, filepath.Join(issueRequestDir(), name), nowFn)
	}
}

func (c *Client) handleOneIssueRequest(ctx context.Context, path string, nowFn func() time.Time) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var req IssueRequest
	if err := json.Unmarshal(data, &req); err != nil {
		c.writeIssueResult(path, IssueResponse{Error: "invalid JSON: " + err.Error(), At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		return
	}
	if strings.TrimSpace(req.Repo) == "" || strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Body) == "" || strings.TrimSpace(req.Agent) == "" {
		c.writeIssueResult(path, IssueResponse{Error: "repo, title, body, and agent are required", At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		return
	}
	if !c.managesIssueRequestRepository(req.Repo) {
		c.denyIssueRequest(path, req, "repository is outside this Hive's configured project scope", nowFn)
		return
	}
	fileUID := statUID(data, path)
	if c.issueAuthz == nil {
		c.denyIssueRequest(path, req, "no authorizer configured (fail closed)", nowFn)
		return
	}
	if err := c.issueAuthz(req.Agent, fileUID); err != nil {
		c.denyIssueRequest(path, req, err.Error(), nowFn)
		return
	}

	meta := c.attributionMeta(req.Agent)
	body := req.Body
	if c.attributionTrailerOn() {
		body = AppendTrailer(body, meta)
	}
	marker := agentFindingMarker(req)
	body = marker + "\n\n" + body

	number, url, reused, managed, err := c.createOrReuseAgentIssue(ctx, req, body, marker)
	resp := IssueResponse{At: nowFn().UTC().Format(time.RFC3339)}
	if err != nil {
		resp.Error = err.Error()
		c.writeIssueResult(path, resp)
		c.logger.Warn("issue-request watcher: open failed, will retry",
			slog.String("repo", req.Repo), slog.String("agent", req.Agent), slog.String("error", err.Error()))
		return
	}
	resp.OK = true
	resp.Number = number
	resp.URL = url
	resp.AlreadyExisted = reused
	resp.DeduplicatedAgainstManaged = managed
	c.recordCreationAudit(AuditActionAgentIssueCreated, meta,
		"repo", req.Repo, "number", strconv.Itoa(number), "url", url,
		"reused", strconv.FormatBool(reused), "managed_dedupe", strconv.FormatBool(managed))
	c.writeIssueResult(path, resp)
	_ = os.Remove(path)
	c.logger.Info("issue-request watcher: issue resolved by Hive",
		slog.String("repo", req.Repo), slog.Int("number", number),
		slog.Bool("reused", reused), slog.Bool("managed_dedupe", managed), slog.String("agent", req.Agent))
}

func (c *Client) managesIssueRequestRepository(requested string) bool {
	requestedOwner, requestedRepository, err := splitFullRepository(requested)
	if err != nil {
		return false
	}
	for _, configured := range c.getRepos() {
		owner, repository := c.splitRepo(configured)
		if strings.EqualFold(strings.TrimSpace(owner), requestedOwner) &&
			strings.EqualFold(strings.TrimSpace(repository), requestedRepository) {
			return true
		}
	}
	return false
}

func (c *Client) createOrReuseAgentIssue(ctx context.Context, req IssueRequest, body, marker string) (int, string, bool, bool, error) {
	owner, repo, err := splitFullRepository(req.Repo)
	if err != nil {
		return 0, "", false, false, err
	}
	var open []*gh.Issue
	page := 1
	seenPages := map[int]bool{}
	for {
		if page <= 0 || seenPages[page] {
			return 0, "", false, false, fmt.Errorf("list open issues before create: invalid pagination cycle at page %d", page)
		}
		seenPages[page] = true
		issues, response, err := c.client.Issues.ListByRepo(ctx, owner, repo, &gh.IssueListByRepoOptions{
			State: "open", ListOptions: gh.ListOptions{Page: page, PerPage: 100},
		})
		if err != nil {
			return 0, "", false, false, fmt.Errorf("list open issues before create: %w", err)
		}
		for _, issue := range issues {
			if issue != nil && !issue.IsPullRequest() {
				open = append(open, issue)
			}
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		page = response.NextPage
	}
	for _, issue := range open {
		if strings.Contains(issue.GetBody(), marker) {
			return issue.GetNumber(), issue.GetHTMLURL(), true, false, nil
		}
	}
	for _, issue := range open {
		if isManagedVisualIssue(issue) && sameManagedFinding(req.Title+"\n"+req.Body, issue.GetTitle()+"\n"+issue.GetBody()) {
			return issue.GetNumber(), issue.GetHTMLURL(), true, true, nil
		}
	}
	labels := append([]string(nil), req.Labels...)
	created, _, err := c.client.Issues.Create(ctx, owner, repo, &gh.IssueRequest{
		Title: gh.Ptr(req.Title), Body: gh.Ptr(body), Labels: &labels,
	})
	if err != nil {
		return 0, "", false, false, fmt.Errorf("create agent issue: %w", err)
	}
	return created.GetNumber(), created.GetHTMLURL(), false, false, nil
}

func agentFindingMarker(req IssueRequest) string {
	normalize := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ") }
	// Agent identity is attribution, not finding identity. Omitting it makes an
	// exact retry idempotent even when another authorized specialist observes
	// the same failure before the first request is visible in its work list.
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(req.Repo)) + "\x00" + normalize(req.Title) + "\x00" + normalize(req.Body)))
	return "<!-- hive-agent-finding:" + hex.EncodeToString(sum[:]) + " -->"
}

func isManagedVisualIssue(issue *gh.Issue) bool {
	labels := map[string]bool{}
	for _, label := range issue.Labels {
		labels[strings.ToLower(strings.TrimSpace(label.GetName()))] = true
	}
	return labels["hive/managed"] && labels["visual-hive"]
}

var repoPathPattern = regexp.MustCompile("(?:^|[\\s\\\"'=:(`])((?:\\.\\.?/)?[A-Za-z0-9_.@*()-]+(?:/[A-Za-z0-9_.@*()-]+)+\\.[A-Za-z0-9]+)")
var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func findingPaths(text string) map[string]bool {
	out := map[string]bool{}
	for _, match := range repoPathPattern.FindAllStringSubmatch(text, -1) {
		if len(match) < 2 {
			continue
		}
		path := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(match[1]), "./"), "../"))
		if path != "" && !strings.Contains(path, "://") {
			out[path] = true
		}
	}
	return out
}

var findingStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "bug": true, "defect": true, "finding": true,
	"for": true, "from": true, "hive": true, "in": true, "issue": true, "map": true,
	"of": true, "on": true, "repo": true, "scanner": true, "story": true, "storybook": true,
	"the": true, "to": true, "tsx": true, "ts": true, "visual": true,
}

func findingTerms(text string) map[string]bool {
	text = camelBoundary.ReplaceAllString(text, `${1} ${2}`)
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := map[string]bool{}
	for _, field := range fields {
		if len(field) >= 4 && !findingStopWords[field] {
			out[field] = true
		}
	}
	return out
}

// sameManagedFinding is deliberately conservative: an exact repository path
// must overlap, plus either a second path or two meaningful terms. A mere
// reference to the same source file is not enough to collapse distinct bugs.
func sameManagedFinding(requested, managed string) bool {
	reqPaths, managedPaths := findingPaths(requested), findingPaths(managed)
	overlap := 0
	for path := range reqPaths {
		if managedPaths[path] {
			overlap++
		}
	}
	if overlap == 0 {
		return false
	}
	if overlap >= 2 {
		return true
	}
	reqTerms, managedTerms := findingTerms(requested), findingTerms(managed)
	shared := 0
	for term := range reqTerms {
		if managedTerms[term] {
			shared++
		}
	}
	return shared >= 2
}

func (c *Client) denyIssueRequest(path string, req IssueRequest, reason string, nowFn func() time.Time) {
	c.writeIssueResult(path, IssueResponse{Error: "authorization denied: " + reason, At: nowFn().UTC().Format(time.RFC3339)})
	_ = os.Rename(path, path+".denied")
	c.logger.Warn("issue-request watcher: DENIED (policy)",
		slog.String("agent", req.Agent), slog.String("repo", req.Repo), slog.String("reason", reason))
}

func (c *Client) writeIssueResult(reqPath string, resp IssueResponse) {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	if data, err := json.MarshalIndent(resp, "", "  "); err == nil {
		_ = os.WriteFile(out, data, 0o644)
	}
}

func WriteIssueRequest(dir string, req IssueRequest) (string, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	req.Labels = append([]string(nil), req.Labels...)
	sort.Strings(req.Labels)
	data, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d.json", sanitizeAgentName(req.Agent), time.Now().UnixNano())
	path := filepath.Join(dir, name)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return "", err
	}
	return path, nil
}

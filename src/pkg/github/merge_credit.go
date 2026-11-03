package github

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	gh "github.com/google/go-github/v72/github"
)

// Issue-author credit at merge time (hivecommons/hive#14581).
//
// A PR the hive opens itself already credits the human who filed the issue it
// answers: `Co-authored-by: <issue author>` on its commits (RFC #6588,
// issue-coauthor.sh) and `requested_by=@login` in its `— hive:` trailer
// (#7208). A PR an external clanker opens — a contributor hive, or an agent
// behind the relay — bypasses that choke point, so the person who asked for
// the work lands nowhere in the history (#14552 → #14546, #12927 → #12922).
// The merge is the one place every lane passes through, so the credit is
// applied there: the hive's merge paths ask MergeCreditor for the commit
// message to send with the merge, and GitHub writes the trailer into the
// squash (or merge) commit.
//
// The trailer is additive. It never changes who authored the change — the
// squash is still attributed to the PR author, every Signed-off-by is kept —
// it only puts the issue author on the commit, where GitHub's contributor
// graph counts a Co-authored-by for them.

// mergeCreditCaller labels the REST calls this file makes.
const mergeCreditCaller = "hive:merge_credit"

// mergeCreditMaxIssues bounds the issue lookups one merge performs; a PR
// body closing more issues than this credits the first ones only.
const mergeCreditMaxIssues = 10

// mergeCreditMaxCommitPages bounds the commit read for the squash message
// (GitHub caps the endpoint at 250 commits, three pages of 100).
const mergeCreditMaxCommitPages = 3

// mergeCreditNoreplyDomain is the address domain GitHub resolves a
// co-author by; the id-prefixed form resolves regardless of profile email.
const mergeCreditNoreplyDomain = "users.noreply.github.com"

// coAuthoredByRE matches one Co-authored-by trailer line, capturing the
// name and the address.
var coAuthoredByRE = regexp.MustCompile(`(?mi)^Co-authored-by:\s*(.*?)\s*<([^>]*)>\s*$`)

// IssueAuthorCredit is one human issue author a merge credits.
type IssueAuthorCredit struct {
	Login  string `json:"login"`
	ID     int64  `json:"id,omitempty"`
	Issues []int  `json:"issues,omitempty"`
}

// Email is the id-prefixed GitHub noreply address, the one form guaranteed
// to resolve to the account (issue-coauthor.sh chooses the same).
func (c IssueAuthorCredit) Email() string {
	login := strings.TrimSpace(c.Login)
	if c.ID > 0 {
		return strconv.FormatInt(c.ID, 10) + "+" + login + "@" + mergeCreditNoreplyDomain
	}
	return login + "@" + mergeCreditNoreplyDomain
}

// Trailer renders the Co-authored-by line.
func (c IssueAuthorCredit) Trailer() string {
	return "Co-authored-by: " + strings.TrimSpace(c.Login) + " <" + c.Email() + ">"
}

// ClosingIssueRefs returns the issue numbers body closes in fullRepo
// (owner/repo), in order of first appearance, de-duplicated. It reads the
// same closing keywords ParseClaimedIssues does (the ones GitHub auto-closes
// on, plus their bounded prose form); a reference to another repository is
// left out, the credit is for this repository's history only.
func ClosingIssueRefs(body, fullRepo string) []int {
	fullRepo = strings.TrimSpace(fullRepo)
	seen := map[int]bool{}
	var out []int
	for _, ref := range ParseClaimedIssues(body, fullRepo) {
		if ref.Issue <= 0 || seen[ref.Issue] || !strings.EqualFold(ref.Repo, fullRepo) {
			continue
		}
		seen[ref.Issue] = true
		out = append(out, ref.Issue)
	}
	return out
}

// MergeCredit is what MergeCreditor.Prepare decided for one merge.
type MergeCredit struct {
	// Authors are the human issue authors the PR's closing references name,
	// minus the PR author, de-duplicated. Recorded on the evidence bundle
	// and the audit trail whether or not a trailer was added.
	Authors []IssueAuthorCredit
	// Added are the Authors whose trailer the merge commit message gains;
	// an author the branch commits already credit is left out.
	Added []IssueAuthorCredit
	// CommitMessage is the body to send with the merge, or "" to keep
	// GitHub's default message (nothing to add).
	CommitMessage string
}

// Logins returns the credited logins in order.
func (m MergeCredit) Logins() []string {
	out := make([]string, 0, len(m.Authors))
	for _, a := range m.Authors {
		out = append(out, a.Login)
	}
	return out
}

type issueAuthorEntry struct {
	login string
	id    int64
	human bool
}

// MergeCreditor resolves issue-author credit for the hive's merges. Issue
// authors never change, so a resolved author is cached per owner/repo#n for
// the life of the process: a PR evaluated by several sweeps costs one
// GET /issues/{n} per referenced issue in total.
type MergeCreditor struct {
	gh      *gh.Client
	enabled func(repo string) bool
	logger  *slog.Logger

	mu      sync.Mutex
	authors map[string]issueAuthorEntry
}

// NewMergeCreditor returns a creditor over client. enabled reports the live
// auto_merge.credit_issue_authors setting for an owner/repo; nil means on.
func NewMergeCreditor(client *gh.Client, enabled func(repo string) bool, logger *slog.Logger) *MergeCreditor {
	if logger == nil {
		logger = slog.Default()
	}
	return &MergeCreditor{gh: client, enabled: enabled, logger: logger, authors: map[string]issueAuthorEntry{}}
}

// Enabled reports whether the creditor applies to repo.
func (m *MergeCreditor) Enabled(repo string) bool {
	if m == nil || m.gh == nil {
		return false
	}
	return m.enabled == nil || m.enabled(repo)
}

// Prepare resolves the credit for merging pr with method. It never fails
// the merge: a lookup error, a rebase merge (no message to write) or a PR
// with no human issue author behind it returns an empty credit and the
// merge proceeds with GitHub's default message.
func (m *MergeCreditor) Prepare(ctx context.Context, owner, repo string, pr *gh.PullRequest, method string) MergeCredit {
	full := owner + "/" + repo
	if pr == nil || !m.Enabled(full) {
		return MergeCredit{}
	}
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" {
		method = "squash"
	}
	if method == "rebase" {
		return MergeCredit{}
	}
	refs := ClosingIssueRefs(pr.GetBody(), full)
	if len(refs) == 0 {
		return MergeCredit{}
	}
	if len(refs) > mergeCreditMaxIssues {
		refs = refs[:mergeCreditMaxIssues]
	}
	ctx = WithRESTCaller(ctx, mergeCreditCaller)
	authors := m.resolveAuthors(ctx, owner, repo, pr.GetNumber(), pr.GetUser().GetLogin(), refs)
	if len(authors) == 0 {
		return MergeCredit{}
	}
	credit := MergeCredit{Authors: authors}
	switch method {
	case "merge":
		credit.Added = authors
		credit.CommitMessage = mergeCommitCreditMessage(pr.GetTitle(), authors)
	default:
		commits, err := m.listCommits(ctx, owner, repo, pr.GetNumber())
		if err != nil {
			// Without the branch commits the squash body cannot be rebuilt,
			// and a body without them would drop their Signed-off-by lines:
			// keep GitHub's default message rather than risk the DCO trail.
			m.logger.Warn("merge credit: could not read PR commits, merging without the issue-author trailer",
				slog.String("repo", full), slog.Int("number", pr.GetNumber()), slog.String("error", err.Error()))
			return credit
		}
		credit.Added = uncreditedAuthors(commits, authors)
		if len(credit.Added) > 0 {
			credit.CommitMessage = squashCreditMessage(commits, credit.Added)
		}
	}
	return credit
}

// resolveAuthors maps the referenced issues to their human authors, skipping
// bots, the PR author and pull requests, and folding one person's several
// issues into one credit.
func (m *MergeCreditor) resolveAuthors(ctx context.Context, owner, repo string, number int, prAuthor string, refs []int) []IssueAuthorCredit {
	var (
		out   []IssueAuthorCredit
		index = map[string]int{}
	)
	for _, n := range refs {
		if n == number {
			continue
		}
		entry, ok := m.issueAuthor(ctx, owner, repo, n)
		if !ok || !entry.human {
			continue
		}
		key := strings.ToLower(entry.login)
		if key == "" || key == strings.ToLower(strings.TrimSpace(prAuthor)) {
			continue
		}
		if i, seen := index[key]; seen {
			out[i].Issues = append(out[i].Issues, n)
			continue
		}
		index[key] = len(out)
		out = append(out, IssueAuthorCredit{Login: entry.login, ID: entry.id, Issues: []int{n}})
	}
	return out
}

func (m *MergeCreditor) issueAuthor(ctx context.Context, owner, repo string, n int) (issueAuthorEntry, bool) {
	key := strings.ToLower(owner+"/"+repo) + "#" + strconv.Itoa(n)
	m.mu.Lock()
	entry, ok := m.authors[key]
	m.mu.Unlock()
	if ok {
		return entry, true
	}
	issue, _, err := m.gh.Issues.Get(ctx, owner, repo, n)
	if err != nil {
		m.logger.Debug("merge credit: issue author lookup failed",
			slog.String("repo", owner+"/"+repo), slog.Int("issue", n), slog.String("error", err.Error()))
		return issueAuthorEntry{}, false
	}
	return m.NoteIssue(owner, repo, n, issue), true
}

// NoteIssue records issue n's author from an issue the caller already read
// (the automerge linked-issue gate fetches every closing issue just before
// the merge), so Prepare costs no second GET for it. A nil issue records
// that GitHub reported the number missing, which credits nobody. It returns
// the cached entry; a nil creditor is a no-op.
func (m *MergeCreditor) NoteIssue(owner, repo string, n int, issue *gh.Issue) issueAuthorEntry {
	if m == nil {
		return issueAuthorEntry{}
	}
	key := strings.ToLower(owner+"/"+repo) + "#" + strconv.Itoa(n)
	var entry issueAuthorEntry
	if issue != nil {
		user := issue.GetUser()
		entry = issueAuthorEntry{
			login: strings.TrimSpace(user.GetLogin()),
			id:    user.GetID(),
			human: !issue.IsPullRequest() && isHumanIssueAuthor(user),
		}
	}
	m.mu.Lock()
	m.authors[key] = entry
	m.mu.Unlock()
	return entry
}

// isHumanIssueAuthor applies issue-coauthor.sh's rule: `type` is the
// authoritative signal, the login shapes the belt-and-braces one.
func isHumanIssueAuthor(user *gh.User) bool {
	login := strings.ToLower(strings.TrimSpace(user.GetLogin()))
	if login == "" || strings.EqualFold(user.GetType(), "Bot") {
		return false
	}
	return !strings.HasSuffix(login, "[bot]") && !strings.HasPrefix(login, "github-actions") && !strings.HasSuffix(login, "-bot")
}

func (m *MergeCreditor) listCommits(ctx context.Context, owner, repo string, number int) ([]*gh.RepositoryCommit, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var all []*gh.RepositoryCommit
	for page := 0; page < mergeCreditMaxCommitPages; page++ {
		commits, resp, err := m.gh.PullRequests.ListCommits(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, commits...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("pull request has no commits")
	}
	return all, nil
}

// uncreditedAuthors drops the authors whose Co-authored-by already appears
// on a branch commit (a hive-opened PR carries it from issue-coauthor.sh),
// matched by address or by login, so the squash never names one person
// twice.
func uncreditedAuthors(commits []*gh.RepositoryCommit, authors []IssueAuthorCredit) []IssueAuthorCredit {
	existing := map[string]bool{}
	for _, rc := range commits {
		for _, m := range coAuthoredByRE.FindAllStringSubmatch(rc.GetCommit().GetMessage(), -1) {
			if addr := strings.ToLower(strings.TrimSpace(m[2])); addr != "" {
				existing[addr] = true
				if local, _, ok := strings.Cut(addr, "@"); ok {
					if _, login, hasID := strings.Cut(local, "+"); hasID {
						existing["login:"+login] = true
					} else {
						existing["login:"+local] = true
					}
				}
			}
			if name := strings.ToLower(strings.TrimSpace(m[1])); name != "" {
				existing["login:"+name] = true
			}
		}
	}
	var out []IssueAuthorCredit
	for _, a := range authors {
		if existing[strings.ToLower(a.Email())] || existing["login:"+strings.ToLower(strings.TrimSpace(a.Login))] {
			continue
		}
		out = append(out, a)
	}
	return out
}

// squashCreditMessage rebuilds GitHub's default squash body from the branch
// commits — a single commit's body, or one `* subject` paragraph per commit
// with its body — and appends the credit trailers after it. Merge-from-base
// commits are left out, as GitHub leaves them out. Every line the commits
// carried, Signed-off-by included, is kept where it was; the post-merge DCO
// monitor scans the whole body, not just the final trailer block.
func squashCreditMessage(commits []*gh.RepositoryCommit, added []IssueAuthorCredit) string {
	var messages []string
	for _, rc := range commits {
		if rc == nil || len(rc.Parents) > 1 {
			continue
		}
		if msg := strings.TrimSpace(rc.GetCommit().GetMessage()); msg != "" {
			messages = append(messages, msg)
		}
	}
	var parts []string
	switch len(messages) {
	case 0:
	case 1:
		_, rest, _ := strings.Cut(messages[0], "\n")
		if rest = strings.TrimSpace(rest); rest != "" {
			parts = append(parts, rest)
		}
	default:
		for _, msg := range messages {
			subject, rest, _ := strings.Cut(msg, "\n")
			para := "* " + strings.TrimSpace(subject)
			if rest = strings.TrimSpace(rest); rest != "" {
				para += "\n\n" + rest
			}
			parts = append(parts, para)
		}
	}
	return strings.Join(append(parts, creditTrailers(added)), "\n\n")
}

// mergeCommitCreditMessage is the merge commit body: GitHub's default (the
// PR title) followed by the credit trailers. The branch commits land
// unchanged under a merge commit, so their own trailers need no rebuilding.
func mergeCommitCreditMessage(title string, added []IssueAuthorCredit) string {
	parts := []string{}
	if title = strings.TrimSpace(title); title != "" {
		parts = append(parts, title)
	}
	return strings.Join(append(parts, creditTrailers(added)), "\n\n")
}

func creditTrailers(added []IssueAuthorCredit) string {
	lines := make([]string, 0, len(added))
	for _, a := range added {
		lines = append(lines, a.Trailer())
	}
	return strings.Join(lines, "\n")
}

// mergeCreditLogins renders the credited logins for an audit detail.
func mergeCreditLogins(credit MergeCredit) string {
	logins := credit.Logins()
	sort.Strings(logins)
	return strings.Join(logins, ",")
}

// SetCreditIssueAuthors installs the live auto_merge.credit_issue_authors
// switch (nil means on). Nil-receiver safe like the other setters.
func (c *Client) SetCreditIssueAuthors(fn func(repo string) bool) {
	if c == nil {
		return
	}
	c.mergeCreditMu.Lock()
	defer c.mergeCreditMu.Unlock()
	c.creditIssueAuthors = fn
	if c.mergeCredit != nil {
		c.mergeCredit.enabled = fn
	}
}

// MergeCreditor returns the client's shared creditor (one issue-author cache
// for every merge path), or nil without an API client.
func (c *Client) MergeCreditor() *MergeCreditor {
	if c == nil || c.client == nil {
		return nil
	}
	c.mergeCreditMu.Lock()
	defer c.mergeCreditMu.Unlock()
	if c.mergeCredit == nil {
		c.mergeCredit = NewMergeCreditor(c.client, c.creditIssueAuthors, c.logger)
	}
	return c.mergeCredit
}

// NoteMergeCredit stashes the credit decided for repo#number so the merge's
// pr_merged audit and evidence merge event can name the issue authors. An
// empty credit clears any earlier note.
func (c *Client) NoteMergeCredit(repo string, number int, credit MergeCredit) {
	if c == nil || number <= 0 {
		return
	}
	key := strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
	c.mergeCreditMu.Lock()
	defer c.mergeCreditMu.Unlock()
	if len(credit.Authors) == 0 {
		delete(c.mergeCredits, key)
		return
	}
	if c.mergeCredits == nil {
		c.mergeCredits = map[string]MergeCredit{}
	}
	c.mergeCredits[key] = credit
}

func (c *Client) takeMergeCredit(repo string, number int) MergeCredit {
	if c == nil {
		return MergeCredit{}
	}
	key := strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
	c.mergeCreditMu.Lock()
	defer c.mergeCreditMu.Unlock()
	credit := c.mergeCredits[key]
	delete(c.mergeCredits, key)
	return credit
}

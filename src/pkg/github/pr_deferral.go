package github

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A hive agent's deferral of a pull request (hivecommons/hive#17347). The
// scanner diagnoses a red PR, decides the head can never go green as it
// stands, files the successor issues and posts "Deferring." — a decision
// with an owner and successors. The relay records it as an
// agent_pr_deferred audit entry bound to the head it was posted on, so the
// activity monitor can read "Deferred · waiting on #N" instead of
// "Unassigned" (nobody looked).

// AuditActionAgentPRDeferred is recorded, next to the comment's own
// agent_comment_created entry, when the issue-request relay posts a hive
// agent's comment that defers an open pull request. Detail pairs: the PR
// head the deferral was made on, the successor references it names, whether
// it says a maintainer may close the PR, the comment URL and the reason.
const AuditActionAgentPRDeferred = "agent_pr_deferred"

// Detail pair keys of an agent_pr_deferred audit entry.
const (
	PRDeferralPairHead       = "head"
	PRDeferralPairSuccessors = "successors"
	PRDeferralPairCloseOK    = "close_ok"
	PRDeferralPairURL        = "url"
	PRDeferralPairReason     = "reason"
)

// prDeferralReasonMax bounds the reason carried in the audit detail.
const prDeferralReasonMax = 300

// prDeferralSentence matches a sentence or line that opens with
// "Deferring"/"Deferred" (after markdown and an optional "Status:" or
// "Update:" label), up to its closing punctuation; a dot inside a token
// ("docs/a.md") does not end it. A PR deferral is the conclusion of a
// diagnosis, so it usually closes the comment ("… a maintainer may also
// close it now. Deferring.") rather than opening it, unlike an issue
// deferral (commentDeferral), which only looks at how the comment starts.
var prDeferralSentence = regexp.MustCompile(`(?im)(?:^|[.!?][ \t]+)[^\w\n]*(?:(?:status|update)\s*:[^\w\n]*)?(deferr(?:ed|ing)\b(?:[^.!?\n]|[.!?][^\s.!?])*[.!?]?)`)

// prDeferralCloseOK matches a deferral that says a person may close the PR
// now ("a maintainer may also close it now", "safe to close").
var prDeferralCloseOK = regexp.MustCompile(`(?i)\b(?:maintainers?|owners?|you|a\s+person)\s+(?:may|can|could)\s+(?:also\s+|safely\s+|just\s+)?close\s+(?:it|this|the\s+PR)\b|\bsafe\s+to\s+close\b`)

// prDeferralRef matches an issue or pull-request reference in prose: a
// GitHub issue/pull URL, "owner/repo#N" or a bare "#N" that does not sit
// inside a word, a path or an HTML entity.
var prDeferralRef = regexp.MustCompile(`https?://github\.com/([\w.-]+/[\w.-]+)/(?:pull|issues)/(\d+)\b|(?:^|[\s(\[{,;:'"` + "`" + `*_>])(?:([\w.-]+/[\w.-]+))?#(\d+)\b`)

// prDeferralFence matches a fenced code block; its content is quoted
// material, not the agent's prose.
var prDeferralFence = regexp.MustCompile("(?s)```.*?```")

// prDeferralSpace collapses runs of whitespace in the reason.
var prDeferralSpace = regexp.MustCompile(`\s+`)

// prDeferral is what one deferral comment says.
type prDeferral struct {
	Reason     string
	Successors []string
	CloseOK    bool
}

// parsePRDeferral reports whether body, a hive agent's comment on repo's
// pull request number, defers the PR, and if so the reason, the successor
// references (in order, deduplicated, "#N" within repo, "owner/repo#N"
// across repos, never the PR itself) and whether a maintainer may close
// it. A statement that work starts now overrides the deferral, as on
// issues.
func parsePRDeferral(body, repo string, number int) (prDeferral, bool) {
	note := strings.ReplaceAll(body, "\r\n", "\n")
	note = attributionIdentityFooter.ReplaceAllString(note, "")
	note = strings.TrimSpace(prDeferralFence.ReplaceAllString(note, " "))
	if note == "" || commentSaysStartNow(note) {
		return prDeferral{}, false
	}
	m := prDeferralSentence.FindStringSubmatch(note)
	if m == nil && !commentDeferral.MatchString(note) {
		return prDeferral{}, false
	}
	// A deferral sentence that says why ("Deferring this PR (dependabot-
	// authored, hive does not push to it).") is the reason; a bare
	// "Deferring." concludes the paragraph before it.
	reason := ""
	if m != nil && len(strings.Fields(m[1])) >= 3 {
		reason = m[1]
	} else {
		reason, _, _ = strings.Cut(note, "\n\n")
	}
	return prDeferral{
		Reason:     prDeferralTrim(reason),
		Successors: prDeferralRefs(note, repo, number),
		CloseOK:    prDeferralCloseOK.MatchString(note),
	}, true
}

func prDeferralTrim(s string) string {
	s = strings.TrimSpace(prDeferralSpace.ReplaceAllString(s, " "))
	if utf8.RuneCountInString(s) <= prDeferralReasonMax {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:prDeferralReasonMax-1])) + "…"
}

func prDeferralRefs(note, repo string, number int) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range prDeferralRef.FindAllStringSubmatch(note, -1) {
		refRepo, n := m[1], m[2]
		if n == "" {
			refRepo, n = m[3], m[4]
		}
		num, err := strconv.Atoi(n)
		if err != nil || num <= 0 {
			continue
		}
		ref := "#" + n
		if refRepo != "" && !strings.EqualFold(refRepo, repo) {
			ref = refRepo + "#" + n
		} else if num == number {
			continue
		}
		if key := strings.ToLower(ref); !seen[key] {
			seen[key] = true
			out = append(out, ref)
		}
	}
	return out
}

// prDeferralPairBoundary is a pair boundary (", <key>=") as audit readers
// split it; a reason that contains one would be cut there.
var prDeferralPairBoundary = regexp.MustCompile(`, ([a-z_]+)=`)

// auditPairs renders the agent_pr_deferred detail pairs. The reason goes
// last among them; readers split pairs only at ", <key>=", so any such
// boundary inside the reason is written as "; <key>=" and the reason stays
// one value.
func (d prDeferral) auditPairs(head, url string) []string {
	return []string{
		PRDeferralPairHead, head,
		PRDeferralPairSuccessors, strings.Join(d.Successors, " "),
		PRDeferralPairCloseOK, strconv.FormatBool(d.CloseOK),
		PRDeferralPairURL, url,
		PRDeferralPairReason, prDeferralPairBoundary.ReplaceAllString(d.Reason, "; $1="),
	}
}

// recordPRDeferral audits a relayed comment that defers an open pull
// request, bound to the PR's current head. Best effort: the comment is
// already posted, so a lookup failure only costs the dashboard its
// "Deferred" word. A number that is an issue (404 from the pulls API) or a
// closed PR records nothing.
func (c *Client) recordPRDeferral(ctx context.Context, req IssueRequest, meta InvocationMeta, url string) {
	if c == nil || c.client == nil || req.Number <= 0 {
		return
	}
	owner, name := c.splitRepo(req.Repo)
	d, ok := parsePRDeferral(req.Body, owner+"/"+name, req.Number)
	if !ok {
		return
	}
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, req.Number)
	if err != nil {
		if !githubStatusError(err, http.StatusNotFound) {
			c.logger.Warn("issue-request watcher: deferral comment posted, PR head lookup failed; not recorded",
				slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("error", err.Error()))
		}
		return
	}
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if pr.GetState() != "open" || head == "" {
		return
	}
	c.recordWriteAudit(AuditActionAgentPRDeferred, meta, WriteTarget{Repo: req.Repo, Number: req.Number}, d.auditPairs(head, url)...)
}

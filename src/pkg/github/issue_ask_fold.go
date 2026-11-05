package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// One decision, one owner ask. When an agent parks an issue whose text
// defers to another open decision park ("Answer on #511; the same choice
// applies here", "an answer here or on #511 settles all three"), or that
// cites the same Actions run as one, the relay links the new park "blocked
// by" the earlier issue itself, as `--blocked-by` would: the dashboard then
// shows it waiting on that issue's answer instead of as a second ask
// (Danathar/tududi#511, #512, #528; Danathar/openaudible-autoupdate#173,
// #174, #177 asked one Fedora 45 question four times in seven hours).

// askFoldMaxCandidates bounds the issues one park text may cost a read.
const askFoldMaxCandidates = 4

var (
	askFoldRefRE = regexp.MustCompile(`(?:^|[^\w/#&])#(\d+)\b`)
	askFoldRunRE = regexp.MustCompile(`/actions/runs/(\d+)`)
)

// askDefersTo reports whether text defers its question to issue n in so
// many words: an answer or choice made on #n, the same decision as #n,
// parking with #n, or #n settling it.
func askDefersTo(text string, n int) bool {
	ref := `#` + strconv.Itoa(n) + `\b`
	for _, pattern := range []string{
		`\b(?:answer|reply|respond|pick|choose|decide)\w*\b(?:[^.;\n]|\.\d){0,40}\b(?:on|in|at)\s+` + ref,
		`\bsame\s+(?:choice|decision|question|answer)\b(?:[^.;\n]|\.\d){0,60}` + ref,
		`\bpark\w*\s+(?:this\s+|it\s+)?(?:with|on|behind)\s+` + ref,
		ref + `(?:[^.;\n]|\.\d){0,60}\b(?:settles|decides|answers)\b`,
		`\bdefer(?:s|red)?\s+to\s+` + ref,
	} {
		if regexp.MustCompile(`(?i)` + pattern).MatchString(text) {
			return true
		}
	}
	return false
}

// askRuns returns the Actions run ids text cites.
func askRuns(text string) map[string]bool {
	runs := map[string]bool{}
	for _, m := range askFoldRunRE.FindAllStringSubmatch(text, -1) {
		runs[m[1]] = true
	}
	return runs
}

// askFoldCandidates are the same-repo issue numbers text refers to, in
// order, below self (an issue filed earlier: numbers only grow, so a fold
// never points back at a later ask and two asks cannot wait on each other),
// at most askFoldMaxCandidates. self is 0 for an issue not filed yet.
func askFoldCandidates(text string, self int) []int {
	var out []int
	seen := map[int]bool{}
	for _, m := range askFoldRefRE.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || seen[n] || (self > 0 && n >= self) {
			continue
		}
		seen[n] = true
		out = append(out, n)
		if len(out) == askFoldMaxCandidates {
			break
		}
	}
	return out
}

// askFoldTarget picks the earlier open decision park a new park in repo
// asks again, or 0. A candidate counts when it is an open issue (not a pull
// request) still parked on a decision, and the text defers to it or cites
// an Actions run its body or a hive comment on it also cites. A failed read
// skips the candidate: a missed fold leaves a second ask, as before.
func (c *Client) askFoldTarget(ctx context.Context, repo string, self int, text string) int {
	owner, name := c.splitRepo(repo)
	runs := askRuns(text)
	for _, n := range askFoldCandidates(text, self) {
		issue, _, err := c.client.Issues.Get(ctx, owner, name, n)
		if err != nil || issue.IsPullRequest() || issue.GetState() != "open" {
			continue
		}
		labels := issueLabelNames(issue.Labels)
		if !labelPresent(labels, c.needsDecisionLabel()) && !labelPresent(labels, NeedsDecisionStageLabel) {
			continue
		}
		if askDefersTo(text, n) {
			return n
		}
		if len(runs) == 0 {
			continue
		}
		cited := issue.GetBody()
		if comments, err := c.listIssueComments(ctx, owner, name, n); err == nil {
			for _, cm := range comments {
				if cm != nil && c.isHiveAccount(cm.GetUser()) {
					cited += "\n" + cm.GetBody()
				}
			}
		}
		for run := range askRuns(cited) {
			if runs[run] {
				return n
			}
		}
	}
	return 0
}

// askFoldNote is appended to a park folded into issue n, so the owner reads
// where to answer.
func askFoldNote(n int) string {
	return fmt.Sprintf("_Hive linked this issue as blocked by #%d: it asks the same decision, so answer on #%d and this issue follows it._", n, n)
}

func appendAskFoldNote(body string, n int) string {
	return strings.TrimRight(body, "\n") + "\n\n" + askFoldNote(n)
}

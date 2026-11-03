package evidence

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Change failures (hivecommons/hive#13241). A merged PR is a change failure
// when, within LinkWindow of its merge, a later PR reverted it (reverted_by)
// or a person reopened an issue it closed (issue_reopens). Both are recorded
// on the merged PR's bundle so the Outcome card's change-failure rate
// survives restarts; neither is ever read back into a merge decision.

// IssueReopenSourceClaimLedger names the duplicate-PR claim ledger as the
// observer of an issue reopen: it saw the issues.reopened webhook or the
// issue's timeline after an enumeration listed it with state_reason
// "reopened".
const IssueReopenSourceClaimLedger = "claim-ledger"

// IssueReopen records that an issue the PR closed was reopened after the
// merge. Repo is the issue's repository when it differs from the PR's.
type IssueReopen struct {
	Issue  int       `json:"issue"`
	Repo   string    `json:"repo,omitempty"`
	At     time.Time `json:"at"`
	Source string    `json:"source"`
}

// revertCommit matches git's own revert message ("This reverts commit
// <sha>.") and the shorter "reverts commit <sha>" people write by hand.
var revertCommit = regexp.MustCompile(`(?i)\breverts?\s+commit\s+([0-9a-f]{7,40})\b`)

// revertTitle matches the title git and GitHub give a revert: `Revert "…"`,
// with any emoji or type prefix before it.
var revertTitle = regexp.MustCompile(`(?i)(?:^|[\s:\]\)])revert\s*["“']`)

// revertTitlePR pulls PR numbers out of a `Revert "…"` title: GitHub's
// generated title quotes the squash-merge title, which ends with the original
// PR's number — `Revert "Original title (#N)"` — and people also write
// `Revert "…" (#N)`.
var revertTitlePR = regexp.MustCompile(`\(#(\d+)\)`)

// MinRevertSHALen is the shortest commit prefix a revert body may name for it
// to be matched against a bundle's merge or head SHA.
const MinRevertSHALen = 7

// LooksLikeRevert reports whether a merged PR's title or body claims to
// revert something: a `Revert "…"` title, a "Reverts #N" reference or a
// "This reverts commit <sha>" line. It is the cheap pre-check before any
// bundle is read.
func LooksLikeRevert(title, body string) bool {
	if revertTitle.MatchString(title) {
		return true
	}
	text := title + "\n" + body
	return revertRef.MatchString(text) || revertCommit.MatchString(text)
}

// RevertCommitSHAs returns the lower-cased commit SHAs (or prefixes of at
// least MinRevertSHALen) text names as reverted, in first-mention order.
func RevertCommitSHAs(text string) []string {
	var out []string
	for _, m := range revertCommit.FindAllStringSubmatch(text, -1) {
		sha := strings.ToLower(m[1])
		if len(sha) < MinRevertSHALen || slices.Contains(out, sha) {
			continue
		}
		out = append(out, sha)
	}
	return out
}

// RevertTargets is what a revert PR's title and body name as reverted: PR
// numbers of the same repo (ChangeRefs' revert rules) and commit SHAs.
type RevertTargets struct {
	PRs  []int
	SHAs []string
}

// ParseRevertTargets extracts RevertTargets from a merged PR's title and
// body. Empty when nothing in the text claims a revert.
func ParseRevertTargets(repo string, number int, title, body string) RevertTargets {
	text := title + "\n" + body
	prs, _ := ChangeRefs(repo, number, text)
	if revertTitle.MatchString(title) {
		for _, m := range revertTitlePR.FindAllStringSubmatch(title, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n != number && !slices.Contains(prs, n) {
				prs = append(prs, n)
			}
		}
	}
	return RevertTargets{PRs: prs, SHAs: RevertCommitSHAs(text)}
}

// SHAMatches reports whether sha (a full SHA or a prefix of at least
// MinRevertSHALen) names full.
func SHAMatches(sha, full string) bool {
	sha, full = strings.ToLower(strings.TrimSpace(sha)), strings.ToLower(strings.TrimSpace(full))
	if len(sha) < MinRevertSHALen || full == "" {
		return false
	}
	return strings.HasPrefix(full, sha)
}

// RevertsMergedBundle reports whether b records the merge a revert naming
// sha undid: the SHA is the merge commit or the head the hive merged, and
// the merge lies within LinkWindow before revertedAt.
func RevertsMergedBundle(b *Bundle, sha string, revertedAt time.Time) bool {
	if b == nil || b.Merge == nil || !WithinLinkWindow(b.Merge.At, revertedAt) {
		return false
	}
	return SHAMatches(sha, b.Merge.SHA) || SHAMatches(sha, b.HeadSHA)
}

// ChangeFailure classifies a folded PR for the Outcome card. Reverted is
// true when a reverted_by link exists (the linker only writes one for a
// revert merged within LinkWindow of the original). Reopened is true when an
// issue the PR closed was reopened within LinkWindow of the merge. Neither is
// set for a PR that did not merge.
func (p *PR) ChangeFailure() (reverted, reopened bool) {
	if p == nil || p.Merge == nil {
		return false, false
	}
	reverted = len(p.RevertedBy) > 0
	for _, r := range p.IssueReopens {
		if WithinLinkWindow(p.Merge.At, r.At) {
			reopened = true
			break
		}
	}
	return reverted, reopened
}

// AddIssueReopen appends r to b.IssueReopens unless the same issue is already
// recorded, and reports whether b changed.
func (b *Bundle) AddIssueReopen(r IssueReopen) bool {
	if b == nil || r.Issue <= 0 || r.At.IsZero() {
		return false
	}
	for _, have := range b.IssueReopens {
		if have.Issue == r.Issue && strings.EqualFold(have.Repo, r.Repo) {
			return false
		}
	}
	b.IssueReopens = append(b.IssueReopens, r)
	return true
}

// mergeIssueReopens unions two reopen lists by (repo, issue).
func mergeIssueReopens(have, add []IssueReopen) []IssueReopen {
	for _, r := range add {
		dup := false
		for _, h := range have {
			if h.Issue == r.Issue && strings.EqualFold(h.Repo, r.Repo) {
				dup = true
				break
			}
		}
		if !dup {
			have = append(have, r)
		}
	}
	return have
}

// RevertBundleID is the bundle id a revert PR without a bundle of its own is
// recorded under in reverted_by: owner/repo#number@head, with the merge
// commit standing in when the head is unknown.
func RevertBundleID(repo string, number int, head string) string {
	head = strings.TrimSpace(head)
	if head == "" {
		head = "merged"
	}
	return repo + "#" + strconv.Itoa(number) + "@" + head
}

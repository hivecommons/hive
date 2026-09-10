package github

import (
	"context"
	"regexp"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Rejected-finding re-file gate (#6463).
//
// A scanner running in issues-only mode files an issue per finding, and the
// policy correctly forbids it from listing issues — so the moment a maintainer
// closes one of those issues as not-planned, the rejection leaves the agent's
// field of view entirely. On the next kick the same finding is rediscovered,
// reworded, and filed again. Observed live on a downstream repo: one false
// positive filed five times in four days, each closed with a rebuttal, each
// re-filed within ~24h.
//
// The existing exact-title dedupe in CreateIssue cannot catch this: every one
// of those five filings was worded differently, and findingEvidenceHash
// (pkg/advisory) is deliberately verbatim-only. What DID stay stable across
// all five was the set of files the finding pointed at — the line numbers
// drifted with unrelated commits, the prose was rephrased every time, but the
// file paths were the finding's actual subject and the producer could not
// rephrase those without changing what it was reporting.
//
// So the gate keys on exactly that: before creating an issue, CreateIssue
// looks for a recently closed issue that
//
//   - was filed by this hive's own App bot (an agent filing, by construction —
//     every agent create flows through this same chokepoint), and
//   - was closed as "not_planned" or "duplicate" — a maintainer's explicit
//     rejection, never "completed" (a completed close means fixed, and a
//     re-report after a fix may be a genuine regression), and
//   - references exactly the same set of files as the pending request.
//
// When one exists, the request is refused and the response names the closed
// issue, so the agent (and its transcript) sees the maintainer's rebuttal
// instead of silently re-litigating it. GitHub's own closed issue IS the
// rejection tombstone — there is deliberately no new persistent store, the
// same posture advisory_suppress.go took for digest suppression: the baseline
// is re-read from the forge, so it survives restarts and is visible to the
// humans it protects.
//
// The gate fails toward FILING, in every direction it can:
//
//   - lookup error → file (same posture as the open-title dedupe lookup, and
//     as advisory's ResolveRef: suppression only ever on positive evidence);
//   - either file set empty → file (nothing to key on);
//   - file sets differ at all → file (exact equality, not overlap — a genuine
//     new defect in an already-argued-about file names a different set the
//     moment it involves any other file);
//   - no App-bot identity on this client → file (a token-authenticated hive
//     cannot distinguish its own filings from a human's, and suppressing
//     against a human-authored issue is not this gate's mandate);
//   - rejection older than issueRejectionWindow → file (a maintainer's "no"
//     from months ago should not silence a world that may have changed).

// issueRejectionWindow bounds how long a not-planned closure suppresses
// re-filing. Mirrors advisory's refClosedWindow reasoning: the observed
// re-file loop cycles in ~24h, so 30 days is deep coverage for the failure
// mode, while a rejection from a different era eventually stops gating —
// the codebase, and the maintainer's reasoning, may both have moved on.
const issueRejectionWindow = 30 * 24 * time.Hour

// issueRejectionMaxPages bounds the closed-issue scan, same budget as
// findOpenIssueByTitle: agent-filed issues are recent by construction, and
// the scan additionally early-exits at the window boundary (sorted by
// updated desc, and closed_at ≤ updated_at, so past-window updated means
// past-window closed for everything after it).
const issueRejectionMaxPages = 3

// rejectedStateReasons are the GitHub state_reason values that count as a
// maintainer rejecting a finding. "completed" is deliberately absent — see
// the file comment.
var rejectedStateReasons = map[string]bool{
	"not_planned": true,
	"duplicate":   true,
}

// fileRefTokenPattern matches a file-path-like token: path characters ending
// in a dot-extension that starts with a letter, optionally followed by one or
// more :NN line/column suffixes ("client.py", "pkg/advisory/evidence.go:53",
// "a.go:1:2"). The letter-first extension is what keeps version strings
// ("v4.23.3") and bare numerics out.
var fileRefTokenPattern = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./-]*\.[A-Za-z][A-Za-z0-9_]*(?::[0-9]+)*`)

// urlPattern strips URLs before token extraction so "peps.python.org/pep-0758"
// inside a link never becomes a "file". Bare schemeless domains are handled by
// domainExtensions below.
var urlPattern = regexp.MustCompile(`https?://[^\s)\]>"']+`)

// lineSuffixPattern strips trailing :NN(:NN...) line/column references so
// "client.py:290" and "client.py:321" — the same site renumbered by unrelated
// commits, exactly the drift observed live — normalize to the same key.
var lineSuffixPattern = regexp.MustCompile(`(:[0-9]+)+$`)

// domainExtensions are dot-suffixes that make a token a hostname rather than a
// file. Small on purpose: a miss here only ADDS a token to both sides' sets,
// and the sets must be exactly equal anyway — noise degrades toward filing.
var domainExtensions = map[string]bool{
	"com": true, "org": true, "net": true, "io": true,
	"dev": true, "edu": true, "gov": true,
}

// issueFileRefSet extracts the normalized set of file paths a finding's text
// references.
func issueFileRefSet(text string) map[string]bool {
	text = urlPattern.ReplaceAllString(text, " ")
	out := make(map[string]bool)
	for _, tok := range fileRefTokenPattern.FindAllString(text, -1) {
		tok = lineSuffixPattern.ReplaceAllString(tok, "")
		if dot := strings.LastIndex(tok, "."); dot >= 0 && domainExtensions[strings.ToLower(tok[dot+1:])] {
			continue
		}
		out[tok] = true
	}
	return out
}

// equalFileRefSets reports whether two non-empty file-reference sets are
// exactly equal. Empty on either side never matches: no evidence, no gate.
func equalFileRefSets(a, b map[string]bool) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// findRejectedTwin returns a recently closed, maintainer-rejected, App-bot-
// filed issue whose file-reference set exactly matches the pending request's,
// or nil. An error means the lookup could not tell; the caller files anyway.
func (c *Client) findRejectedTwin(ctx context.Context, owner, repo, title, body string) (*gh.Issue, error) {
	botLogin := strings.TrimSpace(c.appBotLogin)
	if botLogin == "" {
		return nil, nil
	}
	want := issueFileRefSet(title + "\n" + body)
	if len(want) == 0 {
		return nil, nil
	}
	cutoff := time.Now().Add(-issueRejectionWindow)
	opts := &gh.IssueListByRepoOptions{
		State:       "closed",
		Creator:     botLogin,
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for page := 1; page <= issueRejectionMaxPages; page++ {
		opts.ListOptions.Page = page
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if is.GetUpdatedAt().Time.Before(cutoff) {
				// Sorted by updated desc: everything after this is older, and
				// closed_at ≤ updated_at, so nothing further can qualify.
				return nil, nil
			}
			if is.IsPullRequest() {
				continue
			}
			if !rejectedStateReasons[is.GetStateReason()] {
				continue
			}
			if is.GetClosedAt().Time.Before(cutoff) {
				continue
			}
			if equalFileRefSets(want, issueFileRefSet(is.GetTitle()+"\n"+is.GetBody())) {
				return is, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
	}
	return nil, nil
}

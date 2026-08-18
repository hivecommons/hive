package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PRRef identifies a pull request parsed out of a GitHub PR URL.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
}

// FullName returns "owner/repo".
func (r PRRef) FullName() string { return r.Owner + "/" + r.Repo }

// prURLPattern matches the "…/{owner}/{repo}/pull/{number}" tail of a GitHub PR
// URL. It is deliberately host-agnostic (works for github.com AND GHE hosts) and
// anchored on the "/pull/" segment so trailing paths (/files, #discussion) and
// query strings do not defeat it. Only the FIRST such match in the string is
// used by ParsePRURL — the reported URL is expected to be a single PR link.
var prURLPattern = regexp.MustCompile(`([^/\s]+)/([^/\s]+)/pull/(\d+)`)

// ParsePRURL extracts owner, repo and PR number from a GitHub pull-request URL.
// It accepts full URLs (https://github.com/owner/repo/pull/123, GHE hosts, with
// or without trailing path/fragment/query) and is intentionally strict about the
// "/pull/<number>" shape so an arbitrary non-PR URL does not parse. It returns an
// error when no PR reference can be found.
func ParsePRURL(prURL string) (PRRef, error) {
	s := strings.TrimSpace(prURL)
	if s == "" {
		return PRRef{}, fmt.Errorf("empty PR URL")
	}
	m := prURLPattern.FindStringSubmatch(s)
	if m == nil {
		return PRRef{}, fmt.Errorf("not a recognizable GitHub pull request URL: %q", prURL)
	}
	num, err := strconv.Atoi(m[3])
	if err != nil || num <= 0 {
		return PRRef{}, fmt.Errorf("invalid PR number in URL %q", prURL)
	}
	return PRRef{Owner: m[1], Repo: m[2], Number: num}, nil
}

// PRVerification is the result of VerifyReportedPR. Verified is true only when
// the PR exists, its base repo matches the expected repo, and its author matches
// the expected contributor. When Verified is false, Reason explains why (used for
// logging/audit) and Err is set when the failure was a GitHub API error (as
// opposed to a clean "PR does not match" negative).
type PRVerification struct {
	Verified bool
	Reason   string
	Err      error
	// Author is the resolved PR author login (best-effort, for logging).
	Author string
	// BaseRepo is the resolved PR base repo "owner/repo" (best-effort, for logging).
	BaseRepo string
}

// VerifyReportedPR checks a client-reported PR URL against GitHub server-side
// before the hub is willing to trust it (see kubestellar/hive#2565). The
// contributor relay scrapes a PR URL out of tmux output and reports it on
// task_complete; that value is entirely client-supplied, so on its own it must
// not drive the long completion cooldown or trust promotion. This resolves the
// URL via the GitHub API and confirms three things:
//
//  1. Exists      — the PR resolves via GET /repos/{owner}/{repo}/pulls/{number}.
//  2. Assigned    — the PR's BASE repo equals expectedRepo (the repo of the
//     assignment). This closes the relay's "first PR URL mentioned anywhere in
//     the tmux tail" fallback: an unrelated PR link no longer counts.
//  3. Author      — the PR author login equals expectedAuthor (the connected
//     contributor's GitHub identity, which the hub already knows).
//
// expectedRepo may be a bare repo name ("repo") or "owner/repo"; only the repo
// component is compared when no owner is supplied, so callers that track just the
// short repo name still match. Comparisons are case-insensitive (GitHub logins
// and repo names are).
//
// The method never panics and never returns an error to abort a caller: a
// GitHub API failure (rate limit, transient network, 404) yields
// Verified=false with Err set, so the completion handler can degrade to the
// short cooldown / no trust credit rather than crashing or stranding the
// contributor. A nil client (hive without GitHub credentials) is treated the
// same way — it cannot verify, so it must not grant trust.
func (c *Client) VerifyReportedPR(ctx context.Context, expectedRepo, prURL, expectedAuthor string) PRVerification {
	if c == nil || c.client == nil {
		return PRVerification{Reason: "no github client configured", Err: ErrNoGitHubClient}
	}

	ref, err := ParsePRURL(prURL)
	if err != nil {
		// A malformed / non-PR URL is a clean negative, not an API error: there
		// is nothing to look up. No Err → caller treats it as "no verified PR".
		return PRVerification{Reason: "unparseable PR URL: " + err.Error()}
	}

	pr, _, err := c.client.PullRequests.Get(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		// Transient/permission/404 — fail CLOSED on trust, but surface Err so the
		// caller logs it and treats the completion as unverified rather than
		// aborting. We do NOT distinguish 404 here: a PR the App cannot see is,
		// for trust purposes, indistinguishable from one that does not exist.
		return PRVerification{Reason: "github lookup failed", Err: err}
	}
	if pr == nil {
		return PRVerification{Reason: "github returned no pull request"}
	}

	author := safeGetLogin(pr.GetUser())
	baseRepo := pr.GetBase().GetRepo().GetFullName() // "owner/repo"
	v := PRVerification{Author: author, BaseRepo: baseRepo}

	if !prBaseRepoMatches(baseRepo, expectedRepo) {
		v.Reason = fmt.Sprintf("PR base repo %q does not match assigned repo %q", baseRepo, expectedRepo)
		return v
	}
	if expectedAuthor == "" || author == "" || !strings.EqualFold(author, expectedAuthor) {
		v.Reason = fmt.Sprintf("PR author %q does not match contributor %q", author, expectedAuthor)
		return v
	}

	v.Verified = true
	return v
}

// prBaseRepoMatches reports whether a PR's base repo ("owner/repo", from the API)
// satisfies the expected repo. expected may be bare ("repo") or "owner/repo".
// When expected carries an owner, both owner and repo must match; when it is a
// bare name, only the repo component is compared (callers that track only the
// short repo name still match). All comparisons are case-insensitive.
func prBaseRepoMatches(baseFullName, expected string) bool {
	baseFullName = strings.TrimSpace(baseFullName)
	expected = strings.TrimSpace(expected)
	if baseFullName == "" || expected == "" {
		return false
	}
	baseOwner, baseRepo := splitRepo(baseFullName)
	if expOwner, expRepo := splitRepo(expected); expOwner != "" {
		return strings.EqualFold(expOwner, baseOwner) && strings.EqualFold(expRepo, baseRepo)
	} else {
		return strings.EqualFold(expRepo, baseRepo)
	}
}

// splitRepo splits "owner/repo" into its parts. A bare "repo" yields an empty
// owner. Anything with more than one slash keeps only the last two segments.
func splitRepo(full string) (owner, repo string) {
	full = strings.Trim(strings.TrimSpace(full), "/")
	if full == "" {
		return "", ""
	}
	parts := strings.Split(full, "/")
	if len(parts) == 1 {
		return "", parts[0]
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

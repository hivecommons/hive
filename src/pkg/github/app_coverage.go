package github

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Repository coverage for a GitHub App installation (#4360).
//
// A hive can be pointed at a repo its App installation does not cover, and
// nothing says so. The failure surfaces later and lazily, as an agent action
// failing, and it is reported as something else entirely — the live case that
// prompted this reported "the App private key has not reached this spoke",
// which was false: the key had arrived, and re-uploading it could never help.
//
// The distinction from AppStateWriteForbidden matters. That state is the same
// underlying problem INFERRED from a 403 on a real write, after the fact and
// only for the one repo something happened to touch. This is the same problem
// established DIRECTLY, before anything is attempted, for every configured
// repo at once:
//
//	covered    := GET /installation/repositories
//	configured := project.repos
//	missing    := configured - covered
//
// No error-code inference, and no guessing between "not covered", "renamed",
// and "does not exist" — GitHub answers 404 for all three when an installation
// cannot see a repo, which is exactly why the 403/404 path cannot tell them
// apart and this one can.

// coveragePageSize is the maximum GitHub allows per page.
const coveragePageSize = 100

// coverageMaxPages bounds the walk. An installation set to "all repositories"
// on a large org can list thousands, and this check is not worth an unbounded
// number of round trips. Hitting the cap sets Truncated, and a truncated list
// deliberately reports NOTHING missing — see Missing.
const coverageMaxPages = 10

// InstallationCoverage is the set of repositories an App installation can see.
type InstallationCoverage struct {
	// Repos holds "owner/name" for every covered repository, lowercased,
	// because GitHub treats those names case-insensitively.
	Repos map[string]struct{}

	// Truncated reports that the listing stopped at coverageMaxPages, so the
	// set is a prefix rather than the whole of it. Absence from a truncated
	// set proves nothing.
	Truncated bool
}

// InstallationCoverage lists what this installation actually covers.
//
// It uses the installation token, not the App JWT: /installation/repositories
// is scoped to the token's own installation, which is precisely the question
// being asked. A nil receiver or one with no key returns an error rather than
// an empty set — "we could not ask" must never be mistaken for "it covers
// nothing", which would accuse every configured repo at once.
func (a *AppAuth) InstallationCoverage(ctx context.Context) (InstallationCoverage, error) {
	cov := InstallationCoverage{Repos: map[string]struct{}{}}

	if a == nil || !a.HasKey() {
		return cov, fmt.Errorf("no GitHub App key loaded")
	}

	token, err := a.Token(ctx)
	if err != nil {
		return cov, fmt.Errorf("minting installation token: %w", err)
	}

	client := newTokenClient(token, a.apiURL)
	opts := &gh.ListOptions{PerPage: coveragePageSize}

	for page := 1; page <= coverageMaxPages; page++ {
		list, resp, err := client.Apps.ListRepos(ctx, opts)
		if err != nil {
			return cov, fmt.Errorf("listing installation repositories: %w", err)
		}
		if list == nil {
			return cov, nil
		}
		for _, r := range list.Repositories {
			full := strings.TrimSpace(r.GetFullName())
			if full == "" {
				// Older/enterprise payloads occasionally omit full_name;
				// rebuild it rather than dropping the repo, because a dropped
				// entry reads as "not covered" and would be an accusation.
				owner := strings.TrimSpace(r.GetOwner().GetLogin())
				name := strings.TrimSpace(r.GetName())
				if owner == "" || name == "" {
					continue
				}
				full = owner + "/" + name
			}
			cov.Repos[strings.ToLower(full)] = struct{}{}
		}
		if resp == nil || resp.NextPage == 0 {
			return cov, nil
		}
		opts.Page = resp.NextPage
	}

	cov.Truncated = true
	return cov, nil
}

// NormalizeRepoRef expands a configured repo reference to "owner/name".
// Configuration accepts both a bare name and a fully qualified one; comparison
// needs the qualified form on both sides.
func NormalizeRepoRef(owner, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if strings.Contains(ref, "/") {
		return strings.ToLower(ref)
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return strings.ToLower(ref)
	}
	return strings.ToLower(owner + "/" + ref)
}

// Missing returns the configured repos this installation does not cover, in
// "owner/name" form, sorted for stable copy and stable tests.
//
// A truncated listing returns nothing. Absence from a partial set is not
// evidence of absence from the whole, and a false "your App does not include
// this repo" sends an operator to change a setting that was already correct —
// the exact class of mistake this whole check exists to stop making.
func (c InstallationCoverage) Missing(owner string, configured []string) []string {
	if c.Truncated {
		return nil
	}

	seen := map[string]struct{}{}
	var missing []string
	for _, ref := range configured {
		full := NormalizeRepoRef(owner, ref)
		if full == "" {
			continue
		}
		if _, ok := c.Repos[full]; ok {
			continue
		}
		if _, dup := seen[full]; dup {
			continue
		}
		seen[full] = struct{}{}
		missing = append(missing, full)
	}
	sort.Strings(missing)
	return missing
}

// RepoMove is one configured repository that the installation does not cover
// under the configured owner, but DOES cover under a different one.
type RepoMove struct {
	// Configured is the "owner/name" the hive is pointed at, lowercased.
	Configured string
	// CoveredAs is the "owner/name" the installation actually covers,
	// lowercased — the same repository name under a different account.
	CoveredAs string
}

// MovedTo detects the org-transfer shape of a coverage miss (#5774).
//
// WHY THIS IS NOT JUST A SUBSET OF Missing. Missing answers "which configured
// repos does this installation not cover", and its fix — tick the repo in the
// installation's repository access — is right for the case it was written for
// (#4360: a second repo in the RIGHT org that was never ticked). Applied to a
// repository that has been TRANSFERRED to another account, that same copy sends
// an operator to tick a repo that no longer exists under the owner they are
// looking at. That is not a less precise version of the right answer; it is a
// wrong one. Preferring deterministic coverage over 403/404 inference exists in
// this file precisely to stop issuing that class of confident misdirection.
//
// The kubestellar to hivecommons migration is the live case. Once the App is
// installed on the new org while a hive's config still names the old one, every
// configured repo reads as "not covered" and the banner points at a settings
// page for an org the repository has left.
//
// THE RULE — deliberately narrow, all three clauses required:
//
//  1. The listing is complete. A truncated set proves no absence — the same
//     rule Missing already follows, for the same reason.
//  2. The installation covers NOTHING AT ALL under the configured owner. A
//     transfer moves the whole repository out of that account; if the
//     installation still covers other repos there, the account is reachable and
//     a single missing repo is an ordinary scope gap, which is Missing's story
//     and not this one.
//  3. Exactly ONE covered repository carries the configured repo's name. Two
//     accounts owning a repo called "tools" is unremarkable, and picking one of
//     them would be a guess. With no candidate, or more than one, this returns
//     nothing and Missing's verdict stands.
//
// Returns nil when nothing matches, so a caller can simply prefer a non-empty
// result over the not-covered verdict.
func (c InstallationCoverage) MovedTo(owner string, configured []string) []RepoMove {
	if c.Truncated {
		return nil
	}
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "" || len(c.Repos) == 0 {
		return nil
	}

	// Clause 2: the configured owner must be entirely absent from coverage.
	ownerPrefix := owner + "/"
	for full := range c.Repos {
		if strings.HasPrefix(full, ownerPrefix) {
			return nil
		}
	}

	// Index coverage by bare repository name, so a change of OWNER is visible
	// as "same name, different account".
	byName := map[string][]string{}
	for full := range c.Repos {
		i := strings.LastIndex(full, "/")
		if i <= 0 || i == len(full)-1 {
			continue
		}
		byName[full[i+1:]] = append(byName[full[i+1:]], full)
	}

	seen := map[string]struct{}{}
	var moves []RepoMove
	for _, ref := range configured {
		full := NormalizeRepoRef(owner, ref)
		if full == "" {
			continue
		}
		if _, covered := c.Repos[full]; covered {
			continue
		}
		if _, dup := seen[full]; dup {
			continue
		}
		i := strings.LastIndex(full, "/")
		if i <= 0 || i == len(full)-1 {
			continue
		}
		candidates := byName[full[i+1:]]
		// Clause 3: exactly one candidate, or this would be a guess.
		if len(candidates) != 1 {
			continue
		}
		seen[full] = struct{}{}
		moves = append(moves, RepoMove{Configured: full, CoveredAs: candidates[0]})
	}

	sort.Slice(moves, func(i, j int) bool { return moves[i].Configured < moves[j].Configured })
	return moves
}

// MovedOwner returns the single account every move in the set points at, or ""
// when the set is empty or the moves disagree. Copy that names one destination
// org must never be composed from a set that names several.
func MovedOwner(moves []RepoMove) string {
	owner := ""
	for _, m := range moves {
		i := strings.Index(m.CoveredAs, "/")
		if i <= 0 {
			return ""
		}
		acct := m.CoveredAs[:i]
		if owner == "" {
			owner = acct
			continue
		}
		if !strings.EqualFold(owner, acct) {
			return ""
		}
	}
	return owner
}

// webBaseFromAPIURL turns a GitHub API base into the web base an operator can
// click. github.com and GitHub Enterprise spell these differently, and a
// hardcoded github.com link on a GHE hive sends the operator to an account
// they may not even have.
//
//	https://api.github.com/            -> https://github.com
//	https://ghe.example.com/api/v3/    -> https://ghe.example.com
func webBaseFromAPIURL(apiURL string) string {
	raw := strings.TrimSpace(apiURL)
	if raw == "" {
		return "https://github.com"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if strings.EqualFold(u.Host, "api.github.com") {
		return "https://github.com"
	}
	// Enterprise: the API lives under /api/v3 on the same host the web UI uses.
	return u.Scheme + "://" + u.Host
}

// InstallationSettingsURL is the page that actually fixes AppStateRepoNotCovered:
// the org's settings for this specific installation, where repository access is
// ticked. Empty when there is not enough information to build a link that
// works, because a wrong link is worse than none.
func (d AppAuthDiagnosis) InstallationSettingsURL() string {
	org := strings.TrimSpace(d.ExpectedAccount)
	if org == "" || d.InstallationID == 0 {
		return ""
	}
	base := webBaseFromAPIURL(d.APIURL)
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/organizations/%s/settings/installations/%d", base, org, d.InstallationID)
}

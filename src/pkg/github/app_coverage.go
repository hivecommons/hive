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

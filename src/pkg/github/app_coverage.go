package github

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

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

	return a.installationCoverageWithToken(ctx, token)
}

// InstallationCoverageForID lists repositories for a discovered installation
// without changing the installation or cached token used by the hive.
func (a *AppAuth) InstallationCoverageForID(ctx context.Context, installationID int64) (InstallationCoverage, error) {
	if a == nil || !a.HasKey() {
		return InstallationCoverage{}, fmt.Errorf("no GitHub App key loaded")
	}
	if installationID <= 0 {
		return InstallationCoverage{}, fmt.Errorf("invalid installation ID")
	}
	token, _, err := a.mintInstallationTokenWithOptions(ctx, installationID, nil)
	if err != nil {
		return InstallationCoverage{}, fmt.Errorf("minting installation token: %w", err)
	}
	return a.installationCoverageWithToken(ctx, token)
}

// RepositoryCoveredByInstallation checks one repository with the App JWT.
// Unlike a repository GET with an installation token, this endpoint identifies
// the actual installation and cannot mistake public read access for selection.
// A 404 means the App has no installation for the repository; other failures
// and an unexpected installation are inconclusive.
func (a *AppAuth) RepositoryCoveredByInstallation(ctx context.Context, installationID int64, owner, repo string) (bool, error) {
	if a == nil || !a.HasKey() || installationID <= 0 || owner == "" || repo == "" {
		return false, fmt.Errorf("invalid repository installation probe")
	}
	token, err := a.generateJWT()
	if err != nil {
		return false, fmt.Errorf("generating JWT: %w", err)
	}
	inst, resp, err := newJWTClient(token, a.apiURL).Apps.FindRepositoryInstallation(ctx, owner, repo)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return false, nil
		}
		return false, fmt.Errorf("finding repository installation: %w", err)
	}
	if inst == nil || inst.GetID() != installationID {
		return false, fmt.Errorf("repository installation does not match discovered installation %d", installationID)
	}
	return true, nil
}

func (a *AppAuth) installationCoverageWithToken(ctx context.Context, token string) (InstallationCoverage, error) {
	cov := InstallationCoverage{Repos: map[string]struct{}{}}

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

// renameProbeLimit bounds how many uncovered repos ResolveRenames looks up in
// one check. The recheck runs on a timer, and a hive pointed at many unticked
// repos must not turn every pass into a burst of extra API calls; repos past
// the limit simply keep their not-covered verdict.
const renameProbeLimit = 20

// repoNameResolver returns the "owner/name" GitHub currently reports for a
// repository reference, following the redirect GitHub serves for a renamed one.
type repoNameResolver func(ctx context.Context, owner, name string) (string, error)

// ResolveRenames (#19915) drops repos from a Missing result that were RENAMED
// within the configured account and are covered under their new name.
//
// WHY. Missing compares configured names against the installation listing by
// string. When a repository is renamed (tuna-os/bootc-installer to
// tuna-os/installer was the live case), the listing reports the new name, the
// hive's config still holds the old one, and the check reports the repo as not
// covered. Every call the hive makes still works, because GitHub redirects the
// old name, so the hive is healthy, yet the banner tells an org owner to tick a
// repo that no longer appears under that name on the settings page. That is the
// same confident misdirection #4360 and #5774 exist to stop.
//
// THE RULE, every clause required for a repo to be dropped:
//
//  1. The listing is complete. A truncated listing proves nothing.
//  2. GitHub resolves the configured name to a DIFFERENT name. A repo that
//     resolves to itself is an ordinary unticked repo.
//  3. The resolved name is under the SAME account. A change of account is a
//     transfer, which MovedTo and AppStateRepoMoved report.
//  4. The installation listing contains the resolved name. Being able to read
//     a public repo proves nothing about coverage; the listing does.
//
// A lookup error keeps the repo missing, so a failed lookup can never hide a
// real gap. Returns the repos still missing (sorted) and the renames found.
func (a *AppAuth) ResolveRenames(ctx context.Context, cov InstallationCoverage, missing []string) (stillMissing []string, renamed []RepoMove) {
	if a == nil || !a.HasKey() {
		return missing, nil
	}
	return cov.withoutRenames(ctx, missing, func(ctx context.Context, owner, name string) (string, error) {
		token, err := a.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("minting installation token: %w", err)
		}
		repo, _, err := newTokenClient(token, a.apiURL).Repositories.Get(ctx, owner, name)
		if err != nil {
			return "", fmt.Errorf("resolving repository name: %w", err)
		}
		return repo.GetFullName(), nil
	})
}

func (c InstallationCoverage) withoutRenames(ctx context.Context, missing []string, resolve repoNameResolver) ([]string, []RepoMove) {
	if c.Truncated || len(missing) == 0 || resolve == nil {
		return missing, nil
	}
	var still []string
	var renamed []RepoMove
	probes := 0
	for _, full := range missing {
		owner, name, ok := strings.Cut(full, "/")
		if !ok || owner == "" || name == "" || probes >= renameProbeLimit {
			still = append(still, full)
			continue
		}
		probes++
		got, err := resolve(ctx, owner, name)
		if err != nil {
			still = append(still, full)
			continue
		}
		canonical := strings.ToLower(strings.TrimSpace(got))
		newOwner, _, ok := strings.Cut(canonical, "/")
		if !ok || canonical == full || newOwner != strings.ToLower(owner) {
			still = append(still, full)
			continue
		}
		if _, covered := c.Repos[canonical]; !covered {
			still = append(still, full)
			continue
		}
		renamed = append(renamed, RepoMove{Configured: full, CoveredAs: canonical})
	}
	sort.Strings(still)
	return still, renamed
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

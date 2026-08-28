package github

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ── Why this file exists ────────────────────────────────────────────────────
//
// A classic PAT with too few scopes does not fail at boot. It fails much later
// and far from the cause: the scanner reports "no actionable work" because a
// search 403'd, or an agent's PR creation dies inside gh-wrapper with a generic
// "403 Resource not accessible". Nothing in that chain names the missing scope,
// so an operator has to reverse-engineer which capability was denied from a
// symptom that looks like an empty backlog.
//
// GitHub hands us the answer for free. Every authenticated REST response
// carries X-OAuth-Scopes (the scopes the token was granted) and
// X-Accepted-OAuth-Scopes (what the endpoint wanted). One cheap authenticated
// call at boot turns a silent, delayed, mis-attributed failure into a specific
// startup line naming the scope AND the capability it costs.
//
// This check is a DIAGNOSTIC and must never become a failure mode of its own:
// it fails soft in every direction (see CheckTokenScopes).

const (
	// scopeCheckTimeout bounds the single boot-time probe. Startup must not be
	// held hostage by a slow or blackholed api.github.com — under forced proxy
	// egress a misconfigured proxy can hang a connection indefinitely. Five
	// seconds is far above the p99 for a /rate_limit call yet short enough to
	// be invisible in a boot sequence that already takes seconds.
	scopeCheckTimeout = 5 * time.Second

	// oauthScopesHeader carries the scopes actually granted to a classic PAT.
	// It is ABSENT (not empty-but-present) for GitHub App installation tokens
	// and is present-but-empty for fine-grained PATs, which carry repository
	// permissions instead of scopes. That distinction is the whole reason
	// ScopeResult separates "not determinable" from "missing".
	oauthScopesHeader = "X-OAuth-Scopes"
)

// Scope names GitHub uses for classic PATs. Named rather than inlined so the
// requirement table below reads as capabilities, not string literals.
const (
	scopeRepo       = "repo"
	scopePublicRepo = "public_repo"
	scopeReadOrg    = "read:org"
	scopeWorkflow   = "workflow"
	scopeRepoStatus = "repo:status"
)

// ScopeStatus classifies the outcome of the boot-time scope probe.
type ScopeStatus string

const (
	// ScopeStatusOK means the granted scopes cover everything this hive needs
	// at its configured ACMM level.
	ScopeStatusOK ScopeStatus = "ok"
	// ScopeStatusMissing means the token IS a classic PAT, we read its granted
	// scopes, and a required one is genuinely absent. This is the only status
	// that asserts a defect.
	ScopeStatusMissing ScopeStatus = "missing"
	// ScopeStatusUndetermined means we could not learn the token's scopes:
	// a fine-grained PAT (no X-OAuth-Scopes), an App token, a network error,
	// or a non-GitHub API endpoint. Absence of evidence — reported as such and
	// never as evidence of absence.
	ScopeStatusUndetermined ScopeStatus = "undetermined"
	// ScopeStatusSkipped means the check did not apply — App auth, or no
	// client at all.
	ScopeStatusSkipped ScopeStatus = "skipped"
)

// ScopeResult is the outcome of the boot-time probe, in a shape both the
// startup log and the dashboard's github_auth health check can render.
type ScopeResult struct {
	Status ScopeStatus
	// Granted is the scope list read from X-OAuth-Scopes. Only meaningful for
	// ScopeStatusOK / ScopeStatusMissing.
	Granted []string
	// Missing lists required scopes that were not granted, in the order the
	// requirement table declares them (most consequential first).
	Missing []string
	// Detail is the operator-facing sentence. For ScopeStatusMissing it names
	// the scope AND the capability lost, because "insufficient permissions" is
	// exactly the unhelpful message this check exists to replace.
	Detail string
	// Reason explains an Undetermined result so an operator can tell "your
	// token is fine-grained, we can't introspect it" from "we couldn't reach
	// GitHub".
	Reason string
}

// scopeRequirement binds one scope to the capability an operator loses without
// it and the lowest ACMM level at which the hive actually exercises that
// capability.
type scopeRequirement struct {
	// scope is the classic-PAT scope name.
	scope string
	// alternatives are scopes that also satisfy this requirement. GitHub's
	// scope hierarchy is not a simple set-membership test: a token granted
	// "repo" implicitly covers "public_repo", "repo:status", and the Actions
	// read surface, and GitHub reports only the broader scope in
	// X-OAuth-Scopes. Without this a perfectly good "repo" token would be
	// warned at for "missing" repo:status — a false alarm of exactly the kind
	// this check must not produce.
	alternatives []string
	// minLevel is the lowest ACMM level whose agents exercise the capability.
	// Below it the scope is genuinely unnecessary and warning about it would
	// be noise on a correctly-configured read-only hive.
	minLevel int
	// capability is the operator-facing consequence, phrased as what breaks.
	capability string
}

// ACMM level boundaries at which new GitHub capabilities switch on. These
// mirror the level packs in pkg/config/packs/: L1 Inception, L2 Advisory
// (read + comment), L3 Quality-Gated, L4 Security-Aware (issue filing),
// L5 Semi-Autonomous (PR creation), L6 Fully Autonomous (auto-merge).
const (
	// acmmLevelAdvisory (L2) is where agents first read repositories and post
	// advisory comments — the lowest level that touches GitHub at all.
	acmmLevelAdvisory = 2
	// acmmLevelSecurityAware (L4) is where agents file issues and manage
	// labels rather than only commenting.
	acmmLevelSecurityAware = 4
	// acmmLevelSemiAutonomous (L5) is where agents open pull requests.
	acmmLevelSemiAutonomous = 5

	// ACMMLevelUnset is passed by callers whose config carries no acmm_level.
	// It resolves to the MOST capable level, deliberately: an unset level means
	// we cannot know what the hive will attempt, and under-warning (staying
	// silent about a genuinely missing scope) is precisely the failure this
	// check exists to eliminate. The cost of over-warning is one actionable log
	// line. Note this is why the caller must not reuse inferACMMLevel's L1
	// default here — L1 requires no scopes, so it would suppress everything.
	ACMMLevelUnset = -1

	// acmmLevelFullyAutonomous (L6) is the ceiling, and what ACMMLevelUnset
	// resolves to.
	acmmLevelFullyAutonomous = 6
)

// scopeRequirements maps the GitHub REST surface this hive actually calls to
// the classic-PAT scopes those calls need.
//
// DERIVED FROM THE CODE, not from documentation-by-guesswork. The call sites
// backing each entry are named so a future reader can re-verify the mapping
// when the API surface changes — this is the table most likely to drift.
var scopeRequirements = []scopeRequirement{
	{
		// pullrequest.go PullRequests.Create; automerge_sweep.go
		// PullRequests.Merge; client.go Issues.CreateComment;
		// advisory.go Issues.Create. Every write path against a private repo
		// needs full "repo"; GitHub grants no narrower write scope.
		scope:        scopeRepo,
		alternatives: []string{},
		minLevel:     acmmLevelSemiAutonomous,
		capability:   "agents will fail to create or merge pull requests",
	},
	{
		// advisory.go Issues.Create + Issues.AddLabelsToIssue /
		// Issues.CreateLabel. Issue filing on a public repo is satisfied by
		// public_repo; a private repo needs full repo, which subsumes it.
		scope:        scopePublicRepo,
		alternatives: []string{scopeRepo},
		minLevel:     acmmLevelSecurityAware,
		capability:   "agents will fail to file issues or apply labels",
	},
	{
		// client.go fetchIssues (Issues.ListByRepo), Search.Issues, and
		// advisory.go Issues.CreateComment — the read + advise surface. Public
		// repos need public_repo; private repos need repo.
		scope:        scopePublicRepo,
		alternatives: []string{scopeRepo},
		minLevel:     acmmLevelAdvisory,
		capability:   "the scanner will see no actionable issues and agents cannot post advisory comments",
	},
	{
		// health.go FetchWorkflowHealth (Actions.ListWorkflows /
		// ListWorkflowRunsByID / ListWorkflowJobs) and automerge_sweep.go
		// commitGreen (Checks.ListCheckRunsForRef,
		// Repositories.GetCombinedStatus).
		//
		// minLevel is acmmLevelAdvisory, NOT L5, even though the merge
		// consequence is an L5+ concern: FetchWorkflowHealth is ungated and
		// runs at EVERY level, so an L2 hive with a token that cannot read
		// Actions loses its workflow-health dashboard silently. Gating this at
		// L5 would have left exactly that hive unwarned. The capability string
		// therefore names the read loss first, which is what is true at every
		// level.
		//
		// "workflow" is NOT required: it governs mutating workflow FILES, which
		// this hive only ever does through PR contents. It is listed as an
		// alternative because a token that has it also reads Actions fine.
		scope:        scopeRepoStatus,
		alternatives: []string{scopeRepo, scopePublicRepo, scopeWorkflow},
		minLevel:     acmmLevelAdvisory,
		capability:   "CI status is unreadable — workflow health shows unknown, and at L5+ the auto-merge sweep will never merge a green PR",
	},
	{
		// client.go ListContributors and the org-scoped Search.Issues in
		// fleet_stats.go / pr_issue_counts.go. Reading org membership and
		// private org repos requires read:org; "repo" alone does not grant it.
		//
		// Deliberately NOT justified by app_discovery.go's org lookups
		// (Apps.FindOrganizationInstallation): those run on a JWT App client,
		// never on a PAT, so they are irrelevant to a scope check that only
		// runs on the token path.
		scope:        scopeReadOrg,
		alternatives: []string{},
		minLevel:     acmmLevelSecurityAware,
		capability:   "org-scoped issue search and contributor lookups will return empty results",
	},
}

// requiredScopesForLevel returns the deduplicated requirements that apply at
// the given ACMM level, preserving table order so the most consequential
// capability is reported first.
func requiredScopesForLevel(level int) []scopeRequirement {
	if level == ACMMLevelUnset {
		level = acmmLevelFullyAutonomous
	}
	var out []scopeRequirement
	seen := make(map[string]bool)
	for _, r := range scopeRequirements {
		if level < r.minLevel || seen[r.scope] {
			continue
		}
		seen[r.scope] = true
		out = append(out, r)
	}
	return out
}

// satisfied reports whether the granted set covers this requirement, honouring
// the scope hierarchy via alternatives.
func (r scopeRequirement) satisfied(granted map[string]bool) bool {
	if granted[r.scope] {
		return true
	}
	for _, alt := range r.alternatives {
		if granted[alt] {
			return true
		}
	}
	return false
}

// parseScopeHeader splits the comma-separated X-OAuth-Scopes value. GitHub
// emits ", " separators; entries are trimmed and empties dropped so a header
// of "" or ", " yields an empty slice rather than phantom scopes.
func parseScopeHeader(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// CheckTokenScopes probes the token's granted scopes and compares them against
// what this hive needs at acmmLevel.
//
// FAIL-SOFT IN EVERY DIRECTION. It returns a result and never an error; no
// input, no network condition, and no API response can make it report a
// problem it did not actually observe:
//
//   - App auth (c.appAuth != nil) → Skipped. An App has PERMISSIONS, not
//     scopes; X-OAuth-Scopes does not apply and warning about a PAT that is
//     not in use would be pure noise.
//   - Header absent or empty → Undetermined, NEVER Missing. Fine-grained PATs
//     carry repository permissions and report no scopes at all, so a
//     fail-closed reading here would raise a false alarm on every
//     fine-grained-token deployment — and if it gated startup, would brick
//     them. Absence of evidence is not evidence of absence.
//   - Network error or timeout → Undetermined. The check is a diagnostic; it
//     must never invent a new way for a hive to fail to boot.
//
// It issues exactly ONE authenticated request, at boot only. The fleet shares a
// rate-limit budget, so this must not become a periodic probe. /rate_limit is
// deliberately chosen as the probe endpoint: it is authenticated (so GitHub
// returns the scope headers), needs no scopes of its own (so it cannot 403 for
// the very reason we are investigating), and does not itself consume quota.
func (c *Client) CheckTokenScopes(ctx context.Context, acmmLevel int) ScopeResult {
	if c == nil || c.client == nil {
		return ScopeResult{Status: ScopeStatusSkipped, Reason: "no github client configured"}
	}
	if c.appAuth != nil {
		return ScopeResult{
			Status: ScopeStatusSkipped,
			Reason: "GitHub App authentication in use — Apps carry permissions, not OAuth scopes",
		}
	}

	ctx, cancel := context.WithTimeout(ctx, scopeCheckTimeout)
	defer cancel()

	req, err := c.client.NewRequest(http.MethodGet, "rate_limit", nil)
	if err != nil {
		return ScopeResult{Status: ScopeStatusUndetermined, Reason: "could not build scope probe request: " + err.Error()}
	}
	resp, err := c.client.Do(ctx, req, nil)
	if err != nil && resp == nil {
		// No response at all: DNS, dial, TLS, or timeout. We learned nothing.
		return ScopeResult{Status: ScopeStatusUndetermined, Reason: "could not reach GitHub to read token scopes: " + err.Error()}
	}
	if resp == nil || resp.Response == nil {
		return ScopeResult{Status: ScopeStatusUndetermined, Reason: "no response headers available to read token scopes"}
	}

	// The header may be present-and-empty (fine-grained PAT) or wholly absent
	// (App token, non-GitHub endpoint). Both mean "not introspectable", and
	// both must read as Undetermined. Note that an error response is still
	// useful here — GitHub sets the scope headers on 401/403 too — so we read
	// the header before considering err.
	raw, present := resp.Header[http.CanonicalHeaderKey(oauthScopesHeader)]
	if !present {
		if err != nil {
			return ScopeResult{Status: ScopeStatusUndetermined, Reason: "GitHub returned no scope header: " + err.Error()}
		}
		return ScopeResult{
			Status: ScopeStatusUndetermined,
			Reason: "GitHub returned no " + oauthScopesHeader + " header — the token is not a classic PAT (fine-grained PATs carry repository permissions, not scopes)",
		}
	}

	granted := parseScopeHeader(strings.Join(raw, ","))
	if len(granted) == 0 {
		// Present but empty is the fine-grained-PAT signature. Saying "missing
		// repo" here would be a false alarm on a working deployment.
		return ScopeResult{
			Status: ScopeStatusUndetermined,
			Reason: "GitHub reported no OAuth scopes for this token — it is a fine-grained PAT, whose repository permissions cannot be read from " + oauthScopesHeader + "; verify its permissions in GitHub settings",
		}
	}

	grantedSet := make(map[string]bool, len(granted))
	for _, s := range granted {
		grantedSet[s] = true
	}

	var missing []string
	var details []string
	for _, r := range requiredScopesForLevel(acmmLevel) {
		if r.satisfied(grantedSet) {
			continue
		}
		missing = append(missing, r.scope)
		details = append(details, `github token is missing the "`+r.scope+`" scope — `+r.capability)
	}

	if len(missing) == 0 {
		return ScopeResult{Status: ScopeStatusOK, Granted: granted}
	}
	return ScopeResult{
		Status:  ScopeStatusMissing,
		Granted: granted,
		Missing: missing,
		Detail:  strings.Join(details, "; "),
	}
}

// LogTokenScopeCheck runs the probe and reports the outcome at the severity the
// operator needs, then returns the result so a caller can also surface it in
// the dashboard's github_auth health check.
//
// Never logs token material — only scope NAMES and capability consequences.
// A silent pass is intentional: a correctly-scoped token produces no output,
// so any line from this check is actionable by construction.
func (c *Client) LogTokenScopeCheck(ctx context.Context, logger *slog.Logger, acmmLevel int) ScopeResult {
	res := c.CheckTokenScopes(ctx, acmmLevel)
	if logger == nil {
		return res
	}
	switch res.Status {
	case ScopeStatusMissing:
		logger.Warn("github token scope check failed",
			"detail", res.Detail,
			"missing_scopes", strings.Join(res.Missing, ","),
			"granted_scopes", strings.Join(res.Granted, ","),
			"acmm_level", acmmLevel,
		)
	case ScopeStatusUndetermined:
		// Debug, not Warn. We observed nothing wrong; a fine-grained PAT is a
		// perfectly valid configuration and must not be nagged at every boot.
		logger.Debug("github token scopes could not be determined", "reason", res.Reason)
	case ScopeStatusSkipped, ScopeStatusOK:
		// Silent by design — requirement 5, zero noise when nothing is wrong.
	}
	return res
}

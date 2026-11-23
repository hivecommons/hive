package github

import (
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

// Trusted issue authors and the reporter-trust awaiting label.
//
// Owner decision (hivecommons/hive#13298): triage-needed is not a stage for
// hive agents and not for trusted humans. That decision assumed only the
// #9665 reporter-trust gate puts the awaiting label (`needs-triage`) on an
// issue, and the gate never does that for a trusted or hive-filed issue.
// A repository that runs Prow or another triage bot breaks the assumption:
// the bot labels EVERY new issue, so the label alone held maintainer- and
// hive-filed issues at 015 and in the contributor skip list until a person
// removed it by hand. The label therefore means "awaiting a person's first
// look" only on an untrusted author's issue. issueAuthorTrusted decides it
// once per issue at enumeration (and on the stage sink's fresh read); the
// verdict rides on Issue / RepoWorkIssue / IssueStageInput /
// stageengine.IssueFacts as AuthorTrusted, and config.HubConfig
// .MatchContributeSkipLabelFor is the skip-list half.

// reporterTruster is the trust half of the reporter-trust config
// (config.IssueFilterConfig.ReporterTrustTrusts). The enumeration's issue
// filter implements it in production.
type reporterTruster interface {
	ReporterTrustTrusts(login, association string) bool
}

// issueAuthorTrusted is the one place pkg/github decides whether an issue's
// author is trusted for the purpose of the reporter-trust awaiting label. The
// owner's decision names hive agents and trusted humans, so:
//
//   - this hive's own accounts (isHiveAccount: the GitHub App bot, the
//     configured ai_author and additional hive bot logins, a hive identity,
//     the login GET /user resolved) are trusted. That covers the
//     self-reports this hive's App files (fleetreport stamps `needs-triage`
//     at filing). A self-report marker in the body (IsHiveSelfReport) is not
//     enough on its own here: it is text anyone can paste, and on this path
//     it would release a hold, so another account's self-report is judged
//     like any other issue;
//   - every other author is trusted only by the reporter-trust gate's own
//     predicate (config.ReporterTrustConfig.Trusted: TrustedLogins, then the
//     effective trusted associations, OWNER / MEMBER / COLLABORATOR by
//     default). The effective set applies whether or not the gate is
//     enabled: it has defaults, and a triage bot labels issues either way;
//   - another App or bot (`[bot]` login or Type=Bot, not one of this hive's
//     accounts) is trusted only by an explicit TrustedLogins entry, never by
//     its author_association: a third-party App (github-actions[bot] copying
//     a user's text, say) may be CONTRIBUTOR or better on the repository,
//     and Prow's label is the only thing holding its issue. An operator
//     trusts a specific bot, Prow's own for instance, by listing its login
//     in reporter_trust.trusted_logins;
//   - an unknown author (no login seen) is NOT trusted, so the label keeps
//     holding the issue. That fails safe, and it is deliberately the
//     opposite of the display rule that reads an unknown author as a human
//     (hivecommons/hive#13287): here "we could not tell" must not release an
//     issue a triage bot is holding.
func (c *Client) issueAuthorTrusted(issue *gh.Issue) bool {
	if issue == nil {
		return false
	}
	user := issue.GetUser()
	login := strings.TrimSpace(user.GetLogin())
	if login == "" {
		return false
	}
	if c.isHiveAccount(user) {
		return true
	}
	association := issue.GetAuthorAssociation()
	if !c.isHumanAuthor(user) {
		// Another App or bot: trusted by login only.
		association = ""
	}
	return c.reporterTrustTrusts(login, association)
}

// reporterTrustTrusts asks the live issue filter's reporter-trust predicate
// (config.IssueFilterConfig.ReporterTrustTrusts → ReporterTrustConfig
// .Trusted, the predicate admission's AdmitsReporter uses), falling back to
// the built-in defaults (config.ReporterTrustConfig{}) for a client without
// one installed. It does not reuse the PR hold's reporterIsTrusted: that
// seam is the same predicate but is wired only for an App hive with an agent
// manager (cmd/hive applyGitHubClientAgentHooks), while the issue filter is
// installed on every client (applyGitHubClientConfigHooks), and a PAT hive
// must not read every maintainer as untrusted.
func (c *Client) reporterTrustTrusts(login, association string) bool {
	if c != nil {
		if t, ok := c.getIssueFilter().(reporterTruster); ok {
			return t.ReporterTrustTrusts(login, association)
		}
	}
	return config.ReporterTrustConfig{}.Trusted(login, association)
}

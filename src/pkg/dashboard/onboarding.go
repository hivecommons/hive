package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Onboarding problem kinds. Each one corresponds to a step of the dashboard's
// Getting Started guide (WELCOME_STEPS in static/index.html), so the SPA can
// append "open the 🧭 Guide → step …" to whatever surface renders the error.
// Stable wire tokens — the SPA keys off them; rename only with the SPA.
const (
	OnboardingKindGitHubAppNotInstalled      = "github-app-not-installed"
	OnboardingKindGitHubAppWrongInstallation = "github-app-wrong-installation"
	OnboardingKindGitHubAppPermissions       = "github-app-permissions"
	OnboardingKindGitHubAppCredentials       = "github-app-credentials"
	OnboardingKindGitHubBaseURL              = "github-base-url"
	OnboardingKindRepoTarget                 = "repo-target"
	OnboardingKindNoRepos                    = "no-repos"
	OnboardingKindNoAgents                   = "no-agents"
	OnboardingKindProxyToken                 = "proxy-token"
)

// Getting Started guide step ids (WELCOME_STEPS[*].id in static/index.html).
const (
	GuideStepGitHubApp = "github-app"
	GuideStepRepos     = "repos"
	GuideStepAddAgent  = "add-agent"
	GuideStepWelcome   = "welcome"
)

// OnboardingIssue is one setup problem the Getting Started guide has a step
// for. It rides on the status payload (StatusPayload.OnboardingIssues) so
// every dashboard surface can render the same guide tip for it.
type OnboardingIssue struct {
	Kind      string `json:"kind"`
	GuideStep string `json:"guideStep"`
	Message   string `json:"message,omitempty"`
}

// onboardingKindGuideStep maps a kind to the guide step that fixes it.
func onboardingKindGuideStep(kind string) string {
	switch kind {
	case OnboardingKindGitHubAppNotInstalled, OnboardingKindGitHubAppWrongInstallation,
		OnboardingKindGitHubAppPermissions, OnboardingKindGitHubAppCredentials,
		OnboardingKindGitHubBaseURL:
		return GuideStepGitHubApp
	case OnboardingKindRepoTarget, OnboardingKindNoRepos:
		return GuideStepRepos
	case OnboardingKindNoAgents:
		return GuideStepAddAgent
	case OnboardingKindProxyToken:
		return GuideStepWelcome
	}
	return ""
}

// onboardingMessageRules is the ordered substring table behind
// ClassifyOnboardingMessage. First match wins, so the more specific
// phrasings (wrong installation, permissions) sit above the generic
// "installation"/"GitHub App" catch-alls. Matching is case-insensitive.
var onboardingMessageRules = []struct {
	needle string
	kind   string
}{
	{"has no installation for", OnboardingKindGitHubAppNotInstalled},
	{"app not installed", OnboardingKindGitHubAppNotInstalled},
	{"app is not installed", OnboardingKindGitHubAppNotInstalled},
	{"installation_id is 0", OnboardingKindGitHubAppNotInstalled},
	{"installation id is 0", OnboardingKindGitHubAppNotInstalled},
	{"no installation id", OnboardingKindGitHubAppNotInstalled},
	{"installation not found", OnboardingKindGitHubAppWrongInstallation},
	{"installation id mismatch", OnboardingKindGitHubAppWrongInstallation},
	{"wrong installation", OnboardingKindGitHubAppWrongInstallation},
	{"installed on a different org", OnboardingKindGitHubAppWrongInstallation},
	{"different org", OnboardingKindGitHubAppWrongInstallation},
	{"pending org approval", OnboardingKindGitHubAppPermissions},
	{"insufficient permission", OnboardingKindGitHubAppPermissions},
	{"missing permission", OnboardingKindGitHubAppPermissions},
	{"permission issue", OnboardingKindGitHubAppPermissions},
	{"permissions: missing", OnboardingKindGitHubAppPermissions},
	{"resource not accessible by integration", OnboardingKindGitHubAppPermissions},
	{"private key", OnboardingKindGitHubAppCredentials},
	{"credentials not delivered", OnboardingKindGitHubAppCredentials},
	{"never assigned a github app", OnboardingKindGitHubAppCredentials},
	{"placeholder github.app_id", OnboardingKindGitHubAppCredentials},
	{"github.app_id", OnboardingKindGitHubAppCredentials},
	{"github app id", OnboardingKindGitHubAppCredentials},
	{"github.base_url", OnboardingKindGitHubBaseURL},
	{"github base_url", OnboardingKindGitHubBaseURL},
	{"github base url", OnboardingKindGitHubBaseURL},
	{"github_app_slug", OnboardingKindGitHubBaseURL},
	{"proxy token", OnboardingKindProxyToken},
	{"proxy_token", OnboardingKindProxyToken},
	{"no repos configured", OnboardingKindNoRepos},
	{"no repositories configured", OnboardingKindNoRepos},
	{"no projects configured", OnboardingKindNoRepos},
	{"target misconfigured", OnboardingKindRepoTarget},
	{"expected owner/name", OnboardingKindRepoTarget},
	{"expected org/repo", OnboardingKindRepoTarget},
	{"primary_repo", OnboardingKindRepoTarget},
	{"no agents configured", OnboardingKindNoAgents},
	{"no agents defined", OnboardingKindNoAgents},
	// Generic GitHub App catch-all last: any remaining App-shaped message is
	// still a "Connect the GitHub App" problem for a new hive.
	{"github app", OnboardingKindGitHubAppNotInstalled},
}

// ClassifyOnboardingMessage maps a human-readable error/alert message to an
// onboarding kind and the Getting Started guide step that fixes it. ok is
// false for messages that are not setup problems (budget, CI, agent crashes).
// The message text itself is never altered — callers keep rendering it.
func ClassifyOnboardingMessage(msg string) (kind, guideStep string, ok bool) {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return "", "", false
	}
	for _, r := range onboardingMessageRules {
		if strings.Contains(m, r.needle) {
			return r.kind, onboardingKindGuideStep(r.kind), true
		}
	}
	return "", "", false
}

// onboardingKindForGitHubAppState maps a pkg/github AppAuthState wire token to
// the onboarding kind. Unknown/empty tokens yield "".
func onboardingKindForGitHubAppState(state string) string {
	switch state {
	case githubAppStateNotInstalledToken:
		return OnboardingKindGitHubAppNotInstalled
	case "wrong-installation":
		return OnboardingKindGitHubAppWrongInstallation
	case githubAppStateKeyMissingToken, githubAppStateKeyInvalidToken, githubAppStateNoAppAssignedToken:
		return OnboardingKindGitHubAppCredentials
	}
	return ""
}

// onboardingIssuesFor derives the onboarding problems in force from a status
// payload (after its GitHub App / repo-target fields are final) plus config.
// Pure: no locks, no I/O. Order is the guide's own order (GitHub App first).
func onboardingIssuesFor(status *StatusPayload, cfg *config.Config) []OnboardingIssue {
	if status == nil {
		return nil
	}
	var out []OnboardingIssue
	add := func(kind, msg string) {
		out = append(out, OnboardingIssue{Kind: kind, GuideStep: onboardingKindGuideStep(kind), Message: msg})
	}

	if status.GitHubAppRequired || status.GitHubAppInstallMissing {
		kind := onboardingKindForGitHubAppState(status.GitHubAppState)
		if kind == "" {
			if k, _, ok := ClassifyOnboardingMessage(status.GitHubAppPermIssue); ok && strings.HasPrefix(k, "github-app-") {
				kind = k
			} else if status.GitHubAppPermIssue != "" {
				kind = OnboardingKindGitHubAppPermissions
			} else {
				kind = OnboardingKindGitHubAppNotInstalled
			}
		}
		add(kind, status.GitHubAppPermIssue)
	}
	if status.RepoTargetMisconfigured {
		add(OnboardingKindRepoTarget, status.RepoTargetIssue)
	}
	if cfg != nil {
		if len(cfg.Project.Repos) == 0 && strings.TrimSpace(cfg.Project.PrimaryRepo) == "" {
			add(OnboardingKindNoRepos, "No projects configured — add at least one project and mark the primary")
		}
	}
	if cfg != nil && len(status.Agents) == 0 {
		add(OnboardingKindNoAgents, "No agents configured — add an agent so the hive can work")
	}
	return out
}

package dashboard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The dashboard must say "open the 🧭 Guide" itself whenever it shows an
// onboarding problem: a new GHE operator read "GitHub App 5686 has no
// installation for this org" and had to ask Slack for the install URL and
// where the guide was. These tests pin the server-side classification the
// SPA keys off (kind + guide step) and its presence on the status payload.

func TestClassifyOnboardingMessage(t *testing.T) {
	cases := []struct {
		msg      string
		wantKind string
		wantStep string
		wantOK   bool
	}{
		{"GitHub App permission issue: GitHub App 5686 has no installation for this org — install it (the spoke adopts the installation automatically)", OnboardingKindGitHubAppNotInstalled, GuideStepGitHubApp, true},
		{"GitHub App not installed — no installation for this org", OnboardingKindGitHubAppNotInstalled, GuideStepGitHubApp, true},
		{"GitHub App install pending — the App is not yet installed on the org", OnboardingKindGitHubAppNotInstalled, GuideStepGitHubApp, true},
		{"installation_id points at a different org than project.org", OnboardingKindGitHubAppWrongInstallation, GuideStepGitHubApp, true},
		{"GitHub App is installed but pending org approval", OnboardingKindGitHubAppPermissions, GuideStepGitHubApp, true},
		{"GitHub App permissions: missing `workflows` · 3 items blocked", OnboardingKindGitHubAppPermissions, GuideStepGitHubApp, true},
		{"Resource not accessible by integration", OnboardingKindGitHubAppPermissions, GuideStepGitHubApp, true},
		{"GitHub App credentials not delivered by the hub — agents cannot act as the App (operator action; no action needed from the hive owner)", OnboardingKindGitHubAppCredentials, GuideStepGitHubApp, true},
		{"GitHub App private key does not match the App it authenticates as — GitHub rejects its JWT (operator must push the correct key)", OnboardingKindGitHubAppCredentials, GuideStepGitHubApp, true},
		{"This hive still carries the placeholder github.app_id", OnboardingKindGitHubAppCredentials, GuideStepGitHubApp, true},
		{"github.base_url is not a valid URL for this enterprise host", OnboardingKindGitHubBaseURL, GuideStepGitHubApp, true},
		{"Project target misconfigured — expected owner/name. Fix in Settings → Projects.", OnboardingKindRepoTarget, GuideStepRepos, true},
		{"primary_repo \"hive\" is not one of project.repos", OnboardingKindRepoTarget, GuideStepRepos, true},
		{"No projects configured — add at least one project", OnboardingKindNoRepos, GuideStepRepos, true},
		{"No agents configured — add an agent", OnboardingKindNoAgents, GuideStepAddAgent, true},
		{"hub proxy token missing — dashboard links will not authenticate", OnboardingKindProxyToken, GuideStepWelcome, true},
		// Not onboarding: must stay untouched so budget/CI/crash alerts never
		// grow a misleading "open the guide" tip.
		{"Weekly token budget 95% consumed", "", "", false},
		{"agent scanner restarted 4 times in 10 minutes", "", "", false},
		{"CI failing on main for 3 hours", "", "", false},
		{"tmux not installed on this host", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		kind, step, ok := ClassifyOnboardingMessage(tc.msg)
		if ok != tc.wantOK || kind != tc.wantKind || step != tc.wantStep {
			t.Errorf("ClassifyOnboardingMessage(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.msg, kind, step, ok, tc.wantKind, tc.wantStep, tc.wantOK)
		}
	}
}

func TestOnboardingKindGuideStep_EveryKindHasAStep(t *testing.T) {
	for _, kind := range []string{
		OnboardingKindGitHubAppNotInstalled, OnboardingKindGitHubAppWrongInstallation,
		OnboardingKindGitHubAppPermissions, OnboardingKindGitHubAppCredentials,
		OnboardingKindGitHubBaseURL, OnboardingKindRepoTarget, OnboardingKindNoRepos,
		OnboardingKindNoAgents, OnboardingKindProxyToken,
	} {
		if onboardingKindGuideStep(kind) == "" {
			t.Errorf("kind %q has no guide step — the SPA tip would point nowhere", kind)
		}
	}
	if got := onboardingKindGuideStep("budget-exceeded"); got != "" {
		t.Errorf("unknown kind mapped to step %q, want none", got)
	}
}

func TestOnboardingIssuesFor_GitHubAppStates(t *testing.T) {
	cases := []struct {
		name   string
		status StatusPayload
		want   string
	}{
		{"not installed (config truth)", StatusPayload{GitHubAppInstallMissing: true, GitHubAppRequired: true, GitHubAppState: "not-installed"}, OnboardingKindGitHubAppNotInstalled},
		{"wrong installation", StatusPayload{GitHubAppRequired: true, GitHubAppState: "wrong-installation"}, OnboardingKindGitHubAppWrongInstallation},
		{"key missing", StatusPayload{GitHubAppRequired: true, GitHubAppState: "key-missing"}, OnboardingKindGitHubAppCredentials},
		{"key invalid", StatusPayload{GitHubAppRequired: true, GitHubAppState: "key-invalid"}, OnboardingKindGitHubAppCredentials},
		{"no app assigned", StatusPayload{GitHubAppRequired: true, GitHubAppState: "no-app-assigned"}, OnboardingKindGitHubAppCredentials},
		{"perm issue, unclassified state", StatusPayload{GitHubAppRequired: true, GitHubAppPermIssue: "GitHub App is installed but pending org approval"}, OnboardingKindGitHubAppPermissions},
		{"required with no detail", StatusPayload{GitHubAppRequired: true}, OnboardingKindGitHubAppNotInstalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.status
			st.Agents = []FrontendAgent{{Name: "scanner"}}
			got := onboardingIssuesFor(&st, nil)
			if len(got) != 1 {
				t.Fatalf("got %d issues, want 1: %+v", len(got), got)
			}
			if got[0].Kind != tc.want || got[0].GuideStep != GuideStepGitHubApp {
				t.Errorf("issue = %+v, want kind %q step %q", got[0], tc.want, GuideStepGitHubApp)
			}
			if got[0].Message != tc.status.GitHubAppPermIssue {
				t.Errorf("message %q altered; the server must keep the diagnostic text verbatim", got[0].Message)
			}
		})
	}
}

func TestOnboardingIssuesFor_NothingWhenHealthy(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Org = "hivecommons"
	cfg.Project.Repos = []string{"hive"}
	cfg.Project.PrimaryRepo = "hive"
	st := &StatusPayload{Agents: []FrontendAgent{{Name: "scanner"}}}
	if got := onboardingIssuesFor(st, cfg); len(got) != 0 {
		t.Errorf("healthy hive produced onboarding issues: %+v", got)
	}
	if got := onboardingIssuesFor(nil, cfg); got != nil {
		t.Errorf("nil status produced %+v", got)
	}
}

func TestOnboardingIssuesFor_RepoTargetNoReposNoAgents(t *testing.T) {
	cfg := &config.Config{}
	st := &StatusPayload{RepoTargetMisconfigured: true, RepoTargetIssue: "primary_repo must be org/repo"}
	got := onboardingIssuesFor(st, cfg)
	kinds := map[string]string{}
	for _, i := range got {
		kinds[i.Kind] = i.GuideStep
	}
	if kinds[OnboardingKindRepoTarget] != GuideStepRepos {
		t.Errorf("repo-target → step %q, want %q (issues: %+v)", kinds[OnboardingKindRepoTarget], GuideStepRepos, got)
	}
	if kinds[OnboardingKindNoRepos] != GuideStepRepos {
		t.Errorf("no-repos → step %q, want %q", kinds[OnboardingKindNoRepos], GuideStepRepos)
	}
	if kinds[OnboardingKindNoAgents] != GuideStepAddAgent {
		t.Errorf("no-agents → step %q, want %q", kinds[OnboardingKindNoAgents], GuideStepAddAgent)
	}
}

// The status payload (and therefore every SSE tick) must carry the
// classification for the exact GHE case from the report: real App ID,
// installation_id 0 — so the banner tip, the health pill, and the Guide chip
// all key off the same server truth.
func TestUpdateStatus_OnboardingIssuesOnPayload(t *testing.T) {
	s := newFullServer(t)
	s.deps.Config.GitHub = gheAppPostResetConfig()

	status := &StatusPayload{Agents: []FrontendAgent{{Name: "scanner"}}}
	s.UpdateStatus(status)

	var ghApp *OnboardingIssue
	for i := range status.OnboardingIssues {
		if status.OnboardingIssues[i].GuideStep == GuideStepGitHubApp {
			ghApp = &status.OnboardingIssues[i]
		}
	}
	if ghApp == nil {
		t.Fatalf("OnboardingIssues = %+v, want a github-app step entry for installation_id 0", status.OnboardingIssues)
	}
	if ghApp.Kind != OnboardingKindGitHubAppNotInstalled {
		t.Errorf("kind = %q, want %q", ghApp.Kind, OnboardingKindGitHubAppNotInstalled)
	}

	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"onboardingIssues":[`, `"kind":"github-app-not-installed"`, `"guideStep":"github-app"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("status JSON missing %s", want)
		}
	}

	// The embedded deep-health check carries the same marker so the header
	// pill tooltip can append the guide pointer.
	s.SetGitHubAppRequired(true)
	s.SetGitHubAppState("not-installed")
	health := s.healthSummaryFor(status, true)
	hraw, _ := json.Marshal(health)
	if !strings.Contains(string(hraw), `"name":"github_auth"`) || !strings.Contains(string(hraw), `"onboarding":true`) || !strings.Contains(string(hraw), `"guideStep":"github-app"`) {
		t.Errorf("deep health github_auth check lacks onboarding/guideStep: %s", hraw)
	}
}

func TestUpdateStatus_NoOnboardingIssuesWhenInstalled(t *testing.T) {
	s := newFullServer(t)
	g := gheAppPostResetConfig()
	g.InstallationID = 12345678
	s.deps.Config.GitHub = g
	s.deps.Config.Project.Org = "hivecommons"
	s.deps.Config.Project.Repos = []string{"hive"}
	s.deps.Config.Project.PrimaryRepo = "hive"

	status := &StatusPayload{Agents: []FrontendAgent{{Name: "scanner"}}}
	s.UpdateStatus(status)
	for _, i := range status.OnboardingIssues {
		if strings.HasPrefix(i.Kind, "github-app-") {
			t.Errorf("installed App still classified as %+v", i)
		}
	}
}

func TestAddSystemAlert_ClassifiesOnboarding(t *testing.T) {
	s := newTestServer()
	s.AddSystemAlert("gh-app", "error", "GitHub App permission issue: GitHub App 5686 has no installation for this org — install it")
	s.AddSystemAlert("budget", "warning", "Weekly token budget 95% consumed")

	s.systemAlertsMu.Lock()
	alerts := append([]SystemAlert(nil), s.systemAlerts...)
	s.systemAlertsMu.Unlock()
	byID := map[string]SystemAlert{}
	for _, a := range alerts {
		byID[a.ID] = a
	}
	if a := byID["gh-app"]; !a.Onboarding || a.GuideStep != GuideStepGitHubApp {
		t.Errorf("gh-app alert = %+v, want onboarding=true guideStep=%q", a, GuideStepGitHubApp)
	}
	if a := byID["budget"]; a.Onboarding || a.GuideStep != "" {
		t.Errorf("budget alert = %+v, must not be classified as onboarding", a)
	}
	// Updating an existing alert re-classifies it from the new message.
	s.AddSystemAlert("gh-app", "info", "all good now")
	s.systemAlertsMu.Lock()
	defer s.systemAlertsMu.Unlock()
	for _, a := range s.systemAlerts {
		if a.ID == "gh-app" && (a.Onboarding || a.GuideStep != "") {
			t.Errorf("updated alert kept stale onboarding marker: %+v", a)
		}
	}
}

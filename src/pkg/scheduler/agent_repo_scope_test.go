package scheduler

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func scopeScheduler(t *testing.T, agents map[string]config.AgentConfig) *Scheduler {
	t.Helper()
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:         "my-org",
			Repos:       []string{"console", "dashboard", "docs"},
			PrimaryRepo: "console",
		},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: agents,
		},
	}
	return New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// An unscoped agent — every agent on a hive that does not use the feature —
// must get the section it always got.
func TestBuildReposSectionFor_UnscopedAgentUnchanged(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
	section := s.buildReposSectionFor("scanner")

	if strings.Contains(section, "REPO-SCOPED") {
		t.Errorf("scope note emitted for an unscoped agent:\n%s", section)
	}
	for _, want := range []string{"my-org/console", "my-org/dashboard", "my-org/docs"} {
		if !strings.Contains(section, want) {
			t.Errorf("repo %q missing:\n%s", want, section)
		}
	}
	if !strings.Contains(section, "this project has 3 authorized repos") {
		t.Errorf("rotation note should count all three:\n%s", section)
	}
	// The no-agent form is the same thing, so pre-existing callers are safe.
	if s.buildReposSection() != section {
		t.Error("buildReposSection() drifted from buildReposSectionFor(\"\")")
	}
}

// A scoped agent sees its own repos, and is TOLD it is scoped: an agent that
// has filed issues in a repo for weeks and suddenly does not see it would
// otherwise read the shorter list as scope loss and file a finding about it.
func TestBuildReposSectionFor_ScopedAgent(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{
		"schema": {Backend: "claude", Repos: []string{"dashboard"}},
	})
	section := s.buildReposSectionFor("schema")

	if !strings.Contains(section, "my-org/dashboard") {
		t.Errorf("agent's own repo missing:\n%s", section)
	}
	for _, unwanted := range []string{"my-org/console", "my-org/docs"} {
		if strings.Contains(section, unwanted) {
			t.Errorf("out-of-scope repo %q offered to a scoped agent:\n%s", unwanted, section)
		}
	}
	if !strings.Contains(section, "REPO-SCOPED") {
		t.Errorf("scoped agent not told it is scoped:\n%s", section)
	}
	if !strings.Contains(section, "NOT scope loss") {
		t.Errorf("scope note does not tell the agent this is deliberate:\n%s", section)
	}
	// One repo left: nothing to rotate between.
	if strings.Contains(section, "MULTI-REPO COVERAGE") {
		t.Errorf("rotation instruction emitted for a single-repo agent:\n%s", section)
	}
}

// The rotation instruction counts the AGENT's repos and names the AGENT's
// primary: pointing a specialist at a repo it may not write to is a
// contradiction it will try to resolve by writing there.
func TestBuildReposSectionFor_RotationUsesAgentReposAndPrimary(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{
		"schema": {Backend: "claude", Repos: []string{"dashboard", "docs"}},
	})
	section := s.buildReposSectionFor("schema")

	if !strings.Contains(section, "this project has 2 authorized repos") {
		t.Errorf("rotation note should count the agent's repos:\n%s", section)
	}
	if strings.Contains(section, "not just the primary (my-org/console)") {
		t.Errorf("rotation note named the hive primary, which this agent cannot write to:\n%s", section)
	}
	if !strings.Contains(section, "not just the primary (my-org/dashboard)") {
		t.Errorf("rotation note should name the agent's own primary:\n%s", section)
	}
}

// A scope that matches nothing is a misconfiguration, and the kick must say so
// rather than present an empty list the agent has to interpret.
func TestBuildReposSectionFor_ScopeMatchesNothing(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{
		"lost": {Backend: "claude", Repos: []string{"not-watched"}},
	})
	section := s.buildReposSectionFor("lost")
	if !strings.Contains(section, "scoped to repos this hive does not watch") {
		t.Errorf("unmatched scope not stated:\n%s", section)
	}
}

// Task filtering is the cost half of the feature: a scoped agent still wakes on
// its cadence, but it wakes to its own repos' work.
func TestSubstituteTemplate_FiltersIssuesToAgentRepos(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{
		"schema":  {Backend: "claude", Repos: []string{"dashboard"}},
		"scanner": {Backend: "claude"},
	})
	issues := []github.Issue{
		{Repo: "console", Number: 1, Title: "console thing", URL: "u1"},
		{Repo: "dashboard", Number: 2, Title: "dashboard thing", URL: "u2"},
	}
	actionable := &github.ActionableResult{Issues: github.IssueResultFromItems(issues)}

	scoped, _ := s.substituteTemplateWithPolicy("${ISSUE_LIST}|${QUEUE_ISSUES}|${PROJECT_REPOS_LIST}|${PROJECT_PRIMARY_REPO}", actionable, "schema", issues)
	if strings.Contains(scoped, "console thing") {
		t.Errorf("a scoped agent was shown an out-of-scope issue:\n%s", scoped)
	}
	if !strings.Contains(scoped, "dashboard thing") {
		t.Errorf("a scoped agent lost its own issue:\n%s", scoped)
	}
	if !strings.Contains(scoped, "|1|") {
		t.Errorf("QUEUE_ISSUES was not recounted for the scoped agent:\n%s", scoped)
	}
	if !strings.Contains(scoped, "|dashboard|") {
		t.Errorf("PROJECT_REPOS_LIST should be the agent's repos:\n%s", scoped)
	}
	if !strings.Contains(scoped, "my-org/dashboard") {
		t.Errorf("PROJECT_PRIMARY_REPO should be the agent's primary:\n%s", scoped)
	}

	// The unscoped agent still sees everything, from the SAME actionable value:
	// filtering must not mutate the fleet-wide input.
	unscoped, _ := s.substituteTemplateWithPolicy("${ISSUE_LIST}|${QUEUE_ISSUES}|${PROJECT_REPOS_LIST}", actionable, "scanner", issues)
	if !strings.Contains(unscoped, "console thing") || !strings.Contains(unscoped, "dashboard thing") {
		t.Errorf("unscoped agent lost issues, so the shared input was mutated:\n%s", unscoped)
	}
	if !strings.Contains(unscoped, "|2|") {
		t.Errorf("unscoped QUEUE_ISSUES = wrong:\n%s", unscoped)
	}
}

// An unscoped red-team lane (hivecommons/hive#17420) is handed no repository
// and no work, and is told why: every write it makes is refused, so the kick
// must not present repos or a queue it could act on.
func TestKickForUnscopedRedTeamOffersNothing(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{
		"redteam": {Backend: "claude", Role: config.RedTeamRole},
	})
	section := s.buildReposSectionFor("redteam")
	for _, unwanted := range []string{"my-org/console", "my-org/dashboard", "my-org/docs", "currently paused"} {
		if strings.Contains(section, unwanted) {
			t.Errorf("unscoped red-team kick offered %q:\n%s", unwanted, section)
		}
	}
	if !strings.Contains(section, "serves no repository until the operator sets its repos: scope") {
		t.Errorf("unscoped red-team kick does not say why it has no repos:\n%s", section)
	}

	issues := []github.Issue{
		{Repo: "console", Number: 1, Title: "console thing", URL: "u1"},
		{Repo: "dashboard", Number: 2, Title: "dashboard thing", URL: "u2"},
	}
	actionable := &github.ActionableResult{Issues: github.IssueResultFromItems(issues)}
	got, _ := s.substituteTemplateWithPolicy("${ISSUE_LIST}|${QUEUE_ISSUES}|${PROJECT_REPOS_LIST}|", actionable, "redteam", issues)
	if strings.Contains(got, "console thing") || strings.Contains(got, "dashboard thing") {
		t.Errorf("unscoped red-team kick listed work:\n%s", got)
	}
	if !strings.Contains(got, "|0|") || !strings.HasSuffix(got, "||") {
		t.Errorf("unscoped red-team kick should count no queue and list no repos:\n%s", got)
	}
}

func TestFormatMergeEligibleDataFor_FiltersByRepo(t *testing.T) {
	data := []byte(`{"merge_eligible":[{"number":1,"repo":"console","title":"a"},{"number":2,"repo":"dashboard","title":"b"}]}`)

	all := formatMergeEligibleData(data, 0)
	if !strings.Contains(all, "console") || !strings.Contains(all, "dashboard") {
		t.Errorf("unfiltered list lost an entry: %q", all)
	}
	only := formatMergeEligibleDataFor(data, func(repo string) bool { return repo == "dashboard" }, 0)
	if strings.Contains(only, "console") || !strings.Contains(only, "dashboard") {
		t.Errorf("filtered list = %q, want only dashboard", only)
	}
	// Everything filtered out reads as "(none)", not as an empty block.
	none := formatMergeEligibleDataFor(data, func(string) bool { return false }, 0)
	if strings.TrimSpace(none) != "(none)" {
		t.Errorf("fully filtered list = %q, want (none)", none)
	}
}

func TestFormatMergeEligibleDataFor_SeparatesMaintainerMerges(t *testing.T) {
	data := []byte(`{"merge_eligible":[{"number":1,"repo":"console","title":"ready"}],"needs_maintainer_merge":[{"number":2,"repo":"dashboard","title":"approved contributor PR","reason":"contributor-lane:no-claim"},{"number":3,"repo":"console","title":"untrusted tier","reason":"contributor-lane:claim-tier-untrusted"},{"number":4,"repo":"console","title":"another trust hold","reason":"contributor-lane:no-claim"}]}`)
	out := formatMergeEligibleData(data, 0)
	eligible, maintainer, ok := strings.Cut(out, "NEEDS MAINTAINER MERGE")
	if !ok {
		t.Fatalf("missing maintainer section: %s", out)
	}
	if !strings.Contains(eligible, "#1 console") || strings.Contains(eligible, "#2") || strings.Contains(eligible, "#3") {
		t.Errorf("eligible section includes refused PRs or loses ready PR: %s", eligible)
	}
	for _, want := range []string{"#2 dashboard", "#3 console", "#4 console", "contributor-lane:no-claim", "contributor-lane:claim-tier-untrusted", "do not request hive-merge", "merge manually", "registry tier", "verified hive claim"} {
		if !strings.Contains(maintainer, want) {
			t.Errorf("maintainer section missing %q: %s", want, maintainer)
		}
	}
	// Filtering applies independently to both sections; a scoped agent can
	// see maintainer work even when it has no eligible PRs.
	only := formatMergeEligibleDataFor(data, func(repo string) bool { return repo == "dashboard" }, 0)
	if !strings.HasPrefix(only, "(none)\n") || !strings.Contains(only, "#2 dashboard") || strings.Contains(only, "console") {
		t.Errorf("dashboard-only list = %q, want only its maintainer row", only)
	}
	console := formatMergeEligibleDataFor(data, func(repo string) bool { return repo == "console" }, 1)
	if !strings.Contains(console, "#3 console") || strings.Contains(console, "#4 console") || !strings.Contains(console, "1 more") {
		t.Errorf("maintainer list did not respect the cap after repo filtering: %s", console)
	}
	if none := formatMergeEligibleDataFor(data, func(string) bool { return false }, 0); none != "(none)\n" {
		t.Errorf("fully filtered list = %q, want (none)", none)
	}
	legacy := formatMergeEligibleData([]byte(`{"merge_eligible":[{"number":1,"repo":"console","title":"ready"}]}`), 0)
	if strings.Contains(legacy, "NEEDS MAINTAINER MERGE") {
		t.Errorf("legacy payload gained an empty maintainer section: %s", legacy)
	}
}

func TestFilterIssuesForRepos(t *testing.T) {
	issues := []github.Issue{{Repo: "a"}, {Repo: "b"}}
	if got := filterIssuesForRepos(issues, nil); len(got) != 2 {
		t.Errorf("nil predicate should pass everything, got %v", got)
	}
	got := filterIssuesForRepos(issues, func(r string) bool { return r == "b" })
	if len(got) != 1 || got[0].Repo != "b" {
		t.Errorf("filterIssuesForRepos = %v, want the b issue", got)
	}
}

// project.reference_repos (hivecommons/hive#18015): the kick names each
// reference repo with its tier and tells the agent an item whose deliverables
// mention one is in scope, so the scanner stops classifying it out-of-scope.
func TestBuildReposSectionFor_ReferenceRepos(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
	s.cfg.Project.ReferenceRepos = []config.ReferenceRepo{
		{Repo: "hivecommons/docs"},
		{Repo: "hivecommons/hive", Write: []string{"issues", "prs"}},
	}
	section := s.buildReposSectionFor("scanner")
	for _, want := range []string{
		"REFERENCE REPOS",
		"https://github.com/hivecommons/docs  [reference · read-only]",
		"https://github.com/hivecommons/hive  [reference · issues+prs]",
		"IN SCOPE, not a scope violation",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("%q missing:\n%s", want, section)
		}
	}
	// A hive without the key sees no trace of it.
	s.cfg.Project.ReferenceRepos = nil
	if strings.Contains(s.buildReposSectionFor("scanner"), "REFERENCE REPOS") {
		t.Error("reference block emitted with no reference repos configured")
	}
}

// referenceReposGolden is the exact 📚 REFERENCE REPOS block for one
// read-only entry and one issues+prs entry (hivecommons/hive#18020). It is a
// golden on purpose: this wording is the only thing that tells the scanner an
// item citing a reference repo is in scope, so a change to it must be a
// deliberate edit here, not a side effect.
const referenceReposGolden = "📚 REFERENCE REPOS (outside this project — study them, do not work them):\n" +
	"  https://github.com/hivecommons/docs  [reference · read-only]\n" +
	"  https://github.com/hivecommons/hive  [reference · issues+prs]\n" +
	"   You MAY clone them (public, unauthenticated), read their files, and GET their issues/PRs/API. " +
	"An authorized-repo item whose deliverables name a reference repo is IN SCOPE, not a scope violation: " +
	"do NOT classify it out-of-scope, critical, or a security concern. " +
	"Findings about a reference repo belong in the deliverable inside the AUTHORIZED repo (the assessment document, the report), " +
	"never as issues or PRs on the reference repo itself — unless its tier above says issues/prs, in which case you may ONLY open an issue, " +
	"open a PR from your own branch, or comment on an issue/PR you opened; the proxy refuses merges, default-branch pushes, labels, assignments, " +
	"reviews and closing others' items deterministically, so retrying cannot succeed.\n"

func TestBuildReposSectionFor_ReferenceReposGolden(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
	s.cfg.Project.ReferenceRepos = []config.ReferenceRepo{
		{Repo: "hivecommons/docs"},
		{Repo: "hivecommons/hive", Write: []string{"issues", "prs"}},
	}
	section := s.buildReposSectionFor("scanner")
	start := strings.Index(section, "📚 REFERENCE REPOS")
	end := strings.Index(section, "⛔ NEVER access")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("reference block must precede the ⛔ scope line (start=%d end=%d):\n%s", start, end, section)
	}
	if got := section[start:end]; got != referenceReposGolden {
		t.Errorf("reference block drifted from the golden:\n--- got\n%s\n--- want\n%s", got, referenceReposGolden)
	}
	// It belongs to the scope list, after the authorized repos: "repos not
	// listed above" must include the reference repos.
	if auth := strings.Index(section, "AUTHORIZED REPOS"); auth < 0 || auth > start {
		t.Errorf("reference block must follow AUTHORIZED REPOS:\n%s", section)
	}
}

// Each tier renders on its own: a read-only-only hive never advertises a
// write tier, and an issues-only or prs-only entry prints exactly that tier.
func TestBuildReposSectionFor_ReferenceRepoTiers(t *testing.T) {
	for _, tc := range []struct {
		write []string
		label string
	}{
		{nil, "read-only"},
		{[]string{"issues"}, "issues"},
		{[]string{"prs"}, "prs"},
		{[]string{"prs", "issues"}, "issues+prs"},
	} {
		s := scopeScheduler(t, map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
		s.cfg.Project.ReferenceRepos = []config.ReferenceRepo{{Repo: "cncf/landscape", Write: tc.write}}
		section := s.buildReposSectionFor("scanner")
		want := "  https://github.com/cncf/landscape  [reference · " + tc.label + "]\n"
		if !strings.Contains(section, want) {
			t.Errorf("write=%v: %q missing:\n%s", tc.write, want, section)
		}
		if !strings.Contains(section, "IN SCOPE, not a scope violation") {
			t.Errorf("write=%v: in-scope rule missing:\n%s", tc.write, section)
		}
	}
}

// The cncf/techdocs#395 shape: the hive's only authorized repo is the docs
// repo, and the item's deliverables cite reference repos only. The kick the
// scanner reads lists those repos inside the scope block and tells it the
// item is in scope — the scope classification is the scanner's, driven by
// this text, so this is the classifier contract.
func TestBuildReposSectionFor_ItemCitingOnlyReferenceReposIsInScope(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
	s.cfg.Project.Org = "cncf"
	s.cfg.Project.Repos = []string{"techdocs"}
	s.cfg.Project.PrimaryRepo = "techdocs"
	s.cfg.Project.ReferenceRepos = []config.ReferenceRepo{
		{Repo: "hivecommons/docs"},
		{Repo: "hivecommons/hivecommons.github.io"},
		{Repo: "hivecommons/hive"},
	}
	section := s.buildReposSectionFor("scanner")
	never := strings.Index(section, "⛔ NEVER access")
	if never < 0 {
		t.Fatalf("scope line missing:\n%s", section)
	}
	for _, cited := range []string{"hivecommons/docs", "hivecommons/hivecommons.github.io", "hivecommons/hive"} {
		at := strings.Index(section, "https://github.com/"+cited+"  [reference · read-only]")
		if at < 0 || at > never {
			t.Errorf("%s must be listed above the ⛔ scope line so it is not 'a repo not listed above':\n%s", cited, section)
		}
	}
	for _, want := range []string{
		"https://github.com/cncf/techdocs\n",
		"do NOT classify it out-of-scope, critical, or a security concern",
		"Findings about a reference repo belong in the deliverable inside the AUTHORIZED repo",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("%q missing:\n%s", want, section)
		}
	}
}

// hivecommons/hive#18159: a derived entry says which request issue granted
// it, and a derived entry served read-only says why, so an agent can tell a
// grant from an operator's decision.
func TestBuildReposSectionFor_ReferenceReposDerivedProvenance(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
	s.cfg.Project.ReferenceRepos = []config.ReferenceRepo{
		{Repo: "hivecommons/docs"},
		{Repo: "acme/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395", CredentialState: config.ReferenceCredentialWriteTier},
		{Repo: "acme/site", DerivedFrom: "cncf/techdocs#395", CredentialState: config.ReferenceCredentialReadOnly, CredentialReason: "the hive's GitHub App is not installed on acme"},
	}
	section := s.buildReposSectionFor("scanner")
	for _, want := range []string{
		"  https://github.com/hivecommons/docs  [reference · read-only]\n",
		"  https://github.com/acme/docs  [reference · issues+prs] (from cncf/techdocs#395)\n",
		"  https://github.com/acme/site  [reference · read-only] (from cncf/techdocs#395; read-only: the hive's GitHub App is not installed on acme)\n",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("REFERENCE REPOS block lacks %q:\n%s", want, section)
		}
	}
	if got := referenceRepoProvenance(config.ReferenceRepo{Repo: "x/y", DerivedFrom: "o/r#1", CredentialState: config.ReferenceCredentialReadOnly}); !strings.Contains(got, derivedReadOnlyNoReason) {
		t.Errorf("read-only without a reason must fall back to the generic one, got %q", got)
	}
}

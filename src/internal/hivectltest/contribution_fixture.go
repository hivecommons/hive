package hivectltest

// Shared state-transition fixture for the staged contribution tree. The staging
// layer's own tests and every tree surface (Web, TUI) start from it, so a
// surface's diffs and receipts can be compared with the shared layer's for the
// same edits. It holds placeholder credentials only, and is never read by
// production code paths. It lives outside the hivectl package so it is not
// compiled into the shipped binary. Its internal home keeps this test-only API
// private to the module.
import "github.com/hivecommons/hive/pkg/hivectl"

const (
	ContributionFixtureRepoA  = "github/github.com/org/a"
	ContributionFixtureRepoB  = "github/github.com/org/b"
	ContributionFixtureHubOne = "https://one.example"
	ContributionFixtureHubTwo = "https://two.example"
)

// ContributionStageFixture returns two scoped subscriptions ("alpha", shared
// across hubs one and two; "beta", on hub one only), both with complete
// discovery of repositories a and b and nothing selected. Each call returns
// fresh values the caller may modify.
func ContributionStageFixture() (*hivectl.ProfileSet, []hivectl.TreeHubSnapshot, map[string]hivectl.TreeRepositorySnapshot) {
	set := &hivectl.ProfileSet{Version: hivectl.ProfilesVersion, Active: "alpha", Profiles: []hivectl.Profile{
		{Name: "alpha", Hub: "wss://alpha.example/contribute?token=endpoint-secret", RegistrationToken: "registration-secret", Selection: &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected}},
		{Name: "beta", Hub: "wss://beta.example/contribute", RegistrationToken: "other-secret", Selection: &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected}},
	}, Hubs: []hivectl.HubEntry{
		{Name: "one", URL: ContributionFixtureHubOne, Follow: hivectl.HubFollowCurrentAndFutureRepos},
		{Name: "two", URL: ContributionFixtureHubTwo, Follow: hivectl.HubFollowCurrent},
	}}
	hubs := []hivectl.TreeHubSnapshot{
		{Name: "one", URL: ContributionFixtureHubOne, Discovery: hivectl.TreeComplete, HiveOrigins: []string{"https://alpha.example", "https://beta.example"}},
		{Name: "two", URL: ContributionFixtureHubTwo, Discovery: hivectl.TreeComplete, HiveOrigins: []string{"https://alpha.example"}},
	}
	repos := map[string]hivectl.TreeRepositorySnapshot{
		"alpha": {Discovery: hivectl.TreeComplete, Repositories: []string{ContributionFixtureRepoA, ContributionFixtureRepoB}},
		"beta":  {Discovery: hivectl.TreeComplete, Repositories: []string{ContributionFixtureRepoA, ContributionFixtureRepoB}},
	}
	return set, hubs, repos
}

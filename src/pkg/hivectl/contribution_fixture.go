package hivectl

// Shared state-transition fixture for the staged contribution tree. The staging
// layer's own tests and every tree surface (Web, TUI) start from it, so a
// surface's diffs and receipts can be compared with the shared layer's for the
// same edits. It holds placeholder credentials only, and is never read by
// production code paths.
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
func ContributionStageFixture() (*ProfileSet, []TreeHubSnapshot, map[string]TreeRepositorySnapshot) {
	set := &ProfileSet{Version: ProfilesVersion, Active: "alpha", Profiles: []Profile{
		{Name: "alpha", Hub: "wss://alpha.example/contribute?token=endpoint-secret", RegistrationToken: "registration-secret", Selection: &RepositorySelection{Mode: SelectionSelected}},
		{Name: "beta", Hub: "wss://beta.example/contribute", RegistrationToken: "other-secret", Selection: &RepositorySelection{Mode: SelectionSelected}},
	}, Hubs: []HubEntry{
		{Name: "one", URL: ContributionFixtureHubOne, Follow: HubFollowCurrentAndFutureRepos},
		{Name: "two", URL: ContributionFixtureHubTwo, Follow: HubFollowCurrent},
	}}
	hubs := []TreeHubSnapshot{
		{Name: "one", URL: ContributionFixtureHubOne, Discovery: TreeComplete, HiveOrigins: []string{"https://alpha.example", "https://beta.example"}},
		{Name: "two", URL: ContributionFixtureHubTwo, Discovery: TreeComplete, HiveOrigins: []string{"https://alpha.example"}},
	}
	repos := map[string]TreeRepositorySnapshot{
		"alpha": {Discovery: TreeComplete, Repositories: []string{ContributionFixtureRepoA, ContributionFixtureRepoB}},
		"beta":  {Discovery: TreeComplete, Repositories: []string{ContributionFixtureRepoA, ContributionFixtureRepoB}},
	}
	return set, hubs, repos
}

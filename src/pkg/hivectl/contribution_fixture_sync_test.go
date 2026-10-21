package hivectl_test

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/hivectltest"
)

// TestStageFixtureMatchesHivectltest guards the hand-copied fixture in
// contribution_stage_test.go against drifting from the shared
// hivectltest.ContributionStageFixture that every tree surface's tests use.
// The external test package can import hivectltest without an import cycle.
func TestStageFixtureMatchesHivectltest(t *testing.T) {
	localSet, localHubs, localRepos := hivectl.StageFixtureDataForTest()
	sharedSet, sharedHubs, sharedRepos := hivectltest.ContributionStageFixture()

	if !reflect.DeepEqual(localSet, sharedSet) {
		t.Errorf("profile set drifted from hivectltest.ContributionStageFixture:\n local: %+v\nshared: %+v", localSet, sharedSet)
	}
	if !reflect.DeepEqual(localHubs, sharedHubs) {
		t.Errorf("hub snapshots drifted from hivectltest.ContributionStageFixture:\n local: %+v\nshared: %+v", localHubs, sharedHubs)
	}
	if !reflect.DeepEqual(localRepos, sharedRepos) {
		t.Errorf("repository snapshots drifted from hivectltest.ContributionStageFixture:\n local: %+v\nshared: %+v", localRepos, sharedRepos)
	}
}

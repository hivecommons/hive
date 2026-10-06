package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// configureGitHubClient installs the live merge strategy and the lane gate
// (#10889): direct by default, hive-serialized once the repo policy says so,
// with no rebuild.
func TestConfigureGitHubClientInstallsSerializedLane(t *testing.T) {
	cfg := rebuildTestConfig(false)
	b, _ := newDepsTestBoot(t, cfg)
	client := fakeGitHubClient(t)
	b.configureGitHubClient(client)

	strategy, gate := client.SerializedLane()
	if strategy == nil || gate == nil {
		t.Fatal("configureGitHubClient did not install the serialized lane")
	}
	if github.SerializedStrategy(strategy, rebuildTestOpenRepo) {
		t.Fatal("a repo without merge_strategy must stay direct")
	}
	cfg.Project.RepoPolicies = append(cfg.Project.RepoPolicies, config.RepoPolicy{Repo: rebuildTestOpenRepo, MergeStrategy: config.MergeStrategyHiveSerialized})
	if !github.SerializedStrategy(strategy, rebuildTestOpenRepo) {
		t.Fatal("the strategy must follow the live config")
	}
}

// A lane that cannot be built refuses the merge with the cause; it never
// falls back to a direct merge.
func TestSerializedLaneGateRefusesWhenLaneUnavailable(t *testing.T) {
	b, _ := newDepsTestBoot(t, rebuildTestConfig(false))
	b.mergeLaneOnce.Do(func() { b.mergeLaneErr = errors.New("lane store unreadable") })

	res, err := b.serializedLaneGate(context.Background(), github.LaneMergeRequest{Repo: rebuildTestOpenRepo, Number: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged() || res.Outcome != github.LaneOutcomeRefused || !strings.HasPrefix(res.Reason, github.ReasonLaneUnavailable) || !strings.Contains(res.Reason, "lane store unreadable") {
		t.Fatalf("result = %+v, want a refusal naming the cause", res)
	}
}

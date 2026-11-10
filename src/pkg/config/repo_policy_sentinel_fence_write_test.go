package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func repoOverrideWriteConfig(t *testing.T) *Config {
	t.Helper()
	return loadRepoPolicySentinelFenceYAML(t, `
project:
  org: acme
  repos: [a, b, c, d]
  repo_policies:
    - repo: a
      auto_merge: false
      merge_strategy: hive-serialized
    - repo: d
      forge_url: https://git.example.com
sentinel:
  sensitive_paths: ["OWNERS", "Justfile"]
auto_merge:
  contributor_lane:
    workflow_fence_scope: all
`)
}

func repoOverrideStrPtr(s string) *string { return &s }

// An owner sets and clears one repository's workflow fence scope; the other
// fields of its policy entry, other repositories and the hive-wide value stay
// as they were. On v6 tip there is no setter, so this does not compile.
func TestSetRepoWorkflowFenceScopesSetsAndClearsOneRepository(t *testing.T) {
	cfg := repoOverrideWriteConfig(t)
	changes, err := cfg.SetRepoWorkflowFenceScopes(map[string]*string{"a": repoOverrideStrPtr("Human_Merge_Paths"), "acme/b": repoOverrideStrPtr("all")})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := strings.Join(changes, "; "); got != "acme/a: hive-wide → human_merge_paths; acme/b: hive-wide → all" {
		t.Fatalf("changes = %q", got)
	}
	if scope, override := cfg.RepoWorkflowFenceScope("a"); scope != ContributorLaneWorkflowFenceScopeHumanMergePaths || !override {
		t.Fatalf("a = %q, %v; want its own human_merge_paths", scope, override)
	}
	rp, _ := cfg.RepoPolicyFor("a")
	if rp.AutoMerge == nil || *rp.AutoMerge || rp.MergeStrategy != MergeStrategyHiveSerialized {
		t.Fatalf("setting the scope changed a's other fields: %+v", rp)
	}
	if scope, override := cfg.RepoWorkflowFenceScope("c"); scope != ContributorLaneWorkflowFenceScopeAll || override {
		t.Fatalf("c = %q, %v; want the hive-wide all", scope, override)
	}
	if cfg.AutoMerge.ContributorLane.WorkflowFenceScope != ContributorLaneWorkflowFenceScopeAll {
		t.Fatalf("hive-wide scope changed to %q", cfg.AutoMerge.ContributorLane.WorkflowFenceScope)
	}

	// Clearing returns a to the hive-wide value and keeps its entry, which
	// still holds other overrides; clearing b prunes its now-empty entry.
	if _, err := cfg.SetRepoWorkflowFenceScopes(map[string]*string{"acme/a": nil, "b": nil}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, override := cfg.RepoWorkflowFenceScope("a"); override {
		t.Fatal("a still has an override after clearing")
	}
	if rp, ok := cfg.RepoPolicyFor("a"); !ok || rp.MergeStrategy != MergeStrategyHiveSerialized {
		t.Fatalf("clearing a's scope dropped its entry: %+v, %v", rp, ok)
	}
	if _, ok := cfg.RepoPolicyFor("b"); ok {
		t.Fatal("b's entry held only the scope and should be pruned")
	}
	// Clearing what is not set is no change.
	if changes, err := cfg.SetRepoWorkflowFenceScopes(map[string]*string{"c": nil}); err != nil || len(changes) != 0 {
		t.Fatalf("clearing an unset scope = %v, %v; want no change", changes, err)
	}

	// An invalid value refuses the batch before anything changes.
	before := append([]RepoPolicy(nil), cfg.Project.RepoPolicies...)
	if _, err := cfg.SetRepoWorkflowFenceScopes(map[string]*string{"c": repoOverrideStrPtr("all"), "a": repoOverrideStrPtr("release_only")}); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
	if len(cfg.Project.RepoPolicies) != len(before) {
		t.Fatalf("a refused batch changed the policies: %+v", cfg.Project.RepoPolicies)
	}
	if _, override := cfg.RepoWorkflowFenceScope("c"); override {
		t.Fatal("a refused batch still set c")
	}
}

// An owner sets, empties and clears one repository's Sentinel list. On v6 tip
// there is no setter, so this does not compile.
func TestSetRepoSentinelSensitivePathsSetsAndClearsOneRepository(t *testing.T) {
	cfg := repoOverrideWriteConfig(t)
	own := []string{" OWNERS ", "OWNERS", ".github/workflows/**"}
	none := []string{}
	if _, err := cfg.SetRepoSentinelSensitivePaths(map[string]*[]string{"a": &own, "c": &none}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, override := cfg.RepoSentinelSensitivePaths("a"); !override || strings.Join(got, ",") != "OWNERS,.github/workflows/**" {
		t.Fatalf("a = %v, %v; want its own trimmed, de-duplicated list", got, override)
	}
	if got, override := cfg.RepoSentinelSensitivePaths("c"); !override || got == nil || len(got) != 0 {
		t.Fatalf("c = %#v, %v; want an explicit empty list that flags nothing", got, override)
	}
	if got, override := cfg.RepoSentinelSensitivePaths("b"); override || strings.Join(got, ",") != "OWNERS,Justfile" {
		t.Fatalf("b = %v, %v; want the hive-wide list", got, override)
	}
	if strings.Join(cfg.Sentinel.SensitivePaths, ",") != "OWNERS,Justfile" {
		t.Fatalf("hive-wide list changed: %v", cfg.Sentinel.SensitivePaths)
	}
	if rp, _ := cfg.RepoPolicyFor("a"); rp.AutoMerge == nil || rp.MergeStrategy != MergeStrategyHiveSerialized {
		t.Fatalf("setting the list changed a's other fields: %+v", rp)
	}

	// Blank and malformed patterns are refused, and nothing changes.
	for _, bad := range [][]string{{"OWNERS", "  "}, {"[unclosed"}} {
		list := bad
		if _, err := cfg.SetRepoSentinelSensitivePaths(map[string]*[]string{"b": &list}); err == nil {
			t.Fatalf("pattern list %q was accepted", bad)
		}
		if _, override := cfg.RepoSentinelSensitivePaths("b"); override {
			t.Fatalf("refused list %q still set b", bad)
		}
	}

	// d's entry carries only forge settings; adding and then clearing a list
	// must leave those in place.
	if _, err := cfg.SetRepoSentinelSensitivePaths(map[string]*[]string{"d": &own}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.SetRepoSentinelSensitivePaths(map[string]*[]string{"a": nil, "c": nil, "d": nil}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if rp, ok := cfg.RepoPolicyFor("d"); !ok || rp.ForgeURL != "https://git.example.com" || rp.SentinelSensitivePaths != nil {
		t.Fatalf("clearing d's list lost its forge_url: %+v, %v", rp, ok)
	}
	if _, ok := cfg.RepoPolicyFor("c"); ok {
		t.Fatal("c's entry held only the list and should be pruned")
	}
	if _, override := cfg.RepoSentinelSensitivePaths("a"); override {
		t.Fatal("a still has its own list after clearing")
	}
}

// An explicit empty list must survive a save and reload: omitempty dropped it,
// so on v6 tip a saved "flag nothing by path" came back as the hive-wide list.
func TestRepoPolicyEmptySentinelPathsSurviveYAMLRoundTrip(t *testing.T) {
	in := []RepoPolicy{
		{Repo: "a", SentinelSensitivePaths: []string{}, WorkflowFenceScope: "human_merge_paths"},
		{Repo: "b", AutoMerge: new(bool)},
		{Repo: "c", SentinelSensitivePaths: []string{"OWNERS"}},
	}
	data, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out []RepoPolicy
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	if len(out) != 3 {
		t.Fatalf("round trip = %+v", out)
	}
	if out[0].SentinelSensitivePaths == nil || len(out[0].SentinelSensitivePaths) != 0 || out[0].WorkflowFenceScope != "human_merge_paths" {
		t.Fatalf("a after round trip = %+v; want an explicit empty list and its scope\n%s", out[0], data)
	}
	if out[1].SentinelSensitivePaths != nil || strings.Count(string(data), "sentinel_sensitive_paths") != 2 {
		t.Fatalf("an unset list must stay unset:\n%s", data)
	}
	if strings.Join(out[2].SentinelSensitivePaths, ",") != "OWNERS" {
		t.Fatalf("c after round trip = %+v", out[2])
	}
}

package github

import (
	"os"
	"path"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const v2TestsWorkflowPath = "../../../.github/workflows/v2-tests.yml"

type workflowPathTriggers struct {
	On struct {
		PullRequest struct {
			Paths []string `yaml:"paths"`
		} `yaml:"pull_request"`
	} `yaml:"on"`
}

// githubPathPatternMatches implements the subset of GitHub's path-filter glob
// syntax used by v2-tests.yml. path.Match handles exact paths and single-level
// globs; the explicit /** case gives directory patterns their recursive GitHub
// meaning instead of Go's slash-stopping meaning.
func githubPathPatternMatches(pattern, changed string) bool {
	pattern = strings.TrimPrefix(pattern, "./")
	changed = strings.TrimPrefix(changed, "./")
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		return changed == prefix || strings.HasPrefix(changed, prefix+"/")
	}
	matched, err := path.Match(pattern, changed)
	return err == nil && matched
}

func anyPathTriggerMatches(patterns []string, changed string) bool {
	matched := false
	for _, pattern := range patterns {
		exclude := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "!")
		if githubPathPatternMatches(pattern, changed) {
			// GitHub evaluates path patterns in order: a later negative match
			// excludes a path, and a later positive match can include it again.
			matched = !exclude
		}
	}
	return matched
}

func v2TestsPullRequestPaths(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(v2TestsWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", v2TestsWorkflowPath, err)
	}
	var workflow workflowPathTriggers
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse %s: %v", v2TestsWorkflowPath, err)
	}
	paths := workflow.On.PullRequest.Paths
	if len(paths) == 0 {
		t.Fatalf("%s has no pull_request path filters; workflow parsing or trigger wiring drifted", v2TestsWorkflowPath)
	}
	return paths
}

// TestV2TestsTriggersForExternalPackageTestInputs closes the #5388 path-filter
// exemption class. These are real repository files read by tests under
// src/pkg/..., not illustrative filenames. A PR changing only one of them must
// start v2-tests, or the relevant assertion is structurally unable to fail.
func TestV2TestsTriggersForExternalPackageTestInputs(t *testing.T) {
	paths := v2TestsPullRequestPaths(t)
	externalInputs := []struct {
		property string
		changed  string
	}{
		{"agent launch stderr scrubbing", "bin/agent-launch.sh"},
		{"container pane working directory", "bin/contributor-agent.sh"},
		{"relay protocol compatibility", "bin/contributor-relay.js"},
		{"scoped GitHub App token isolation", "bin/gh-app-token.sh"},
		{"pull request default-branch handling", "bin/hive-open-pr.sh"},
		{"fresh-install release branch", "bin/hive-setup.sh"},
		{"prerequisite install release branch", "bin/hive-prereq-check.sh"},
		{"shell and Go backend parity", "config/backends.conf"},
		{"contributor recipe behavior", "Justfile"},
		{"OpenAPI route and schema parity", "dashboard/openapi.json"},
		{"this trigger contract", ".github/workflows/v2-tests.yml"},
	}
	for _, input := range externalInputs {
		t.Run(input.property, func(t *testing.T) {
			if !anyPathTriggerMatches(paths, input.changed) {
				t.Errorf("a PR changing only %s does not trigger v2-tests; the %s guard cannot fail", input.changed, input.property)
			}
		})
	}
}

// TestV2TestsPathMatcherReproducesThePre5388Gap is the failure-direction
// control: the matcher must reject the exact external inputs that the old
// filter omitted. Without this control, an accidentally always-true matcher
// could make the contract above green without proving any trigger property.
func TestV2TestsPathMatcherReproducesThePre5388Gap(t *testing.T) {
	oldPaths := []string{"src/**", "dashboard/openapi.json", ".github/workflows/v2-tests.yml"}
	for _, changed := range []string{"bin/contributor-relay.js", "config/backends.conf", "Justfile"} {
		if anyPathTriggerMatches(oldPaths, changed) {
			t.Errorf("pre-#5388 filters unexpectedly match %s; negative control no longer reproduces the exemption", changed)
		}
	}
	if anyPathTriggerMatches([]string{"bin/**", "!bin/contributor-relay.js"}, "bin/contributor-relay.js") {
		t.Error("a later negative pattern must exclude an earlier positive match")
	}
}

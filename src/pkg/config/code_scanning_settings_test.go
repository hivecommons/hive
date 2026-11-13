package config

import (
	"reflect"
	"testing"
	"time"
)

// Tests for the helpers the code_scanning settings write path shares
// (PUT /api/config/code-scanning and the admin MCP code_scanning.settings
// operation). Before them the block could only be changed by editing
// hive.yaml, and nothing outside this package could run its checks.

// The settings write path refuses a candidate with exactly what loading
// hive.yaml says, because it runs the same function on a copy.
func TestValidateCodeScanningSettingsSaysWhatHiveYAMLLoadingSays(t *testing.T) {
	off := false
	for name, tc := range map[string]struct {
		cs   CodeScanningConfig
		want string
	}{
		"valid":            {CodeScanningConfig{Enabled: true, Interval: 30 * time.Minute, MinSeverity: "medium", ExcludeRules: []string{"cpp/x-*", "!SASTID"}, Repos: map[string]CodeScanningRepoConfig{"acme/widget": {Enabled: &off}}}, ""},
		"interval too low": {CodeScanningConfig{Interval: time.Minute}, "code_scanning.interval: must be at least 10m0s, got 1m0s"},
		"unknown severity": {CodeScanningConfig{MinSeverity: "severe"}, `code_scanning.min_severity: "severe" is not one of critical, high, medium, low, note (or error, warning)`},
		"negative cap":     {CodeScanningConfig{MaxNewIssuesPerRun: -1}, "code_scanning.max_new_issues_per_run: must not be negative, got -1"},
		"malformed rule":   {CodeScanningConfig{ExcludeRules: []string{"cpp/["}}, "code_scanning.exclude_rules: bad rule pattern cpp/[: syntax error in pattern"},
		"empty rule":       {CodeScanningConfig{ExcludeRules: []string{""}}, "code_scanning.exclude_rules: empty rule pattern"},
		"repo not listed":  {CodeScanningConfig{Repos: map[string]CodeScanningRepoConfig{"nope": {}}}, `code_scanning.repos: "nope" is not listed in project.repos`},
		"repo severity":    {CodeScanningConfig{Repos: map[string]CodeScanningRepoConfig{"widget": {MinSeverity: "x"}}}, `code_scanning.repos[widget].min_severity: "x" is not one of critical, high, medium, low, note (or error, warning)`},
	} {
		t.Run(name, func(t *testing.T) {
			var loaded Config
			loaded.Project.Org = "acme"
			loaded.Project.Repos = []string{"widget"}
			loaded.CodeScanning = tc.cs
			onLoad := loaded.validateCodeScanning()
			got := ValidateCodeScanningSettings("acme", []string{"widget"}, tc.cs)
			if (got == nil) != (onLoad == nil) || (got != nil && got.Error() != onLoad.Error()) {
				t.Fatalf("settings check = %v, hive.yaml load check = %v; they must agree", got, onLoad)
			}
			if tc.want == "" {
				if got != nil {
					t.Fatalf("a valid block was refused: %v", got)
				}
				return
			}
			if got == nil || got.Error() != tc.want {
				t.Fatalf("err = %v, want %q", got, tc.want)
			}
		})
	}
}

// One entry per changed key, each with its value before and after, and none
// when the two blocks are equal.
func TestCodeScanningChangesListsEachChangedKeyOnce(t *testing.T) {
	on, off := true, false
	before := CodeScanningConfig{
		MinSeverity: "high",
		Repos: map[string]CodeScanningRepoConfig{
			"acme/widget": {Enabled: &on, MinSeverity: "medium"},
			"gadget":      {Enabled: &off},
		},
	}
	if got := CodeScanningChanges(before, before.Clone()); len(got) != 0 {
		t.Fatalf("equal blocks reported changes: %+v", got)
	}

	after := before.Clone()
	after.Enabled = true
	after.Interval = 30 * time.Minute
	after.MinSeverity = ""
	after.IncludeTools = []string{"CodeQL", "Trivy"}
	after.ExcludeRules = []string{"!SASTID"}
	after.MaxNewIssuesPerRun = 3
	widget := after.Repos["acme/widget"]
	widget.Enabled = nil
	after.Repos["acme/widget"] = widget
	delete(after.Repos, "gadget")
	after.Repos["other"] = CodeScanningRepoConfig{Enabled: &on, MaxNewIssuesPerRun: 1}

	want := []CodeScanningChange{
		{"enabled", "false", "true"},
		{"interval", "default (1h0m0s)", "30m0s"},
		{"min_severity", "high", "default (high)"},
		{"include_tools", "every tool", `["CodeQL" "Trivy"]`},
		{"exclude_rules", "the default list", `["!SASTID"]`},
		{"max_new_issues_per_run", "default (5)", "3"},
		{"repos[acme/widget]", "enabled=true min_severity=medium", "enabled=inherit min_severity=medium"},
		{"repos[gadget]", "enabled=false", "no override"},
		{"repos[other]", "no override", "enabled=true max_new_issues_per_run=1"},
	}
	if got := CodeScanningChanges(before, after); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes =\n%+v\nwant\n%+v", got, want)
	}
}

// A candidate built from the live block shares nothing with it, so a refused
// write cannot have changed the live block.
func TestCodeScanningCloneSharesNothing(t *testing.T) {
	on := true
	live := CodeScanningConfig{
		IncludeTools: []string{"CodeQL"},
		ExcludeRules: []string{"a"},
		Repos:        map[string]CodeScanningRepoConfig{"widget": {Enabled: &on, IncludeTools: []string{"Trivy"}, ExcludeRules: []string{"b"}}},
	}
	cp := live.Clone()
	cp.IncludeTools[0], cp.ExcludeRules[0] = "x", "x"
	w := cp.Repos["widget"]
	*w.Enabled = false
	w.IncludeTools[0], w.ExcludeRules[0] = "x", "x"
	cp.Repos["new"] = CodeScanningRepoConfig{}
	lw := live.Repos["widget"]
	if live.IncludeTools[0] != "CodeQL" || live.ExcludeRules[0] != "a" || !*lw.Enabled || lw.IncludeTools[0] != "Trivy" || lw.ExcludeRules[0] != "b" || len(live.Repos) != 1 {
		t.Fatalf("changing the copy changed the live block: %+v", live)
	}
}

// An override that sets nothing is the one a settings write drops; anything
// else, including enabled: false or a lone cap, is kept.
func TestCodeScanningRepoConfigIsEmpty(t *testing.T) {
	off := false
	if !(CodeScanningRepoConfig{}).IsEmpty() || !(CodeScanningRepoConfig{IncludeTools: []string{}, ExcludeRules: []string{}}).IsEmpty() {
		t.Fatal("an override that sets nothing must be empty")
	}
	for name, rc := range map[string]CodeScanningRepoConfig{
		"enabled false": {Enabled: &off},
		"severity":      {MinSeverity: "low"},
		"tools":         {IncludeTools: []string{"CodeQL"}},
		"rules":         {ExcludeRules: []string{"x"}},
		"cap":           {MaxNewIssuesPerRun: 1},
	} {
		if rc.IsEmpty() {
			t.Errorf("%s: an override that sets something was reported empty", name)
		}
	}
	if CodeScanningRepoKey("acme", "Acme/Widget") != CodeScanningRepoKey("acme", "widget") || CodeScanningRepoKey("acme", "other/widget") == CodeScanningRepoKey("acme", "widget") {
		t.Fatal("repos keys must match under project.org, case-insensitively, and not across owners")
	}
}

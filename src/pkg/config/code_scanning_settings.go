package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/codescanning"
)

// The code_scanning settings write path (PUT /api/config/code-scanning and the
// admin MCP code_scanning.settings operation) builds a candidate block, checks
// it here and only then changes the live config. Everything both callers need
// to agree on lives in this file, so the dashboard and the MCP cannot drift.

// ValidateCodeScanningSettings checks a candidate code_scanning block against
// project.org and project.repos with the checks, and the error text, that
// loading hive.yaml applies (validateCodeScanning). Nothing is changed.
func ValidateCodeScanningSettings(org string, repos []string, cs CodeScanningConfig) error {
	cand := Config{CodeScanning: cs}
	cand.Project.Org = org
	cand.Project.Repos = repos
	return cand.validateCodeScanning()
}

// CodeScanningRepoKey is the form code_scanning.repos keys are compared in:
// the bare repository name under project.org, lower-cased. "widget",
// "Widget" and "acme/widget" are the same key under org acme.
func CodeScanningRepoKey(org, repo string) string {
	return codeScanningRepoKey(org, repo)
}

// IsEmpty reports whether the override sets nothing at all, so the repository
// would behave exactly as it does with no entry.
func (r CodeScanningRepoConfig) IsEmpty() bool {
	return r.Enabled == nil && r.MinSeverity == "" && len(r.IncludeTools) == 0 && len(r.ExcludeRules) == 0 && r.MaxNewIssuesPerRun == 0
}

// Clone returns a copy that shares no slice, map or pointer with c, so a
// candidate built from the live block can be changed and thrown away.
func (c CodeScanningConfig) Clone() CodeScanningConfig {
	out := c
	out.IncludeTools = append([]string(nil), c.IncludeTools...)
	out.ExcludeRules = append([]string(nil), c.ExcludeRules...)
	if c.Repos != nil {
		out.Repos = make(map[string]CodeScanningRepoConfig, len(c.Repos))
		for k, v := range c.Repos {
			if v.Enabled != nil {
				b := *v.Enabled
				v.Enabled = &b
			}
			v.IncludeTools = append([]string(nil), v.IncludeTools...)
			v.ExcludeRules = append([]string(nil), v.ExcludeRules...)
			out.Repos[k] = v
		}
	}
	return out
}

// CodeScanningChange is one code_scanning key a settings write changes, with
// its value before and after as the audit log and the write preview show it.
type CodeScanningChange struct {
	Key    string
	Before string
	After  string
}

// CodeScanningChanges lists what differs between two code_scanning blocks, one
// entry per key: the six top-level keys in a fixed order, then repos[<key>]
// for every per-repository override that was added, removed or changed, in key
// order. Two equal blocks give no entries.
func CodeScanningChanges(before, after CodeScanningConfig) []CodeScanningChange {
	var out []CodeScanningChange
	add := func(key, from, to string) {
		if from != to {
			out = append(out, CodeScanningChange{Key: key, Before: from, After: to})
		}
	}
	add("enabled", strconv.FormatBool(before.Enabled), strconv.FormatBool(after.Enabled))
	add("interval", codeScanningIntervalLabel(before), codeScanningIntervalLabel(after))
	add("min_severity", codeScanningSeverityLabel(before.MinSeverity), codeScanningSeverityLabel(after.MinSeverity))
	add("include_tools", codeScanningListLabel(before.IncludeTools, "every tool"), codeScanningListLabel(after.IncludeTools, "every tool"))
	add("exclude_rules", codeScanningListLabel(before.ExcludeRules, "the default list"), codeScanningListLabel(after.ExcludeRules, "the default list"))
	add("max_new_issues_per_run", codeScanningCapLabel(before.MaxNewIssuesPerRun), codeScanningCapLabel(after.MaxNewIssuesPerRun))

	seen := map[string]bool{}
	var keys []string
	for _, m := range []map[string]CodeScanningRepoConfig{before.Repos, after.Repos} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		b, hadB := before.Repos[k]
		a, hasA := after.Repos[k]
		add("repos["+k+"]", codeScanningRepoLabel(b, hadB), codeScanningRepoLabel(a, hasA))
	}
	return out
}

func codeScanningIntervalLabel(c CodeScanningConfig) string {
	if c.Interval == 0 {
		return "default (" + DefaultCodeScanningInterval.String() + ")"
	}
	return c.Interval.String()
}

func codeScanningSeverityLabel(sev string) string {
	if sev == "" {
		return "default (" + codescanning.DefaultMinLevel.String() + ")"
	}
	return sev
}

func codeScanningCapLabel(n int) string {
	if n == 0 {
		return "default (" + strconv.Itoa(codescanning.DefaultMaxNewIssues) + ")"
	}
	return strconv.Itoa(n)
}

func codeScanningListLabel(list []string, empty string) string {
	if len(list) == 0 {
		return empty
	}
	return fmt.Sprintf("%q", list)
}

// codeScanningRepoLabel renders one per-repository override in full, so two
// overrides are equal exactly when their labels are.
func codeScanningRepoLabel(r CodeScanningRepoConfig, present bool) string {
	if !present {
		return "no override"
	}
	parts := []string{"enabled=inherit"}
	if r.Enabled != nil {
		parts[0] = "enabled=" + strconv.FormatBool(*r.Enabled)
	}
	if r.MinSeverity != "" {
		parts = append(parts, "min_severity="+r.MinSeverity)
	}
	if len(r.IncludeTools) > 0 {
		parts = append(parts, "include_tools="+codeScanningListLabel(r.IncludeTools, ""))
	}
	if len(r.ExcludeRules) > 0 {
		parts = append(parts, "exclude_rules="+codeScanningListLabel(r.ExcludeRules, ""))
	}
	if r.MaxNewIssuesPerRun != 0 {
		parts = append(parts, "max_new_issues_per_run="+strconv.Itoa(r.MaxNewIssuesPerRun))
	}
	return strings.Join(parts, " ")
}

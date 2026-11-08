package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/codescanning"
)

func TestCodeScanningIsOffByDefault(t *testing.T) {
	var c Config
	if _, on := c.CodeScanning.For("acme", "acme/widget"); on || c.CodeScanning.AnyEnabled([]string{"widget"}, "acme") {
		t.Fatal("code scanning sweep is on with no config")
	}
	if got := c.CodeScanning.IntervalOrDefault(); got != time.Hour {
		t.Fatalf("default interval %s, want 1h", got)
	}
}

func TestCodeScanningPerRepoOverride(t *testing.T) {
	var c Config
	src := `
code_scanning:
  min_severity: critical
  exclude_rules: [cpp/commented-out-code]
  repos:
    acme/widget:
      enabled: true
      min_severity: medium
      max_new_issues_per_run: 2
    gadget:
      enabled: true
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	c.Project.Org = "acme"
	c.Project.Repos = []string{"widget", "gadget", "other"}
	if err := c.validateCodeScanning(); err != nil {
		t.Fatalf("valid block rejected: %v", err)
	}
	p, on := c.CodeScanning.For("acme", "widget")
	if !on || p.MinLevel != codescanning.LevelMedium || p.MaxNewIssues != 2 || len(p.ExcludeRules) != 1 {
		t.Fatalf("widget policy %+v on=%v", p, on)
	}
	p, on = c.CodeScanning.For("acme", "acme/gadget")
	if !on || p.MinLevel != codescanning.LevelCritical || p.MaxNewIssues != 0 {
		t.Fatalf("gadget inherits the top-level policy: %+v on=%v", p, on)
	}
	if _, on := c.CodeScanning.For("acme", "other"); on {
		t.Fatal("a repo with no override follows code_scanning.enabled (off)")
	}
	c.CodeScanning.Enabled = true
	off := false
	c.CodeScanning.Repos["other"] = CodeScanningRepoConfig{Enabled: &off}
	if _, on := c.CodeScanning.For("acme", "other"); on {
		t.Fatal("per-repo enabled: false did not turn it off")
	}
}

func TestCodeScanningValidation(t *testing.T) {
	base := func() Config {
		var c Config
		c.Project.Org = "acme"
		c.Project.Repos = []string{"widget"}
		return c
	}
	for name, mutate := range map[string]func(*Config){
		"bad severity": func(c *Config) { c.CodeScanning.MinSeverity = "severe" },
		"negative cap": func(c *Config) { c.CodeScanning.MaxNewIssuesPerRun = -1 },
		"bad interval": func(c *Config) { c.CodeScanning.Interval = time.Minute },
		"bad pattern":  func(c *Config) { c.CodeScanning.ExcludeRules = []string{"cpp/["} },
		"unknown repo": func(c *Config) { c.CodeScanning.Repos = map[string]CodeScanningRepoConfig{"nope": {}} },
		"repo severity": func(c *Config) {
			c.CodeScanning.Repos = map[string]CodeScanningRepoConfig{"widget": {MinSeverity: "x"}}
		},
	} {
		c := base()
		mutate(&c)
		if err := c.validateCodeScanning(); err == nil || !strings.Contains(err.Error(), "code_scanning") {
			t.Errorf("%s: err %v", name, err)
		}
	}
}

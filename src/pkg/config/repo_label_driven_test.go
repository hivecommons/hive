package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// project.repo_policies[].label_driven (hivecommons/hive#10537) is opt-in per
// repo: an unlisted repo, or one listed without the key, keeps today's
// un-park behavior.
func TestRepoLabelDrivenResolvesPerRepo(t *testing.T) {
	var cfg Config
	raw := []byte(`
project:
  org: projectbluefin
  repos: [common, chairlift, other]
  repo_policies:
    - repo: common
      label_driven: true
    - repo: projectbluefin/chairlift
      auto_merge: false
`)
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := map[string]bool{
		"common":                   true,
		"projectbluefin/common":    true,
		"chairlift":                false,
		"projectbluefin/chairlift": false,
		"other":                    false,
	}
	for repo, want := range cases {
		if got := cfg.RepoLabelDriven(repo); got != want {
			t.Errorf("RepoLabelDriven(%q) = %v, want %v", repo, got, want)
		}
	}

	var nilCfg *Config
	if nilCfg.RepoLabelDriven("common") {
		t.Error("nil config must not report label-driven")
	}
}

func TestRepoLabelDrivenCountsAsOverride(t *testing.T) {
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "common", LabelDriven: true}) {
		t.Fatal("a label_driven entry must not be pruned as empty when another override is cleared")
	}
}

// Label-driven approval (hivecommons/hive#11313) resolves only for repos with
// label_driven; everything else gets nil so the feature is a no-op there.
func TestLabelDrivenApprovalLabelsResolve(t *testing.T) {
	var cfg Config
	raw := []byte(`
project:
  org: projectbluefin
  repos: [common, chairlift, printer, other]
  label_driven_approval_labels: [accepted, triage/accepted]
  label_driven_trust_org_members: false
  label_driven_trusted_bots: [" prow[bot] "]
  repo_policies:
    - repo: common
      label_driven: true
    - repo: chairlift
      label_driven: true
      label_driven_approval_labels: [ready]
    - repo: printer
      label_driven: true
      label_driven_approval_labels: []
    - repo: other
      auto_merge: false
`)
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := map[string][]string{
		"common":    {"accepted", "triage/accepted"},
		"chairlift": {"ready"},
		"printer":   {},
		"other":     nil,
		"unlisted":  nil,
	}
	for repo, want := range cases {
		got := cfg.LabelDrivenApprovalLabels(repo)
		if len(got) != len(want) || (want == nil) != (got == nil) {
			t.Errorf("LabelDrivenApprovalLabels(%q) = %v, want %v", repo, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("LabelDrivenApprovalLabels(%q)[%d] = %q, want %q", repo, i, got[i], want[i])
			}
		}
	}
	if cfg.LabelDrivenTrustOrgMembers() {
		t.Error("label_driven_trust_org_members: false must be honoured")
	}
	if bots := cfg.LabelDrivenTrustedBots(); len(bots) != 1 || bots[0] != "prow[bot]" {
		t.Errorf("trusted bots = %v, want [prow[bot]]", bots)
	}

	var defaults Config
	if err := yaml.Unmarshal([]byte("project:\n  org: o\n  repos: [r]\n  repo_policies:\n    - repo: r\n      label_driven: true\n"), &defaults); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := defaults.LabelDrivenApprovalLabels("r"); len(got) != 1 || got[0] != DefaultLabelDrivenApprovalLabel {
		t.Errorf("default labels = %v, want [%s]", got, DefaultLabelDrivenApprovalLabel)
	}
	if !defaults.LabelDrivenTrustOrgMembers() {
		t.Error("org-member trust must default on")
	}
	if got := defaults.LabelDrivenTrustedBots(); len(got) != len(DefaultLabelDrivenTrustedBots) {
		t.Errorf("default bots = %v", got)
	}

	var nilCfg *Config
	if nilCfg.LabelDrivenApprovalLabels("r") != nil || !nilCfg.LabelDrivenTrustOrgMembers() || len(nilCfg.LabelDrivenTrustedBots()) == 0 {
		t.Error("nil config must return defaults without panicking")
	}
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "r", LabelDrivenApprovalLabels: []string{"x"}}) {
		t.Error("a per-repo approval label override must not be pruned as empty")
	}
}

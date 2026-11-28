package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// auto_merge.promotions: per-repository branch-promotion rules, default off.
// On v6 tip the key does not exist, so the YAML below does not load into
// AutoMergeConfig.Promotions and none of these compile.

func promotionTestConfig(t *testing.T, doc string) *Config {
	t.Helper()
	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"widget", "other/lib"}
	return &cfg
}

func TestAutoMergePromotionsLoadAndLookup(t *testing.T) {
	cfg := promotionTestConfig(t, `auto_merge:
  promotions:
    Acme/Widget:
      - source: " main "
        target: stable
        authors: ["github-actions[bot]", " "]
        head_ref_prefix: auto/promote-
`)
	if err := cfg.validateAutoMergePromotions(); err == nil || !strings.Contains(err.Error(), "empty login") {
		t.Fatalf("blank author accepted: %v", err)
	}
	cfg.AutoMerge.Promotions["Acme/Widget"][0].Authors = []string{"github-actions[bot]"}
	if err := cfg.validateAutoMergePromotions(); err != nil {
		t.Fatalf("valid rule refused: %v", err)
	}
	rules := cfg.AutoMerge.PromotionRulesFor(" acme/WIDGET ")
	if len(rules) != 1 || rules[0].Source != "main" || rules[0].Target != "stable" || rules[0].HeadRefPrefix != "auto/promote-" {
		t.Fatalf("rules = %+v, want the normalized main to stable rule", rules)
	}
	if !rules[0].HasAuthor("GitHub-Actions[bot]") || rules[0].HasAuthor("github-actions") || rules[0].HasAuthor("") {
		t.Fatalf("author match is not an exact (case-insensitive) login match: %+v", rules[0].Authors)
	}
	if got := cfg.PromotionRulesFor("acme/widget"); len(got) != 1 {
		t.Fatalf("Config.PromotionRulesFor = %+v, want the rule of a managed repository", got)
	}
	if got := cfg.AutoMerge.PromotionRulesFor("acme/other"); got != nil {
		t.Fatalf("unlisted repository has rules: %+v", got)
	}
	if got := (AutoMergeConfig{}).PromotionRulesFor("acme/widget"); got != nil {
		t.Fatalf("unset config has rules: %+v", got)
	}
}

func TestAutoMergePromotionsValidation(t *testing.T) {
	valid := func() PromotionRule {
		return PromotionRule{Source: "main", Target: "stable", Authors: []string{"github-actions[bot]"}, HeadRefPrefix: "auto/promote-"}
	}
	for _, tc := range []struct {
		name   string
		repo   string
		rules  func() []PromotionRule
		errHas string
	}{
		{name: "valid", repo: "acme/widget", rules: func() []PromotionRule { return []PromotionRule{valid()} }},
		{name: "owner/repo in project.repos", repo: "other/lib", rules: func() []PromotionRule { return []PromotionRule{valid()} }},
		{name: "bare repository name", repo: "widget", rules: func() []PromotionRule { return []PromotionRule{valid()} }, errHas: "want owner/repo"},
		{name: "source equals target", repo: "acme/widget", rules: func() []PromotionRule {
			r := valid()
			r.Target = "main"
			return []PromotionRule{r}
		}, errHas: "must be different"},
		{name: "no author", repo: "acme/widget", rules: func() []PromotionRule {
			r := valid()
			r.Authors = nil
			return []PromotionRule{r}
		}, errHas: "at least one author"},
		{name: "author pattern", repo: "acme/widget", rules: func() []PromotionRule {
			r := valid()
			r.Authors = []string{"*[bot]"}
			return []PromotionRule{r}
		}, errHas: "patterns are not accepted"},
		{name: "empty source", repo: "acme/widget", rules: func() []PromotionRule {
			r := valid()
			r.Source = " "
			return []PromotionRule{r}
		}, errHas: "source is required"},
		{name: "empty target", repo: "acme/widget", rules: func() []PromotionRule {
			r := valid()
			r.Target = ""
			return []PromotionRule{r}
		}, errHas: "target is required"},
		{name: "empty head ref prefix", repo: "acme/widget", rules: func() []PromotionRule {
			r := valid()
			r.HeadRefPrefix = ""
			return []PromotionRule{r}
		}, errHas: "head_ref_prefix is required"},
		{name: "target twice", repo: "acme/widget", rules: func() []PromotionRule {
			r := valid()
			r.Source = "develop"
			return []PromotionRule{valid(), r}
		}, errHas: "more than one rule"},
		{name: "no rules", repo: "acme/widget", rules: func() []PromotionRule { return nil }, errHas: "at least one rule"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := promotionTestConfig(t, "{}")
			cfg.AutoMerge.Promotions = map[string][]PromotionRule{tc.repo: tc.rules()}
			err := cfg.validateAutoMergePromotions()
			if tc.errHas == "" {
				if err != nil {
					t.Fatalf("valid config refused: %v", err)
				}
				if len(cfg.AutoMerge.PromotionRulesFor(tc.repo)) != 1 {
					t.Fatalf("valid rule not returned for %s", tc.repo)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errHas) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.errHas)
			}
			// An invalid entry never widens anything, even unvalidated.
			if got := cfg.AutoMerge.PromotionRulesFor(tc.repo); got != nil && tc.errHas != "want owner/repo" {
				t.Fatalf("invalid rules returned: %+v", got)
			}
		})
	}
	cfg := promotionTestConfig(t, "{}")
	cfg.AutoMerge.Promotions = map[string][]PromotionRule{"acme/widget": {valid()}, "ACME/Widget": {valid()}}
	if err := cfg.validateAutoMergePromotions(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate key accepted: %v", err)
	}
	if got := cfg.AutoMerge.PromotionRulesFor("acme/widget"); got != nil {
		t.Fatalf("ambiguous keys returned rules: %+v", got)
	}
}

// The rules are checked by the config's own Validate, so hive.yaml refuses
// a bad rule at load.
func TestAutoMergePromotionsValidatedOnLoad(t *testing.T) {
	cfg := validatableConfig(nil)
	cfg.Project.Repos = []string{"widget"}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err != nil {
		t.Fatalf("baseline config refused: %v", err)
	}
	cfg.AutoMerge.Promotions = map[string][]PromotionRule{"my-org/widget": {{Source: "main", Target: "stable", Authors: []string{"github-actions[bot]"}, HeadRefPrefix: "auto/"}}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err != nil {
		t.Fatalf("valid promotion refused at load: %v", err)
	}
	cfg.AutoMerge.Promotions = map[string][]PromotionRule{"my-org/widget": {{Source: "main", Target: "main", Authors: []string{"github-actions[bot]"}, HeadRefPrefix: "auto/"}}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err == nil || !strings.Contains(err.Error(), "auto_merge.promotions") {
		t.Fatalf("Validate = %v, want the promotions refusal", err)
	}
}

// A repository can leave project.repos (a hub heartbeat, Settings) while
// its promotion rule stays in the file. The config must still load, and the
// stale rule must never apply.
func TestAutoMergePromotionsStaleRepositoryLoadsAndIsIgnored(t *testing.T) {
	cfg := validatableConfig(nil)
	cfg.Project.Repos = []string{"widget"}
	cfg.AutoMerge.Promotions = map[string][]PromotionRule{"my-org/removed": {{Source: "main", Target: "stable", Authors: []string{"github-actions[bot]"}, HeadRefPrefix: "auto/promote-"}}}
	if err := cfg.ValidateWithOptions(ValidateOptions{}); err != nil {
		t.Fatalf("a promotion rule for a repository that left project.repos made the config unloadable: %v", err)
	}
	if got := cfg.PromotionRulesFor("my-org/removed"); got != nil {
		t.Fatalf("stale repository's rules apply: %+v", got)
	}
}

// inherit_human_merge_paths is off unless a rule sets it, survives the
// normalization every reader goes through, and is left out of the written
// file while it is off. On v6 tip the field does not exist.
func TestAutoMergePromotionsInheritHumanMergePaths(t *testing.T) {
	cfg := promotionTestConfig(t, `auto_merge:
  promotions:
    acme/widget:
      - source: main
        target: stable
        authors: ["github-actions[bot]"]
        head_ref_prefix: auto/promote-
      - source: main
        target: beta
        authors: ["github-actions[bot]"]
        head_ref_prefix: auto/beta-
        inherit_human_merge_paths: true
`)
	if err := cfg.validateAutoMergePromotions(); err != nil {
		t.Fatalf("valid rules refused: %v", err)
	}
	rules := cfg.PromotionRulesFor("acme/widget")
	if len(rules) != 2 || rules[0].InheritHumanMergePaths || !rules[1].InheritHumanMergePaths {
		t.Fatalf("rules = %+v, want the option off by default and on for the beta rule", rules)
	}
	if n := (PromotionRule{Source: " main ", Target: "stable", Authors: []string{"a"}, HeadRefPrefix: "p", InheritHumanMergePaths: true}).Normalized(); !n.InheritHumanMergePaths || n.Source != "main" {
		t.Fatalf("Normalized dropped the option: %+v", n)
	}
	out, err := yaml.Marshal(cfg.AutoMerge.Promotions)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(out), "inherit_human_merge_paths"); got != 1 || !strings.Contains(string(out), "inherit_human_merge_paths: true") {
		t.Fatalf("written rules name the option %d times, want once (only where it is on):\n%s", got, out)
	}
	var back map[string][]PromotionRule
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if r := back["acme/widget"]; len(r) != 2 || r[0].InheritHumanMergePaths || !r[1].InheritHumanMergePaths {
		t.Fatalf("round trip = %+v, want the option kept per rule", back)
	}
}

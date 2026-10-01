package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTrustedBotAuthorSetDefaultsToDependabot(t *testing.T) {
	var a AutoMergeConfig
	got := a.TrustedBotAuthorSet()
	if len(got) != 1 || !got["dependabot[bot]"] {
		t.Fatalf("unset TrustedBotAuthors should default to dependabot only, got %v", got)
	}
}

func TestTrustedBotAuthorSetExplicitEmptyDisables(t *testing.T) {
	var a AutoMergeConfig
	if err := yaml.Unmarshal([]byte("trusted_bot_authors: []\n"), &a); err != nil {
		t.Fatal(err)
	}
	if a.TrustedBotAuthors == nil {
		t.Fatal("explicit empty list must decode as non-nil so it is distinguishable from unset")
	}
	if got := a.TrustedBotAuthorSet(); len(got) != 0 {
		t.Fatalf("explicit empty list should disable the lane, got %v", got)
	}
}

func TestTrustedBotAuthorSetNormalizes(t *testing.T) {
	var a AutoMergeConfig
	if err := yaml.Unmarshal([]byte("trusted_bot_authors:\n  - ' Renovate[bot] '\n  - ''\n  - 'dependabot[bot]'\n"), &a); err != nil {
		t.Fatal(err)
	}
	got := a.TrustedBotAuthorSet()
	if len(got) != 2 || !got["renovate[bot]"] || !got["dependabot[bot]"] {
		t.Fatalf("expected lower-cased, trimmed, blank-dropped set, got %v", got)
	}
}

func TestMergeHumanPRsAtL6DefaultAndACMMGate(t *testing.T) {
	l5 := 5
	l6 := 6
	var a AutoMergeConfig
	if !a.MergeHumanPRsAtL6Enabled() {
		t.Fatal("unset MergeHumanPRsAtL6 should default to enabled")
	}
	if a.MergeHumanPRsAtL6Allowed(&l5) {
		t.Fatal("human-green lane must be impossible below L6")
	}
	if !a.MergeHumanPRsAtL6Allowed(&l6) {
		t.Fatal("human-green lane should be enabled by default at L6")
	}
	off := false
	a.MergeHumanPRsAtL6 = &off
	if a.MergeHumanPRsAtL6Allowed(&l6) {
		t.Fatal("explicit false should disable human-green at L6")
	}
}

func TestMergeHumanPRsAtL6YAMLRoundTrip(t *testing.T) {
	var a AutoMergeConfig
	if err := yaml.Unmarshal([]byte("merge_human_prs_at_l6: false\n"), &a); err != nil {
		t.Fatal(err)
	}
	if a.MergeHumanPRsAtL6 == nil || *a.MergeHumanPRsAtL6 {
		t.Fatalf("expected explicit false pointer, got %+v", a.MergeHumanPRsAtL6)
	}
	out, err := yaml.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "merge_human_prs_at_l6: false") {
		t.Fatalf("round-trip YAML missing flag: %s", out)
	}
}

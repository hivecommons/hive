package config

import (
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

package config

import (
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/intent"
	"gopkg.in/yaml.v3"
)

func TestHumanMergePinOnlyConfig(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte(`auto_merge:
  human_merge_pin_only:
    Owner/Repo:
      enabled: true
      paths: [".github/workflows/*.yml"]
      kinds: [action-sha]
      trusted_authors: [maintainer]
`), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AutoMerge.ValidateHumanMergePinOnly(); err != nil {
		t.Fatal(err)
	}
	policy, ok := cfg.AutoMerge.HumanMergePinOnlyFor(" owner/REPO ")
	if !ok || len(policy.Spec().Paths) != 1 || policy.TrustedAuthors[0] != "maintainer" {
		t.Fatalf("policy = %+v, %v", policy, ok)
	}
	policy.Paths[0] = "*"
	policy.Kinds[0] = "anything"
	policy.TrustedAuthors[0] = "attacker"
	again, _ := cfg.AutoMerge.HumanMergePinOnlyFor("owner/repo")
	if again.Paths[0] == "*" || again.Kinds[0] != "action-sha" || again.TrustedAuthors[0] != "maintainer" {
		t.Fatal("accessor leaked config slices")
	}
	if _, ok := cfg.AutoMerge.HumanMergePinOnlyFor("other/repo"); ok {
		t.Fatal("unknown repo enabled")
	}
	if _, ok := (AutoMergeConfig{}).HumanMergePinOnlyFor("owner/repo"); ok {
		t.Fatal("absent config enabled")
	}
}

func TestHumanMergePinOnlyInvalidConfig(t *testing.T) {
	valid := func() PinOnlyException {
		return PinOnlyException{Enabled: true, Paths: []string{"Containerfile"}, Kinds: []string{"image-digest"}, TrustedAuthors: []string{"maintainer"}}
	}
	for _, tc := range []struct {
		name   string
		change func(*PinOnlyException)
		valid  bool
	}{
		{"valid", func(_ *PinOnlyException) {}, true},
		{"disabled", func(p *PinOnlyException) { *p = PinOnlyException{} }, true},
		{"no paths", func(p *PinOnlyException) { p.Paths = nil }, false},
		{"no kinds", func(p *PinOnlyException) { p.Kinds = nil }, false},
		{"no authors", func(p *PinOnlyException) { p.TrustedAuthors = nil }, false},
		{"blank author", func(p *PinOnlyException) { p.TrustedAuthors = []string{" "} }, false},
		{"blank path", func(p *PinOnlyException) { p.Paths = []string{""} }, false},
		{"unknown kind", func(p *PinOnlyException) { p.Kinds = []string{"sha-shaped-text"} }, false},
		{"action-sha-ref with action-sha", func(p *PinOnlyException) { p.Kinds = []string{"action-sha", "action-sha-ref"} }, true},
		// Valid but inert: without action-sha no uses: pin is accepted, so
		// the classifier refuses every bare SHA line (see pkg/intent).
		{"action-sha-ref alone", func(p *PinOnlyException) { p.Kinds = []string{"action-sha-ref"} }, true},
		{"near miss of action-sha-ref", func(p *PinOnlyException) { p.Kinds = []string{"action-sha", "action-sha-refs"} }, false},
		{"disabled unknown kind", func(p *PinOnlyException) { p.Enabled = false; p.Kinds = []string{"anything"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := valid()
			tc.change(&p)
			a := AutoMergeConfig{HumanMergePinOnly: map[string]PinOnlyException{"o/r": p}}
			if err := a.ValidateHumanMergePinOnly(); (err == nil) != tc.valid {
				t.Fatalf("validation = %v", err)
			}
			_, ok := a.HumanMergePinOnlyFor("O/R")
			if ok != (tc.valid && p.Enabled) {
				t.Fatalf("accessor enabled = %v", ok)
			}
		})
	}
	for _, key := range []string{"", "owner", "o/", "/r", "o/r/extra", "o /r", "*/r"} {
		a := AutoMergeConfig{HumanMergePinOnly: map[string]PinOnlyException{key: valid()}}
		if err := a.ValidateHumanMergePinOnly(); err == nil {
			t.Errorf("accepted invalid key %q", key)
		}
	}
	a := AutoMergeConfig{HumanMergePinOnly: map[string]PinOnlyException{"o/r": valid(), "O/R": valid()}}
	if err := a.ValidateHumanMergePinOnly(); err == nil {
		t.Fatal("accepted duplicate key")
	}
	if _, ok := a.HumanMergePinOnlyFor("o/r"); ok {
		t.Fatal("ambiguous policy enabled")
	}
}

// The kinds config accepts are the classifier's own list, so a kind the
// editors offer is never one the classifier refuses, and the reverse.
func TestPinOnlyKindsMatchClassifier(t *testing.T) {
	if !slices.Equal(PinOnlyKinds, intent.PinOnlyKinds()) {
		t.Fatalf("config.PinOnlyKinds = %v, classifier knows %v", PinOnlyKinds, intent.PinOnlyKinds())
	}
	if !slices.Contains(PinOnlyKinds, "action-sha-ref") {
		t.Fatalf("config.PinOnlyKinds = %v, want action-sha-ref offered", PinOnlyKinds)
	}
	for _, kind := range PinOnlyKinds {
		p := PinOnlyException{Enabled: true, Paths: []string{".github/workflows/*.yml"}, Kinds: []string{kind}, TrustedAuthors: []string{"maintainer"}}
		if err := p.validate(); err != nil {
			t.Errorf("kind %q refused: %v", kind, err)
		}
	}
}

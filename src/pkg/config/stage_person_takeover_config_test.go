package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// governor.labels.stage.adoption.person_after / person_grace / person_repos /
// person_human_repos default off (hivecommons/hive#19395): a config without
// them never takes over a person's PR.
func TestStagePersonTakeoverDefaultsOff(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("acmm_level: 6\nproject:\n  org: acme\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	adoption := cfg.Governor.Labels.Stage.Adoption
	if got := adoption.EffectivePersonAfter(); got != 0 {
		t.Errorf("person_after = %v, want 0 (off)", got)
	}
	if got := adoption.EffectivePersonGrace(); got != DefaultPersonTakeoverGrace {
		t.Errorf("person_grace = %v, want the %v default", got, DefaultPersonTakeoverGrace)
	}
	if DefaultPersonTakeoverGrace != 24*time.Hour {
		t.Errorf("DefaultPersonTakeoverGrace = %v, want 24h", DefaultPersonTakeoverGrace)
	}
	for _, human := range []bool{false, true} {
		if adoption.PersonListed("acme/widget", human) {
			t.Errorf("PersonListed(human=%v) true with nothing configured", human)
		}
		if cfg.PersonTakeoverAllowed("acme/widget", human) {
			t.Errorf("PersonTakeoverAllowed(human=%v) true with nothing configured", human)
		}
	}
}

func TestStagePersonTakeoverParsesKnobs(t *testing.T) {
	src := `acmm_level: 6
governor:
  labels:
    stage:
      adoption:
        person_after: 24h
        person_grace: 6h
        person_repos: [acme/widget, gadget]
        person_human_repos: [acme/docs]
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	adoption := cfg.Governor.Labels.Stage.Adoption
	if got := adoption.EffectivePersonAfter(); got != 24*time.Hour {
		t.Fatalf("person_after = %v, want 24h", got)
	}
	if got := adoption.EffectivePersonGrace(); got != 6*time.Hour {
		t.Fatalf("person_grace = %v, want 6h", got)
	}
	for _, tc := range []struct {
		repo  string
		human bool
		want  bool
	}{
		{"acme/widget", false, true},
		{"ACME/Widget", false, true},
		{"acme/gadget", false, true}, // bare name matches any owner
		{"o/gadget", false, true},
		{"acme/docs", false, false}, // only in the human list
		{"acme/docs", true, true},
		{"acme/widget", true, false}, // only in the owners' list
		{"acme/unlisted", false, false},
		{"", false, false},
	} {
		if got := adoption.PersonListed(tc.repo, tc.human); got != tc.want {
			t.Errorf("PersonListed(%q, human=%v) = %v, want %v", tc.repo, tc.human, got, tc.want)
		}
		if got := cfg.PersonTakeoverAllowed(tc.repo, tc.human); got != tc.want {
			t.Errorf("PersonTakeoverAllowed(%q, human=%v) = %v, want %v", tc.repo, tc.human, got, tc.want)
		}
	}
}

// person_grace: unset, "0", negative or unparsable read as the 24h default,
// never as "no grace".
func TestStagePersonGraceFallsBackToDefault(t *testing.T) {
	for _, raw := range []string{"", "0", "0s", "-1h", "soon"} {
		c := StageAdoptionConfig{PersonGrace: raw}
		if got := c.EffectivePersonGrace(); got != DefaultPersonTakeoverGrace {
			t.Errorf("person_grace %q = %v, want %v", raw, got, DefaultPersonTakeoverGrace)
		}
	}
	if got := (StageAdoptionConfig{PersonGrace: "48h"}).EffectivePersonGrace(); got != 48*time.Hour {
		t.Errorf("person_grace 48h = %v", got)
	}
	for _, raw := range []string{"", "0", "-1h", "soon"} {
		if got := (StageAdoptionConfig{PersonAfter: raw}).EffectivePersonAfter(); got != 0 {
			t.Errorf("person_after %q = %v, want 0 (off)", raw, got)
		}
	}
}

// The timed takeover is L6-only, needs person_after and adoption on.
func TestStagePersonTakeoverGates(t *testing.T) {
	level := func(n int) *int { return &n }
	off := false
	base := func() *Config {
		c := &Config{ACMMLevel: level(6)}
		c.Governor.Labels.Stage.Adoption.PersonAfter = "24h"
		c.Governor.Labels.Stage.Adoption.PersonRepos = []string{"acme/widget"}
		c.Governor.Labels.Stage.Adoption.PersonHumanRepos = []string{"acme/widget"}
		return c
	}
	for _, human := range []bool{false, true} {
		if !base().PersonTakeoverAllowed("acme/widget", human) {
			t.Fatalf("baseline must allow (human=%v)", human)
		}
		below := base()
		below.ACMMLevel = level(5)
		if below.PersonTakeoverAllowed("acme/widget", human) {
			t.Errorf("allowed below L6 (human=%v)", human)
		}
		repoBelow := base()
		repoBelow.Project.Org = "acme"
		repoBelow.Project.RepoPolicies = []RepoPolicy{{Repo: "widget", ACMMLevel: level(4)}}
		if repoBelow.PersonTakeoverAllowed("acme/widget", human) {
			t.Errorf("allowed where the repo policy lowers the level below L6 (human=%v)", human)
		}
		timerOff := base()
		timerOff.Governor.Labels.Stage.Adoption.PersonAfter = "0"
		if timerOff.PersonTakeoverAllowed("acme/widget", human) {
			t.Errorf("allowed with person_after off (human=%v)", human)
		}
		adoptionOff := base()
		adoptionOff.Governor.Labels.Stage.Adoption.Enabled = &off
		if adoptionOff.PersonTakeoverAllowed("acme/widget", human) {
			t.Errorf("allowed with adoption off (human=%v)", human)
		}
	}
	var nilCfg *Config
	if nilCfg.PersonTakeoverAllowed("acme/widget", false) {
		t.Error("nil config allowed")
	}
}

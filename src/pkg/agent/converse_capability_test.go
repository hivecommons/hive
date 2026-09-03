package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/config"
)

// TestDefaultCapabilitiesAreEmptyEverywhere is the compatibility contract of
// #4492 stated as a test: `converse` is opt-in at every mode and every ACMM
// level, so a hive whose config never mentions it is unchanged.
func TestDefaultCapabilitiesAreEmptyEverywhere(t *testing.T) {
	modes := []AgentMode{ModeAdvisory, ModeIssuesOnly, ModeIssuesAndPRs, ModeIssuesPRsMerge}
	for _, m := range modes {
		for level := 1; level <= 5; level++ {
			if got := DefaultCapabilities(m, level); got.Any() {
				t.Errorf("DefaultCapabilities(%s, L%d) = %+v — capabilities must default off so existing hives do not change behaviour", m, level, got)
			}
		}
	}
}

func TestCapabilitiesRoundTrip(t *testing.T) {
	cases := []struct {
		caps AgentCapabilities
		text string
	}{
		{AgentCapabilities{}, ""},
		{AgentCapabilities{Converse: true}, "converse"},
	}
	for _, tc := range cases {
		if got := tc.caps.String(); got != tc.text {
			t.Errorf("String() = %q, want %q", got, tc.text)
		}
		if got := ParseCapabilities(tc.text); got != tc.caps {
			t.Errorf("ParseCapabilities(%q) = %+v, want %+v", tc.text, got, tc.caps)
		}
	}

	// Tolerant of whitespace and case, because the file is hand-editable during
	// debugging and a stray newline must not silently drop a capability.
	if !ParseCapabilities(" Converse \n").CanConverse() {
		t.Error("ParseCapabilities should tolerate surrounding whitespace and case")
	}
	// Unknown tokens degrade to "not held", never to a grant.
	if ParseCapabilities("converse-plus,admin,root").Any() {
		t.Error("unknown tokens must not grant anything")
	}
}

// TestConverseConfigFieldIsInvisibleWhenUnset guards the migration promise:
// existing hive.yaml files must round-trip byte-identically. A non-pointer
// bool, or one without omitempty, would start writing `converse: false` into
// every agent on the next Save().
func TestConverseConfigFieldIsInvisibleWhenUnset(t *testing.T) {
	var a config.AgentConfig
	out, err := yaml.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "converse") {
		t.Errorf("an agent that never set converse serialized it anyway:\n%s", out)
	}

	// Set it, and it must survive a round trip in both directions.
	yes := true
	a.Converse = &yes
	out, err = yaml.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "converse: true") {
		t.Errorf("converse: true did not serialize:\n%s", out)
	}
	var back config.AgentConfig
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Converse == nil || !*back.Converse {
		t.Errorf("converse did not survive the round trip: %v", back.Converse)
	}

	// Explicit false must stay distinguishable from unset, which is the whole
	// reason the field is a pointer: an operator who turns converse OFF on an
	// agent a pack turned ON must not have that read as "said nothing".
	no := false
	a.Converse = &no
	out, _ = yaml.Marshal(a)
	if !strings.Contains(string(out), "converse: false") {
		t.Errorf("explicit converse: false was dropped, so it is indistinguishable from unset:\n%s", out)
	}
}

// TestAgentCapabilitiesResolution: the manager's resolver honours the config
// field and nothing else.
func TestAgentCapabilitiesResolution(t *testing.T) {
	m := &Manager{}
	yes, no := true, false

	cases := []struct {
		name string
		cfg  *bool
		want bool
	}{
		{"unset defaults off", nil, false},
		{"explicit true", &yes, true},
		{"explicit false", &no, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ap := &AgentProcess{Name: "probe", Config: config.AgentConfig{Mode: "ADVISORY", Converse: tc.cfg}}
			if got := m.agentCapabilities(ap).CanConverse(); got != tc.want {
				t.Errorf("CanConverse() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWriteAgentCapsFileRevokes: the file is written even when empty. A stale
// file left behind would keep granting `converse` after an operator cleared it,
// which is the failure mode worth a test of its own.
func TestWriteAgentCapsFileRevokes(t *testing.T) {
	name := "capsprobe-4492"
	path := filepath.Join(agentStateDir, ".hive-caps-"+name)
	t.Cleanup(func() { os.Remove(path) })

	m := &Manager{logger: discardLogger()}

	m.writeAgentCapsFile(name, AgentCapabilities{Converse: true})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("caps file not written: %v", err)
	}
	if !ParseCapabilities(string(data)).CanConverse() {
		t.Fatalf("caps file %q does not grant converse", data)
	}

	m.writeAgentCapsFile(name, AgentCapabilities{})
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("caps file removed instead of emptied: %v", err)
	}
	if ParseCapabilities(string(data)).Any() {
		t.Fatalf("clearing converse left a granting caps file: %q", data)
	}
}

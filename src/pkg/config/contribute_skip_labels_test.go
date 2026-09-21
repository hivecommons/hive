package config

import (
	"reflect"
	"testing"
)

func TestContributeSkipLabelsDefaultSet(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	want := []string{"blocked", "tracking", "epic", "discussion", "question", "needs-decision", "needs-triage"}
	if got := c.Hub.ContributeSkipLabelPatterns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ContributeSkipLabelPatterns() = %v, want %v", got, want)
	}
}

func TestContributeSkipLabelsCustomSetAndBlockedFloor(t *testing.T) {
	h := HubConfig{ContributeSkipLabels: []string{"discussion", "wayfinder:*"}}
	want := []string{"discussion", "wayfinder:*", "blocked"}
	if got := h.ContributeSkipLabelPatterns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ContributeSkipLabelPatterns() = %v, want %v", got, want)
	}
}

func TestContributeSkipLabelsEnvOverride(t *testing.T) {
	t.Setenv(ContributeSkipLabelsEnvVar, "question, wayfinder:* ")
	c := &Config{}
	c.Hub.ContributeSkipLabels = []string{"discussion"}
	c.applyBootstrapEnv()
	c.applyDefaults()
	want := []string{"question", "wayfinder:*", "blocked"}
	if got := c.Hub.ContributeSkipLabelPatterns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("env ContributeSkipLabelPatterns() = %v, want %v", got, want)
	}
}

func TestContributeSkipLabelMatchCaseInsensitiveAndGlob(t *testing.T) {
	h := HubConfig{ContributeSkipLabels: []string{"wayfinder:*", "needs-decision"}}
	if label, ok := h.MatchContributeSkipLabel([]string{"Wayfinder:Grilling"}); !ok || label != "Wayfinder:Grilling" {
		t.Fatalf("glob match = (%q,%v), want Wayfinder:Grilling,true", label, ok)
	}
	if label, ok := h.MatchContributeSkipLabel([]string{"Needs-Decision"}); !ok || label != "Needs-Decision" {
		t.Fatalf("case match = (%q,%v), want Needs-Decision,true", label, ok)
	}
	if label, ok := h.MatchContributeSkipLabel([]string{"bug"}); ok || label != "" {
		t.Fatalf("non-match = (%q,%v), want empty,false", label, ok)
	}
}

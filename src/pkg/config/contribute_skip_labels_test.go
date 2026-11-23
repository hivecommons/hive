package config

import (
	"reflect"
	"testing"
)

func TestContributeSkipLabelsDefaultSet(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	want := []string{"blocked", "hive/already-done", "tracking", "epic", "discussion", "question", DefaultNeedsDecisionLabel, "needs-decision", "needs-triage"}
	if got := c.Hub.ContributeSkipLabelPatterns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ContributeSkipLabelPatterns() = %v, want %v", got, want)
	}
}

func TestContributeSkipLabelsCustomSetAndBlockedFloor(t *testing.T) {
	h := HubConfig{ContributeSkipLabels: []string{"discussion", "wayfinder:*"}}
	want := []string{"discussion", "wayfinder:*", "hive/already-done", "blocked", DefaultNeedsDecisionLabel}
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
	want := []string{"question", "wayfinder:*", "hive/already-done", "blocked", DefaultNeedsDecisionLabel}
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

func TestContributeNeedsDecisionLabelCustomizesSkipFloor(t *testing.T) {
	label := "2-discussing"
	h := HubConfig{ContributeSkipLabels: []string{"blocked"}, ContributeNeedsDecisionLabel: &label}
	want := []string{"blocked", "hive/already-done", "2-discussing"}
	if got := h.ContributeSkipLabelPatterns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ContributeSkipLabelPatterns() = %v, want %v", got, want)
	}
}

func TestContributeNeedsDecisionLabelEmptyDisablesAdditionalSkip(t *testing.T) {
	empty := ""
	h := HubConfig{ContributeSkipLabels: []string{"discussion"}, ContributeNeedsDecisionLabel: &empty}
	want := []string{"discussion", "hive/already-done", "blocked"}
	if got := h.ContributeSkipLabelPatterns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ContributeSkipLabelPatterns() = %v, want %v", got, want)
	}
}

// hivecommons/hive#13298: the reporter-trust awaiting label withholds an
// issue from the contributor relay only when its author is not trusted. A
// triage bot (Prow) puts needs-triage on every new issue, the hive's and a
// maintainer's included; a trusted author's issue is not skipped for that
// label alone, and every other skip label still skips it. Untrusted and
// unknown authors (authorTrusted false) are withheld exactly as before.
func TestMatchContributeSkipLabelForTrustedAuthor(t *testing.T) {
	h := HubConfig{}
	cases := []struct {
		name      string
		labels    []string
		trusted   bool
		wantLabel string
		wantSkip  bool
	}{
		{"untrusted needs-triage is skipped", []string{"kind/bug", "needs-triage"}, false, "needs-triage", true},
		{"trusted needs-triage alone is admitted", []string{"kind/bug", "needs-triage"}, true, "", false},
		{"trusted Needs-Triage (case) is admitted", []string{" Needs-Triage "}, true, "", false},
		{"trusted blocked still skips", []string{"needs-triage", "blocked"}, true, "blocked", true},
		{"trusted epic still skips", []string{"needs-triage", "epic"}, true, "epic", true},
		{"trusted question still skips", []string{"needs-triage", "question"}, true, "question", true},
		{"trusted needs-decision label still skips", []string{"needs-triage", DefaultNeedsDecisionLabel}, true, DefaultNeedsDecisionLabel, true},
		{"trusted legacy needs-decision still skips", []string{"needs-triage", LegacyNeedsDecisionLabel}, true, LegacyNeedsDecisionLabel, true},
		{"untrusted blocked skips", []string{"blocked"}, false, "blocked", true},
		{"plain labels never skip", []string{"kind/bug"}, true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, ok := h.MatchContributeSkipLabelFor(tc.labels, tc.trusted)
			if ok != tc.wantSkip || label != tc.wantLabel {
				t.Fatalf("MatchContributeSkipLabelFor(%v, %v) = (%q,%v), want (%q,%v)", tc.labels, tc.trusted, label, ok, tc.wantLabel, tc.wantSkip)
			}
		})
	}
	// The author-blind matcher keeps today's behaviour: an unknown author
	// is not trusted, so needs-triage still skips.
	if label, ok := h.MatchContributeSkipLabel([]string{"needs-triage"}); !ok || label != "needs-triage" {
		t.Fatalf("MatchContributeSkipLabel(needs-triage) = (%q,%v), want needs-triage,true", label, ok)
	}
	// needs-triage stays in the default set for untrusted reporters.
	found := false
	for _, p := range h.ContributeSkipLabelPatterns() {
		if p == DefaultReporterTrustAwaitingLabel {
			found = true
		}
	}
	if !found {
		t.Fatalf("default skip set lost %s: %v", DefaultReporterTrustAwaitingLabel, h.ContributeSkipLabelPatterns())
	}
	// The filter path agrees with the matcher.
	if d := h.EvaluateContributeFiltersFor("org/repo", "fix", "maintainer", []string{"needs-triage"}, true); !d.Admitted() {
		t.Fatalf("trusted author's needs-triage issue refused by the filters: %+v", d)
	}
	if d := h.EvaluateContributeFiltersFor("org/repo", "fix", "stranger", []string{"needs-triage"}, false); d.Reason != "skip_label" || d.Match != "needs-triage" {
		t.Fatalf("untrusted author's needs-triage issue = %+v, want skip_label needs-triage", d)
	}
	if d := h.EvaluateContributeFilters("org/repo", "fix", "stranger", []string{"needs-triage"}); d.Reason != "skip_label" {
		t.Fatalf("author-blind filters must still skip needs-triage: %+v", d)
	}
}

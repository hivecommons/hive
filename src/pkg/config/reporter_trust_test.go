package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestReporterTrust_ZeroValueChangesNothing is the regression pin: an absent
// reporter_trust block must admit every reporter, labelled or not, so no
// existing hive silently starts refusing the public's issues.
func TestReporterTrust_ZeroValueChangesNothing(t *testing.T) {
	var f IssueFilterConfig
	if !f.IsZero() {
		t.Fatal("zero-value filter must report IsZero")
	}
	if f.ReporterTrustEnabled() {
		t.Fatal("reporter trust must be OFF by default")
	}
	for _, tc := range []struct{ login, assoc string }{{"alice", "NONE"}, {"", ""}, {"bob", "FIRST_TIMER"}} {
		if !f.AdmitsReporter(nil, tc.login, tc.assoc) {
			t.Errorf("zero-value filter refused reporter %q/%q — absent config must change nothing", tc.login, tc.assoc)
		}
	}
}

func TestReporterTrust_Defaults(t *testing.T) {
	var r ReporterTrustConfig
	if got := r.EffectiveTrustedAssociations(); !equalStringSlices(got, []string{"OWNER", "MEMBER", "COLLABORATOR"}) {
		t.Errorf("default trusted associations = %v", got)
	}
	if got := r.EffectiveUntrustedRequireLabels(); !equalStringSlices(got, []string{"triage/accepted"}) {
		t.Errorf("default triage labels = %v", got)
	}
	if got := r.EffectiveAwaitingLabel(); got != "needs-triage" {
		t.Errorf("default awaiting label = %q", got)
	}
	if !r.CommentOn() {
		t.Error("reporter-trust wait comments must default on")
	}
	// CONTRIBUTOR is deliberately NOT trusted by default: one merged typo
	// fix does not make a stranger a maintainer.
	if r.Trusted("carol", "CONTRIBUTOR") {
		t.Error("CONTRIBUTOR must not be trusted by default")
	}
}

func TestReporterTrust_Trusted(t *testing.T) {
	r := ReporterTrustConfig{TrustedLogins: []string{"External-Maintainer"}}
	cases := []struct {
		login, assoc string
		want         bool
	}{
		{"alice", "OWNER", true},
		{"alice", "member", true}, // case-insensitive association
		{"alice", "COLLABORATOR", true},
		{"alice", "CONTRIBUTOR", false},
		{"alice", "NONE", false},
		{"alice", "", false},                  // unknown association fails toward triage
		{"", "", false},                       // unknown reporter fails toward triage
		{"external-maintainer", "NONE", true}, // explicit login wins, case-insensitive
		{"external-maintainer", "", true},     // explicit login needs no association
		{"", "OWNER", true},                   // association alone is enough when present
	}
	for _, tc := range cases {
		if got := r.Trusted(tc.login, tc.assoc); got != tc.want {
			t.Errorf("Trusted(%q, %q) = %v, want %v", tc.login, tc.assoc, got, tc.want)
		}
	}
	custom := ReporterTrustConfig{TrustedAssociations: []string{"OWNER"}}
	if custom.Trusted("alice", "MEMBER") {
		t.Error("a configured trust set replaces the default, it does not extend it")
	}
}

func TestReporterTrust_AdmitsReporter(t *testing.T) {
	f := IssueFilterConfig{ReporterTrust: ReporterTrustConfig{Enabled: boolPtr(true)}}
	if !f.ReporterTrustEnabled() {
		t.Fatal("gate should be enabled")
	}
	if !f.AdmitsReporter(nil, "maintainer", "MEMBER") {
		t.Error("positive control failed: a MEMBER's unlabelled issue must be admitted")
	}
	if f.AdmitsReporter([]string{"bug"}, "stranger", "NONE") {
		t.Error("a stranger's issue without the triage label was admitted")
	}
	if !f.AdmitsReporter([]string{"bug", "Triage/Accepted"}, "stranger", "NONE") {
		t.Error("a stranger's issue carrying the triage label (case-insensitive) must be admitted")
	}
	if f.AdmitsReporter([]string{"triage/accepted-maybe"}, "stranger", "NONE") {
		t.Error("prefix match must not satisfy the triage gate — that over-admits")
	}
	custom := IssueFilterConfig{ReporterTrust: ReporterTrustConfig{Enabled: boolPtr(true), UntrustedRequireLabels: []string{"ok-to-work"}}}
	if custom.AdmitsReporter([]string{"triage/accepted"}, "stranger", "NONE") {
		t.Error("a configured triage list replaces the default")
	}
	if !custom.AdmitsReporter([]string{"ok-to-work"}, "stranger", "NONE") {
		t.Error("configured triage label was not honoured")
	}
	// The ordinary require_labels allow-list still applies afterwards: this
	// method only answers the reporter question.
	both := IssueFilterConfig{RequireLabels: []string{"approved"}, ReporterTrust: ReporterTrustConfig{Enabled: boolPtr(true)}}
	if !both.AdmitsReporter(nil, "maintainer", "OWNER") {
		t.Error("reporter half must admit a trusted reporter regardless of require_labels")
	}
	if both.Admits(nil) {
		t.Error("label half must still refuse an unlabelled issue when require_labels is set")
	}
}

func TestReporterTrust_IsZeroAndEqual(t *testing.T) {
	a := IssueFilterConfig{ReporterTrust: ReporterTrustConfig{Enabled: boolPtr(true)}}
	if a.IsZero() {
		t.Error("an enabled reporter_trust block is not zero, so /api/config must expose it")
	}
	off := IssueFilterConfig{ReporterTrust: ReporterTrustConfig{Enabled: boolPtr(false)}}
	if off.IsZero() {
		t.Error("an explicit enabled: false is a configured value and must round-trip")
	}
	b := IssueFilterConfig{ReporterTrust: ReporterTrustConfig{Enabled: boolPtr(true)}}
	if !a.Equal(b) {
		t.Error("identical blocks must be Equal")
	}
	b.ReporterTrust.TrustedLogins = []string{"x"}
	if a.Equal(b) {
		t.Error("a differing trusted_logins list must not be Equal — the heartbeat reconcile keys on this")
	}
}

func TestValidateReporterTrust(t *testing.T) {
	if err := ValidateReporterTrust(ReporterTrustConfig{TrustedAssociations: []string{"owner", "MEMBER"}}); err != nil {
		t.Errorf("known associations (any case) must validate: %v", err)
	}
	if err := ValidateReporterTrust(ReporterTrustConfig{TrustedAssociations: []string{"MAINTAINER"}}); err == nil {
		t.Error("an association GitHub never reports must be rejected, or a typo silently narrows the trust set")
	}
	if err := ValidateReporterTrust(ReporterTrustConfig{UntrustedRequireLabels: []string{" "}}); err == nil {
		t.Error("a blank triage label must be rejected")
	}
	disabled := " "
	if got := (ReporterTrustConfig{AwaitingLabel: &disabled}).EffectiveAwaitingLabel(); got != "" {
		t.Errorf("blank awaiting label should disable labeling, got %q", got)
	}
}

func TestReporterTrustHold_Resolution(t *testing.T) {
	// nil follows the gate.
	var g GitHubConfig
	if g.ReporterTrustHoldEnabled(false) {
		t.Error("unset hold with the gate off must be off")
	}
	if !g.ReporterTrustHoldEnabled(true) {
		t.Error("unset hold with the gate on must follow it on")
	}
	// Explicit config wins over the gate.
	g.ReporterTrustHold = boolPtr(false)
	if g.ReporterTrustHoldEnabled(true) {
		t.Error("explicit false must win over an enabled gate")
	}
	g.ReporterTrustHold = boolPtr(true)
	if !g.ReporterTrustHoldEnabled(false) {
		t.Error("explicit true must win over a disabled gate")
	}
	// Env wins over everything.
	g.reporterTrustHoldEnvOverride = boolPtr(false)
	if g.ReporterTrustHoldEnabled(true) || !g.ReporterTrustHoldEnvOverrideSet() {
		t.Error("HIVE_REPORTER_TRUST_HOLD must force the effective value")
	}
}

func TestReporterTrustHold_PerRepo(t *testing.T) {
	c := &Config{}
	c.Project.Org = "acme"
	c.Project.Repos = []string{"widget", "gadget"}
	c.Project.IssueFilter.ReporterTrust.Enabled = boolPtr(true)
	if !c.ReporterTrustHoldEnabledForRepo("widget") {
		t.Fatal("with the gate on and no overrides, every repo holds")
	}
	c.SetReporterTrustHoldForRepos(map[string]*bool{"widget": boolPtr(false)})
	if c.ReporterTrustHoldEnabledForRepo("widget") {
		t.Error("per-repo false must switch the hold off for that repo only")
	}
	if !c.ReporterTrustHoldEnabledForRepo("gadget") {
		t.Error("the other repo must keep the hive-wide value")
	}
	rp, ok := c.RepoPolicyFor("widget")
	if !ok || rp.ReporterTrustHold == nil || *rp.ReporterTrustHold {
		t.Fatalf("override not recorded: %+v", rp)
	}
	c.SetReporterTrustHoldForRepos(map[string]*bool{"widget": nil})
	if _, ok := c.RepoPolicyFor("widget"); ok {
		t.Error("clearing the only override must drop the empty repo policy")
	}
	if !c.ReporterTrustHoldEnabledForRepo("widget") {
		t.Error("after clearing, the repo inherits the hive-wide value again")
	}
	// Env lock beats the per-repo override.
	c.SetReporterTrustHoldForRepos(map[string]*bool{"widget": boolPtr(false)})
	c.GitHub.reporterTrustHoldEnvOverride = boolPtr(true)
	if !c.ReporterTrustHoldEnabledForRepo("widget") {
		t.Error("env override must win over the per-repo override")
	}
}

func TestReporterTrust_ClankerRequestedOmittedIsZero(t *testing.T) {
	var r ReporterTrustConfig
	if err := yaml.Unmarshal([]byte("{}\n"), &r); err != nil {
		t.Fatal(err)
	}
	if r.ClankerRequested != nil || r.ClankerRequestedLabel != nil || r.ClankerRequestedAddendum != "" {
		t.Fatalf("omitted keys must be zero, got %+v", r)
	}
	if !r.IsZero() {
		t.Fatal("IsZero must be true when clanker keys are omitted")
	}
	out, err := yaml.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var again ReporterTrustConfig
	if err := yaml.Unmarshal(out, &again); err != nil {
		t.Fatal(err)
	}
	out2, _ := yaml.Marshal(again)
	if string(out) != string(out2) {
		t.Fatalf("round-trip not byte-identical: %q vs %q", out, out2)
	}
	if strings.Contains(string(out), "clanker") {
		t.Fatalf("zero value must not emit clanker keys: %q", out)
	}
}

func TestReporterTrust_ClankerRequestedDefaultsAndEqual(t *testing.T) {
	var r ReporterTrustConfig
	if r.ClankerRequestedOn() {
		t.Error("clanker_requested must default off")
	}
	if got := r.EffectiveClankerRequestedLabel(); got != DefaultClankerRequestedLabel || DefaultClankerRequestedLabel != "clanker-requested" {
		t.Errorf("default label = %q", got)
	}
	on, lbl := true, "  needs-bot "
	set := ReporterTrustConfig{ClankerRequested: &on, ClankerRequestedLabel: &lbl, ClankerRequestedAddendum: "x"}
	if set.IsZero() {
		t.Error("IsZero must be false once clanker keys are set")
	}
	if got := set.EffectiveClankerRequestedLabel(); got != "needs-bot" {
		t.Errorf("label = %q", got)
	}
	if set.Equal(r) || !set.Equal(set) {
		t.Error("Equal must account for clanker keys")
	}
	other := set
	other.ClankerRequestedAddendum = "y"
	if set.Equal(other) {
		t.Error("Equal must compare the addendum")
	}
}

func TestReporterTrust_ClankerRequestedValidation(t *testing.T) {
	blank := "   "
	if err := ValidateReporterTrust(ReporterTrustConfig{ClankerRequestedLabel: &blank}); err == nil {
		t.Error("whitespace-only label must be rejected")
	}
	long := ReporterTrustConfig{ClankerRequestedAddendum: strings.Repeat("a", MaxClankerRequestedAddendumLen+1)}
	if err := ValidateReporterTrust(long); err == nil {
		t.Error("over-long addendum must be rejected")
	}
	ok := ReporterTrustConfig{ClankerRequestedAddendum: strings.Repeat("a", MaxClankerRequestedAddendumLen)}
	if err := ValidateReporterTrust(ok); err != nil {
		t.Errorf("addendum at the cap must pass: %v", err)
	}
}

func TestReporterTrust_ClankerRequestedHoldLabels(t *testing.T) {
	off, on := false, true
	if got := (ReporterTrustConfig{ClankerRequested: &off}).ExtraHoldLabels(); len(got) != 0 {
		t.Errorf("off must add no hold labels, got %v", got)
	}
	if got := (ReporterTrustConfig{}).ExtraHoldLabels(); len(got) != 0 {
		t.Errorf("unset must add no hold labels, got %v", got)
	}
	got := (ReporterTrustConfig{ClankerRequested: &on}).ExtraHoldLabels()
	if !equalStringSlices(got, []string{"clanker-requested"}) {
		t.Errorf("on = %v", got)
	}
}

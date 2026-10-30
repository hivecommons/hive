package prstage

import "testing"

func TestHumanMergeRuleRequiresHumanMerge(t *testing.T) {
	cases := []struct {
		name   string
		rule   HumanMergeRule
		title  string
		labels []string
		want   bool
		reason string
	}{
		{name: "warning sign with VS16", title: "\u26a0\ufe0f drop v4", want: true, reason: "title"},
		{name: "bare warning sign", title: "\u26a0 drop v4", want: true, reason: "title"},
		{name: "leading whitespace trimmed", title: "  \t\u26a0\ufe0f drop v4", want: true, reason: "title"},
		{name: "sign inside the title is not a prefix", title: "fix: handle \u26a0\ufe0f icons", want: false},
		{name: "default label case-insensitive", title: "feat", labels: []string{"lgtm", " Needs-Owner-Merge "}, want: true, reason: "label:needs-owner-merge"},
		{name: "plain PR", title: "\u2728 add thing", labels: []string{"lgtm"}, want: false},
		{name: "configured prefix", rule: HumanMergeRule{TitlePrefixes: []string{"BREAKING:"}}, title: "BREAKING: rename", want: true, reason: "title"},
		{name: "configured prefix replaces default", rule: HumanMergeRule{TitlePrefixes: []string{"BREAKING:"}}, title: "\u26a0\ufe0f rename", want: false},
		{name: "empty prefix list turns the title half off", rule: HumanMergeRule{TitlePrefixes: []string{}}, title: "\u26a0\ufe0f rename", want: false},
		{name: "configured label", rule: HumanMergeRule{Labels: []string{"ship-by-owner"}}, title: "x", labels: []string{"SHIP-BY-OWNER"}, want: true, reason: "label:ship-by-owner"},
		{name: "empty label list turns the label half off", rule: HumanMergeRule{Labels: []string{}}, title: "x", labels: []string{"needs-owner-merge"}, want: false},
		{name: "blank entries are ignored", rule: HumanMergeRule{TitlePrefixes: []string{" ", ""}, Labels: []string{""}}, title: "anything", labels: []string{""}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := tc.rule.RequiresHumanMerge(tc.title, tc.labels)
			if got != tc.want || reason != tc.reason {
				t.Fatalf("RequiresHumanMerge(%q, %v) = %v %q, want %v %q", tc.title, tc.labels, got, reason, tc.want, tc.reason)
			}
		})
	}
}

func TestHumanMergeReasonMapsHumanMergeRequired(t *testing.T) {
	if got := HumanMergeReason(AutoMergeReasonHumanMergeRequired); got != HumanMergeReasonBreakingChange {
		t.Fatalf("HumanMergeReason(%q) = %q", AutoMergeReasonHumanMergeRequired, got)
	}
}

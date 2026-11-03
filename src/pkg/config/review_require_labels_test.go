package config

import "testing"

func TestReviewConfigAdmitsPRLabels(t *testing.T) {
	t.Run("empty gate admits every PR", func(t *testing.T) {
		var r ReviewConfig
		if !r.AdmitsPRLabels(nil) || !r.AdmitsPRLabels([]string{"anything"}) {
			t.Fatal("empty review.require_labels must admit every PR")
		}
		if r.RequireLabelsEnabled() {
			t.Fatal("empty gate must report disabled")
		}
	})
	t.Run("blank-only list is the empty gate", func(t *testing.T) {
		r := ReviewConfig{RequireLabels: []string{" ", ""}}
		if r.RequireLabelsEnabled() || !r.AdmitsPRLabels(nil) {
			t.Fatal("blank entries must not turn the gate on")
		}
	})
	t.Run("requires at least one label, case-insensitive exact", func(t *testing.T) {
		r := ReviewConfig{RequireLabels: []string{"hive-review", " approved-for-agents "}}
		if !r.RequireLabelsEnabled() {
			t.Fatal("expected gate on")
		}
		if r.AdmitsPRLabels(nil) {
			t.Fatal("unlabelled PR must be refused")
		}
		if r.AdmitsPRLabels([]string{"bug", "approved"}) {
			t.Fatal("prefix of an approval label must not admit")
		}
		if !r.AdmitsPRLabels([]string{"bug", "Hive-Review"}) {
			t.Fatal("case-insensitive match must admit")
		}
		if !r.AdmitsPRLabels([]string{"approved-for-agents"}) {
			t.Fatal("trimmed configured label must admit")
		}
	})
	t.Run("normalized drops blanks and duplicates", func(t *testing.T) {
		r := ReviewConfig{RequireLabels: []string{"a", " a ", "", "B", "b"}}
		got := r.NormalizedRequireLabels()
		if len(got) != 2 || got[0] != "a" || got[1] != "B" {
			t.Fatalf("unexpected normalized labels: %v", got)
		}
	})
}

package panes

import (
	"strings"
	"testing"
)

func TestHivesRepoFormCapturesHiveAndSubmitsOnce(t *testing.T) {
	overlay := NewHivesOverlay().SetHives([]HiveRow{{Name: "first", Scope: "2 repos"}, {Name: "second", Scope: "none"}}, "profiles.yml", "contributor.env", "balanced")
	if !strings.Contains(overlay.View(110), "2 repos") || !strings.Contains(overlay.View(110), "none") {
		t.Fatal("missing saved scope summaries")
	}
	overlay, ok := overlay.BeginRepo()
	if !ok || !overlay.Typing() {
		t.Fatal("form did not open")
	}
	if _, _, ok := overlay.Submit(); ok {
		t.Fatal("empty repository submitted")
	}
	overlay = overlay.Type("org/repo")
	overlay, action, ok := overlay.Submit()
	if !ok || action.Kind != HivesActionToggleRepo || action.Name != "first" || action.Repository != "org/repo" {
		t.Fatalf("wrong action: %+v", action)
	}
	if _, _, ok := overlay.Submit(); ok {
		t.Fatal("pending action submitted twice")
	}
	if _, ok := overlay.BeginRepo(); ok {
		t.Fatal("pending write allowed another form")
	}
}

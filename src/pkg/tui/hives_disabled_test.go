package tui

import (
	"strings"
	"testing"
)

func TestHivesTogglePreservesCredentialsAndSignalsRelay(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	original := h.profiles(t).Profiles[0]
	h.run(t, h.send(t, key("e")))
	disabled := h.profiles(t).Profiles[0]
	if !disabled.Disabled {
		t.Fatal("e did not disable selected hive")
	}
	disabled.Disabled = false
	if disabled != original {
		t.Fatal("toggle changed credentials or metadata")
	}
	if !strings.Contains(h.view(), "[disabled]") {
		t.Fatal("disabled hive not marked in overlay")
	}
	if h.signals.Load() != 1 {
		t.Fatal("toggle did not signal running relay")
	}
	h.run(t, h.send(t, key("e")))
	if h.profiles(t).Profiles[0] != original {
		t.Fatal("second e did not restore original profile")
	}
	if h.signals.Load() != 2 || h.regs.Load() != 0 || h.logins.Load() != 0 {
		t.Fatal("toggle must signal without registering or logging in")
	}
}

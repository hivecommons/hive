package commands

import (
	"strings"
	"testing"
)

func TestHivesEnableDisablePersistAndSignal(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	for _, verb := range []string{"disable", "disable", "enable"} {
		before := h.profiles(t).Profiles[0]
		if err := h.run(t, "", "hives", verb, "acme"); err != nil {
			t.Fatal(err)
		}
		after := h.profiles(t).Profiles[0]
		if after.Disabled != (verb == "disable") {
			t.Fatalf("%s did not change disabled state", verb)
		}
		after.Disabled = before.Disabled
		if after != before {
			t.Fatal("toggle changed profile credentials or metadata")
		}
	}
	if h.signalCalls != 3 {
		t.Fatalf("signals = %d, want 3", h.signalCalls)
	}
	if len(h.reg.calls) != 0 || len(h.reissuer.calls) != 0 {
		t.Fatal("toggle must not register or rotate credentials")
	}
	if err := h.run(t, "", "hives", "disable", "missing"); err == nil {
		t.Fatal("unknown profile accepted")
	}
	if h.signalCalls != 3 {
		t.Fatal("failed toggle signaled relay")
	}
	if err := h.run(t, "", "hives", "disable", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "acme (disabled)") {
		t.Fatalf("disabled status missing: %s", h.out.String())
	}
	if err := h.run(t, "", "hives", "list", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), `"disabled": true`) {
		t.Fatalf("JSON lacks status: %s", h.out.String())
	}
}

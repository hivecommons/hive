package stageengine

import "testing"

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": DefaultMode, " Shadow ": ModeShadow, "enforce": ModeEnforce, "ENFORCE": ModeEnforce} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseMode("loud"); err == nil {
		t.Fatal("ParseMode(loud) must fail")
	}
	if _, err := ParseMode("off"); err == nil {
		t.Fatal("ParseMode(off) must fail: the legacy pass-through is gone")
	}
	if Mode("loud").Valid() || !ModeEnforce.Valid() || ModeShadow.String() != "shadow" {
		t.Fatal("Valid/String")
	}
}

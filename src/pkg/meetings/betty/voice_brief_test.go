package betty

import (
	"strings"
	"testing"
)

func TestBriefVoice(t *testing.T) {
	first := "Board pressure is twenty open issues and fifteen open PRs across fourteen repos."
	long := first + " Of those, eighteen are actionable right now and two are blocked."
	got, ok := BriefVoice(long)
	if !ok || got != first {
		t.Fatalf("long answer: %q, %v", got, ok)
	}
	for name, in := range map[string]string{
		"short":              "It ships Friday. Really.",
		"one long sentence":  strings.Repeat("word ", 40) + "end.",
		"first too long":     strings.Repeat("word ", 30) + "end. Second sentence here.",
		"no sentence breaks": strings.Repeat("m", 150),
	} {
		if out, ok := BriefVoice(in); ok {
			t.Fatalf("%s: briefed to %q", name, out)
		}
	}
}

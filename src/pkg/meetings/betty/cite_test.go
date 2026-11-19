package betty

import (
	"strings"
	"testing"
)

func TestCiteSources(t *testing.T) {
	snips := []Snippet{
		{Source: "web example.com", Title: "Cert docs", Text: "Renews nightly (https://example.com/certs)"},
		{Source: "work item hive-42", Title: "Rotate keys", Text: "open"},
		{Source: "knowledge", Title: "Release plan", Text: "Friday"},
		{Source: HiveStatusSource, Title: "The hive right now", Text: "40 issues"},
	}
	for _, c := range []struct {
		text string
		used []string
		want string
	}{
		{"It renews nightly.", []string{"1", "2"}, "It renews nightly. (Source: https://example.com/certs; hive-42)"},
		{"It renews nightly.", []string{"1", "2", "3"}, "It renews nightly. (Source: https://example.com/certs; hive-42)"},
		{"Friday.", []string{"[3]"}, `Friday. (Source: knowledge "Release plan")`},
		{"40 open issues.", []string{"4"}, "40 open issues. (Source: hive live status)"},
		{"See https://example.com/certs.", []string{"1"}, "See https://example.com/certs."},
		{"No sources.", nil, "No sources."},
		{"Bad index.", []string{"9", "x"}, "Bad index."},
	} {
		if got := citeSources("en", c.text, c.used, snips); got != c.want {
			t.Errorf("citeSources(%q, %v) = %q, want %q", c.text, c.used, got, c.want)
		}
	}
	if got := citeSources("de-DE", "Freitag.", []string{"3"}, snips); !strings.HasPrefix(got, "Freitag. (Quelle: ") {
		t.Fatalf("localized %q", got)
	}
}

func TestHedge(t *testing.T) {
	for in, want := range map[string]string{
		"It ships Friday.":            "I'm not sure, but it ships Friday.",
		"Not sure, maybe Friday.":     "Not sure, maybe Friday.",
		"I don't know yet.":           "I don't know yet.",
		"PR 12 fixed it.":             "I'm not sure, but PR 12 fixed it.",
		"I think it ships on Friday.": "I'm not sure, but I think it ships on Friday.",
	} {
		if got := hedge("en", in); got != want {
			t.Errorf("hedge(%q) = %q, want %q", in, got, want)
		}
	}
}

// A low-confidence addressed answer is said as unsure, not as fact; the
// hive's live state is never hedged; spoken answers carry no citation.
func TestAddressedAnswerHedgedAndCited(t *testing.T) {
	llm := &fakeLLM{reply: func(string, string) (string, error) {
		return `{"answer":"It ships Friday.","confidence":0.4,"sources":[1]}`, nil
	}}
	r := newRig(t, testSettings(), llm)
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship?"})
	if a := r.reply(); a.Text != `I'm not sure, but it ships Friday. (Source: knowledge "Release")` {
		t.Fatalf("hedged %q", a.Text)
	}
	r.e.Observe(Line{Who: "Bo", Text: "Betty, when does the release ship, say it out loud?", Spoken: true})
	for {
		a := r.reply()
		if a.Trigger == TriggerAck {
			continue
		}
		if a.Text != "I'm not sure, but it ships Friday." {
			t.Fatalf("spoken %q", a.Text)
		}
		break
	}
}

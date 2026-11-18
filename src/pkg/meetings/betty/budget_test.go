package betty

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func budgetTestEngine(t *testing.T, llm LLM) *Engine {
	t.Helper()
	return New(SettingsFrom(config.MeetingsPersonaConfig{}.Resolve()), Deps{LLM: llm, CanResearch: true})
}

// At the degrade step an answer is one short sentence, and the prompt says
// so; before it, answers keep the normal bounds.
func TestDegradedAnswersAreShortened(t *testing.T) {
	e := budgetTestEngine(t, &fakeLLM{})
	long := "The release ships on Friday after the freeze. The notes are in the wiki. Ping the owner for the rest of the details, which are many."
	normal := e.clipper(false)(long)
	if !strings.Contains(normal, "wiki") || e.degradedNote() != "" {
		t.Fatalf("normal answers keep their bounds: %q", normal)
	}
	e.SetDegraded()
	short := e.clipper(false)(long)
	if short != "The release ships on Friday after the freeze." || len(short) > DegradedMaxChars {
		t.Fatalf("degraded answer %q", short)
	}
	if !strings.Contains(e.degradedNote(), "one short sentence") {
		t.Fatalf("degraded prompt note %q", e.degradedNote())
	}
	if v := e.clipper(true)(long); len(v) > DegradedMaxChars || strings.Count(v, ".") > DegradedMaxSentences {
		t.Fatalf("degraded voice answer %q", v)
	}
}

// A question dropped for budget reaches the observer with who asked and
// what, so the session can record it.
func TestBudgetSkipReportsTheQuestion(t *testing.T) {
	e := budgetTestEngine(t, &fakeLLM{})
	var got []BudgetSkip
	e.OnBudgetSkip(func(s BudgetSkip) { got = append(got, s) })
	e.SetExhausted()
	e.budgetSkip(Line{Who: "Ana", Text: "when does it ship?"}, true)
	if len(got) != 1 || got[0].Who != "Ana" || got[0].Text != "when does it ship?" || !got[0].Addressed {
		t.Fatalf("skip %+v", got)
	}
}

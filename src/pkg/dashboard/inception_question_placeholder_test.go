package dashboard

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func TestInceptionQuestionsRejectKickTemplate(t *testing.T) {
	template, err := os.ReadFile("../policies/defaults/brainstorm-advisory.md")
	if err != nil {
		t.Fatal(err)
	}
	capture := strings.Split(string(template), "### If phase is `clarify`")[0]
	if got := parseQuestionTable(strings.Split(capture, "\n")); len(got) != 0 {
		t.Fatalf("echoed kick produced questions: %+v", got)
	}
}

func TestInceptionQuestionParsersSkipPlaceholders(t *testing.T) {
	w, _, _ := covFWatcher(t)
	for _, text := range []string{
		"<question text>",
		"<question text> This table format is REQUIRED — the inception engine parses it to detect questions. Use",
		"Clarification: <question text>",
		"clarification: <question title>",
		"  YOUR QUESTION HERE  ",
		"   ",
	} {
		t.Run(text, func(t *testing.T) {
			if q := w.parseQuestionFromBdCreate(fmt.Sprintf(`bd create --title %q`, text)); q != nil {
				t.Errorf("command placeholder accepted: %+v", q)
			}
			table := []string{"│ 1 │ users │ " + text + " │ <default value> │", "│ 2 │ users │ Who will use the issue classifier? │ triagers │"}
			got := parseQuestionTable(table)
			if len(got) != 1 || got[0].Text != "Who will use the issue classifier?" {
				t.Errorf("table questions = %+v", got)
			}
			numbered := []string{"1. Users — " + text, "2. Users — Who will use the issue classifier?"}
			got = parseNumberedQuestions(numbered)
			if len(got) != 1 || got[0].Text != "Who will use the issue classifier?" {
				t.Errorf("numbered questions = %+v", got)
			}
		})
	}
}

func TestInceptionPlaceholderQuestionsDoNotAdvance(t *testing.T) {
	for _, source := range []string{"beads", "pluk"} {
		t.Run(source, func(t *testing.T) {
			w, eng, _ := covFWatcher(t)
			state, err := eng.Start("an agent that categorises GitHub issues")
			if err != nil {
				t.Fatal(err)
			}
			var questionBeads []*beads.Bead
			for i := 0; i < minQuestionsForAdvance; i++ {
				// Distinct text prevents deduplication from masking missing validation.
				text := fmt.Sprintf("<question text> instruction %d", i)
				questionBeads = append(questionBeads, &beads.Bead{Title: "Clarification: " + text})
				w.plukQuestions = append(w.plukQuestions, knowledge.Question{ID: fmt.Sprint(i), Text: text, Category: "general"})
			}
			if source == "beads" {
				w.checkForQuestions(questionBeads)
			} else {
				w.applyPlukQuestions(state)
			}
			if got := eng.GetState(); got.Phase != knowledge.PhaseCapture || len(got.Questions) != 0 {
				t.Fatalf("placeholder questions advanced inception: %+v", got)
			}
			// A rejected batch must not prevent subsequent real questions from advancing.
			w.plukQuestions = nil
			questionBeads = nil
			for i := 0; i < minQuestionsForAdvance; i++ {
				text := fmt.Sprintf("Should the classifier support label taxonomy %d?", i)
				questionBeads = append(questionBeads, &beads.Bead{Title: "Clarification: " + text})
				w.plukQuestions = append(w.plukQuestions, knowledge.Question{ID: fmt.Sprint(i), Text: text, Category: "general"})
			}
			if source == "beads" {
				w.checkForQuestions(questionBeads)
			} else {
				w.applyPlukQuestions(state)
			}
			if got := eng.GetState(); got.Phase != knowledge.PhaseClarify || len(got.Questions) != minQuestionsForAdvance {
				t.Fatalf("real questions did not advance inception: %+v", got)
			}
		})
	}
}

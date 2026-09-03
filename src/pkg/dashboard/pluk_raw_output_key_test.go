package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// pluk_raw_output_key_test.go covers the third defect in kubestellar/hive#4285:
// handlePlukEvent read the raw_output text from data["message"], but pluk puts
// it on data["line"] and only there — classifier.js builds the event as
// createEvent(..., 'raw_output', { line }). "message" is a real pluk key, but it
// belongs to rate_limit and error.
//
// The consequence was total rather than partial: line came back "" for every
// raw_output event, the handler returned immediately, and every bd-create
// interception and keyword re-poll underneath it was unreachable.

// TestPlukRawOutputReadsLineKey is the regression. A bd create line arriving on
// the key pluk actually uses must reach the buffer the capture phase parses.
func TestPlukRawOutputReadsLineKey(t *testing.T) {
	w, eng, _ := covFWatcher(t)
	if _, err := eng.Start("raw output key idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const bdCreate = `bd create --title "Who are the users?" --actor brainstorm`
	w.handlePlukEvent(plukEvent{
		Type: "raw_output",
		Data: map[string]string{"line": bdCreate},
	})

	w.plukMu.Lock()
	buffered := append([]string(nil), w.plukBdCreateLines...)
	w.plukMu.Unlock()

	if len(buffered) == 0 {
		t.Fatalf("#4285: a raw_output event carrying %q on the \"line\" key was dropped — "+
			"the handler is reading a key pluk does not set for this event type", bdCreate)
	}
	if buffered[0] != bdCreate {
		t.Fatalf("buffered line = %q, want %q", buffered[0], bdCreate)
	}
}

// TestPlukRawOutputIgnoresMessageKey is the other half, and the one that keeps
// the fix from being quietly reverted into "read both keys". pluk never puts
// raw_output text on "message"; treating it as an alias would mean acting on
// rate_limit and error prose as though it were pane output — those events carry
// human-readable provider text, and "bd create --title" appearing inside an
// error message is not a bd create the agent ran.
func TestPlukRawOutputIgnoresMessageKey(t *testing.T) {
	w, eng, _ := covFWatcher(t)
	if _, err := eng.Start("message key idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	w.handlePlukEvent(plukEvent{
		Type: "raw_output",
		Data: map[string]string{"message": `bd create --title "Should not be read" --actor brainstorm`},
	})

	w.plukMu.Lock()
	buffered := len(w.plukBdCreateLines)
	w.plukMu.Unlock()

	if buffered != 0 {
		t.Fatalf("#4285: raw_output text was read from \"message\"; pluk only ever sets " +
			"\"line\" for this event, and \"message\" belongs to rate_limit/error")
	}
}

// TestPlukRawOutputParsesQuestionsThroughTheLineKey walks the path end to end:
// enough bd create lines on the real key must actually populate the question
// list, which is the behaviour the capture phase depends on and which had been
// unreachable since the key mismatch was introduced.
func TestPlukRawOutputParsesQuestionsThroughTheLineKey(t *testing.T) {
	w, eng, _ := covFWatcher(t)
	if _, err := eng.Start("question parsing idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseCapture {
		t.Skipf("inception did not start in capture phase; nothing to drive")
	}

	for _, title := range []string{
		"Who are the users?",
		"What is the deployment target?",
		"Which data must persist?",
	} {
		w.handlePlukEvent(plukEvent{
			Type: "raw_output",
			Data: map[string]string{"line": `bd create --title "` + title + `" --actor brainstorm`},
		})
	}

	w.plukMu.Lock()
	questions := len(w.plukQuestions)
	buffered := len(w.plukBdCreateLines)
	w.plukMu.Unlock()

	if buffered != 3 {
		t.Fatalf("buffered %d bd create lines, want 3", buffered)
	}
	if questions == 0 {
		t.Fatalf("#4285: no questions were parsed from bd create lines delivered on the " +
			"\"line\" key — the capture-phase interception is still unreachable")
	}
}

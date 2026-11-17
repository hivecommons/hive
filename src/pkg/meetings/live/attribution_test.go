package live

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
)

// heldLLM answers the release question only once release is closed, so a
// second request overlaps it.
type heldLLM struct {
	personaLLM
	release chan struct{}
}

func (h *heldLLM) Complete(ctx context.Context, system, prompt string) (string, error) {
	if strings.Contains(prompt, "Question from Bo") {
		select {
		case <-h.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return h.personaLLM.Complete(ctx, system, prompt)
}

// Overlapping requests (hivecommons/hive#18590): one queue note in chat,
// not charged, and each answer opens with the topic it answers.
func TestOverlappingRequestsQueueNoteAndAttribution(t *testing.T) {
	llm := &heldLLM{release: make(chan struct{})}
	h := personaHarness(t, Policy{ChatPostsPerMinute: 20}, config.MeetingsPersonaConfig{}, llm)
	h.live()
	h.line(meetings.EventChat, "Bo", "Betty, when does the release ship?", false)
	h.line(meetings.EventChat, "Cy", "Betty, who owns the deploy pipeline?", false)
	h.eventually("queue note", h.posted("I have 2 questions; answering each as it's ready."))
	h.eventually("second request answered first", h.posted("On owns the deploy pipeline: "))
	close(llm.release)
	h.eventually("first request answered", h.posted("On release ship: "))
	if n := h.countPosts("I have 2 questions"); n != 1 {
		t.Fatalf("queue notes %d, want 1", n)
	}
	if st := h.state(); st.PersonaReplies != 2 {
		t.Fatalf("persona replies %d: the note is not an answer", st.PersonaReplies)
	}
}

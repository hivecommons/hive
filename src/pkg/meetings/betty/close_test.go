package betty

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/meetings/meetllm"
)

// closingLLM records Close.
type closingLLM struct{ closed int }

func (c *closingLLM) Complete(context.Context, string, string) (string, error) { return "", nil }
func (c *closingLLM) Close() error                                             { c.closed++; return nil }

// A model holding a process (the warm meeting CLI, #18517) is closed with
// the engine, once, started or not.
func TestEngineCloseClosesModel(t *testing.T) {
	m := &closingLLM{}
	e := New(Settings{}, Deps{LLM: m})
	e.Start(context.Background())
	e.Close()
	e.Close()
	if m.closed != 1 {
		t.Fatalf("closed %d times", m.closed)
	}
	m2 := &closingLLM{}
	New(Settings{}, Deps{LLM: m2}).Close()
	if m2.closed != 1 {
		t.Fatalf("unstarted engine closed the model %d times", m2.closed)
	}
}

// MeetLLM closes its warm session; without one Close is a no-op.
func TestMeetLLMClose(t *testing.T) {
	if err := (MeetLLM{}).Close(); err != nil {
		t.Fatal(err)
	}
	s, err := meetllm.NewCLISession(meetllm.Config{Backend: meetllm.BackendClaude})
	if err != nil {
		t.Fatal(err)
	}
	if err := (MeetLLM{Config: meetllm.Config{Session: s}}).Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Warm(); err == nil {
		t.Fatal("want the closed session to refuse a start")
	}
}

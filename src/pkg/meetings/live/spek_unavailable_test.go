package live

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/meetings"
)

// syncLog is a goroutine-safe log sink.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// end ends the meeting and waits for its session to finish, so every
// update pushed before it has been applied.
func (h *harness) end() {
	h.t.Helper()
	ended := h.m
	ended.State = meetings.StateEnded
	h.mt.push(meetings.Update{Meeting: ended})
	h.eventually("session gone", func() bool { _, ok := h.r.session(h.m.ID); return !ok })
}

// unsupportedSpek is a Spek for a spektacular without the meeting command:
// it is done from the start and says so (spektacular.Stream.Unsupported).
type unsupportedSpek struct{ *fakeSpek }

func (unsupportedSpek) Unsupported() bool { return true }

// TestSpekUnsupportedNoForwardingNoWarnings pins hivecommons/hive#18630: a
// spektacular without `meeting` turns live mode off for the meeting at once
// (the panel says notes and questions are off), and transcript lines are
// not forwarded or warned about one by one.
func TestSpekUnsupportedNoForwardingNoWarnings(t *testing.T) {
	logs := &syncLog{}
	spek := unsupportedSpek{newFakeSpek()}
	spek.Close(nil) // done from the start: Lines() is closed
	h := newHarness(t, Policy{}, func(o *Options) {
		o.NewSpek = func(meetings.Meeting) Spek { return spek }
		o.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	h.live()
	h.eventually("stopped status", func() bool {
		return strings.HasPrefix(h.state().SpekStatus, "Notes and clarifying questions are off for this meeting")
	})
	h.line(meetings.EventSegment, "Ana", "let's start", true)
	h.line(meetings.EventChat, "Bo", "agenda?", true)
	h.line(meetings.EventSegment, "Ana", "first item", true)
	h.end()
	if got := spek.stdin(); len(got) != 0 {
		t.Fatalf("lines forwarded to an unsupported spek: %v", got)
	}
	if out := logs.String(); strings.Contains(out, "forwarding to spek") || strings.Contains(out, "level=WARN") {
		t.Fatalf("unexpected warnings: %s", out)
	}
}

// TestSpekForwardingWarnsOncePerOutage pins that a child that is down
// between restarts warns on the first failed forward, not on every line.
func TestSpekForwardingWarnsOncePerOutage(t *testing.T) {
	logs := &syncLog{}
	h := newHarness(t, Policy{}, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	h.live()
	h.spek.mu.Lock()
	h.spek.sendErr = errors.New("spektacular: stream child is not running")
	h.spek.mu.Unlock()
	for _, text := range []string{"one", "two", "three"} {
		h.line(meetings.EventSegment, "Ana", text, true)
	}
	h.eventually("paused status", func() bool {
		return strings.HasPrefix(h.state().SpekStatus, "Notes and clarifying questions are paused")
	})
	h.end()
	if n := strings.Count(logs.String(), "forwarding to spek"); n != 1 {
		t.Fatalf("forwarding warnings = %d, want 1:\n%s", n, logs.String())
	}
}

package live

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
)

// syncBuf is a log sink safe for the session goroutine and the test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const skippedQuestion = "Betty, when does the release ship?"

func TestBudgetWarningFiresOncePerMeeting(t *testing.T) {
	logs := &syncBuf{}
	h := newHarness(t, Policy{MaxTokensPerMeeting: 100}, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	h.live()
	h.spek.intent(`{"type":"notes.append","section":"notes","text":"secret transcript words"}`)
	h.spek.intent(`{"type":"chat.post","text":"a","usage":{"total_tokens":81}}`)
	h.eventually("warned", func() bool { return h.state().BudgetWarned })
	h.spek.intent(`{"type":"chat.post","text":"b","usage":{"total_tokens":2}}`)
	h.spek.intent(`{"type":"chat.post","text":"c","usage":{"total_tokens":2}}`)
	h.eventually("tokens counted", func() bool { return h.state().Tokens == 85 })
	if n := strings.Count(logs.String(), "meeting budget at 80%"); n != 1 {
		t.Fatalf("the 80 percent warning must fire once per meeting, fired %d times:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "secret transcript words") {
		t.Fatal("no transcript text in logs")
	}
	if st := h.state(); st.BudgetDegraded || st.BudgetExhausted {
		t.Fatalf("85 percent is a warning only: %+v", st)
	}
}

func TestNoResearchStartedAtNinetyPercent(t *testing.T) {
	h := newHarness(t, Policy{MaxTokensPerMeeting: 100}, nil)
	h.live()
	h.spek.intent(`{"type":"chat.post","text":"a","usage":{"total_tokens":91}}`)
	h.eventually("degraded", func() bool { return h.state().BudgetDegraded })
	h.spek.intent(`{"type":"research","rid":"r1","query":"how do certs rotate"}`)
	h.eventually("research dropped", func() bool { return h.state().Dropped == 1 })
	h.res.mu.Lock()
	n := h.res.n
	h.res.mu.Unlock()
	if n != 0 || len(h.state().Research) != 0 {
		t.Fatalf("research must not start at 90 percent: dispatched %d, items %+v", n, h.state().Research)
	}
}

func TestDegradeNoticeSaidOnce(t *testing.T) {
	h := personaHarness(t, Policy{ChatPostsPerMinute: 20, MaxTokensPerMeeting: 100}, config.MeetingsPersonaConfig{}, &personaLLM{})
	h.live()
	h.spek.intent(`{"type":"notes.append","section":"notes","text":"n"}`)
	h.spek.intent(`{"type":"chat.post","text":"a","usage":{"total_tokens":92}}`)
	h.eventually("degrade notice", h.posted("answers will be shorter"))
	h.spek.intent(`{"type":"chat.post","text":"b","usage":{"total_tokens":2}}`)
	h.eventually("tokens counted", func() bool { return h.state().Tokens == 94 })
	if n := h.countPosts("answers will be shorter"); n != 1 {
		t.Fatalf("the degrade notice is said once, said %d times", n)
	}
}

func TestQuestionDroppedForBudgetIsRecordedAndShown(t *testing.T) {
	h := personaHarness(t, Policy{ChatPostsPerMinute: 20, MaxTokensPerMeeting: 100}, config.MeetingsPersonaConfig{}, &personaLLM{})
	h.live()
	h.spek.intent(`{"type":"chat.post","text":"a","usage":{"total_tokens":150}}`)
	h.eventually("exhausted", func() bool { return h.state().BudgetExhausted })
	h.eventually("notice says what to do", h.posted("raise the budget or start a new meeting"))
	h.line(meetings.EventChat, "Ana", skippedQuestion, false)
	h.eventually("skipped recorded", func() bool { return h.state().SkippedCount == 1 })
	st := h.state()
	if len(st.Skipped) != 1 || st.Skipped[0].Text != skippedQuestion || st.Skipped[0].Who != "Ana" {
		t.Fatalf("the skipped question must be kept with its text: %+v", st.Skipped)
	}
	// The count reaches the room, and the meter and inbox note carry it.
	h.eventually("count in the notice", h.posted("1 question(s) not answered"))
	if v := NewBudgetView(Policy{MaxTokensPerMeeting: 100}, st, "mtg-1"); v.Skipped != 1 || v.Level != BudgetLevelExhausted || v.Percent != 100 {
		t.Fatalf("meter %+v", v)
	}
	if note := RenderSkippedNote(st); !strings.Contains(note, "Not answered: budget") || !strings.Contains(note, skippedQuestion) {
		t.Fatalf("inbox note %q", note)
	}
	if h.countPosts("release ships") != 0 {
		t.Fatal("no answer past the budget")
	}
}

func TestSkippedQuestionsSurviveRestart(t *testing.T) {
	h := personaHarness(t, Policy{ChatPostsPerMinute: 20, MaxTokensPerMeeting: 100}, config.MeetingsPersonaConfig{}, &personaLLM{})
	h.live()
	h.spek.intent(`{"type":"chat.post","text":"a","usage":{"total_tokens":150}}`)
	h.eventually("exhausted", func() bool { return h.state().BudgetExhausted })
	h.line(meetings.EventChat, "Ana", skippedQuestion, false)
	h.eventually("skipped", func() bool { return h.state().SkippedCount == 1 })
	h.r.Close()
	var st State
	if err := h.store.LoadDoc(h.m.ID, StateDoc, &st); err != nil {
		t.Fatal(err)
	}
	if st.SkippedCount != 1 || len(st.Skipped) != 1 {
		t.Fatalf("the skipped question must be persisted: %+v", st)
	}
}

func TestBudgetViewHasNoQuestionText(t *testing.T) {
	st := State{Tokens: 62, Intents: 3, SkippedCount: 1, Skipped: []SkippedQuestion{{Who: "Ana", Text: skippedQuestion}}}
	v := NewBudgetView(Policy{MaxTokensPerMeeting: 100, MaxIntentsPerMeeting: 500}, st, "mtg-1")
	raw, err := json.Marshal(NewBudgetOverviewForTest(v))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "release") || strings.Contains(string(raw), "Ana") {
		t.Fatalf("the meter every role reads must carry no question text: %s", raw)
	}
	if v.Percent != 62 || v.Level != BudgetLevelOK || v.Skipped != 1 || v.IntentsAnswered != 3 {
		t.Fatalf("view %+v", v)
	}
	for pct, want := range map[int]string{79: BudgetLevelOK, BudgetWarnPercent: BudgetLevelWarning, BudgetDegradePercent: BudgetLevelDegraded} {
		if got := BudgetLevelFor(pct, false); got != want {
			t.Fatalf("level at %d = %s, want %s", pct, got, want)
		}
	}
}

// NewBudgetOverviewForTest wraps a view the way the API serves it.
func NewBudgetOverviewForTest(v BudgetView) BudgetOverview {
	return BudgetOverview{TokenBudget: v.TokenBudget, Meetings: []BudgetView{v}}
}

func TestLedgerUsage(t *testing.T) {
	l := NewLedger(10, 100)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	l.Add(now.Add(-2*ledgerHour), 30)
	l.Add(now.Add(-time.Minute), 7)
	u := l.Usage(now)
	if u.HourlyUsed != 7 || u.DailyUsed != 37 || u.HourlyCeiling != 10 || u.DailyCeiling != 100 {
		t.Fatalf("usage %+v", u)
	}
}

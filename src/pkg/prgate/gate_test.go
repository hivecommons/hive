package prgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

const (
	headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newGate(api API, mode Mode) *Gate {
	return New(api, func(string) Mode { return mode }, quiet())
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeOff, "off": ModeOff, " Shadow ": ModeShadow, "ENFORCE": ModeEnforce} {
		if got, ok := ParseMode(in); !ok || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := ParseMode("on"); ok {
		t.Error("ParseMode accepted an unknown mode")
	}
}

func TestHandleEventTable(t *testing.T) {
	open := Subject{HeadSHA: headA, Title: "✨ add thing", State: "open"}
	tests := []struct {
		name      string
		mode      Mode
		subject   Subject
		readErr   error
		eventHead string
		wantSkip  string
		wantPosts []string
		wantReads int
	}{
		{"mode off makes no API call at all", ModeOff, open, nil, headA, SkipModeOff, nil, 0},
		{"shadow posts only the shadow name", ModeShadow, open, nil, headA, "", []string{ContextVerifyShadow}, 1},
		{"enforce without a confirmed grant runs as shadow", ModeEnforce, open, nil, headA, "", []string{ContextVerifyShadow}, 1},
		{"draft is skipped", ModeShadow, Subject{HeadSHA: headA, Title: "✨ x", State: "open", Draft: true}, nil, headA, SkipDraft, nil, 1},
		{"closed is skipped", ModeShadow, Subject{HeadSHA: headA, Title: "✨ x", State: "closed"}, nil, headA, SkipNotOpen, nil, 1},
		{"a moved head is skipped", ModeShadow, Subject{HeadSHA: headB, Title: "✨ x", State: "open"}, nil, headA, SkipHeadMoved, nil, 1},
		{"a malformed event head is refused before any read", ModeShadow, open, nil, "HEAD", SkipBadHead, nil, 0},
		{"an unreadable PR posts nothing", ModeShadow, open, errors.New("boom"), headA, SkipReadFailed, nil, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := &recordingAPI{subject: tc.subject, readErr: tc.readErr}
			out := newGate(api, tc.mode).HandleEvent(context.Background(), "o/r", 7, tc.eventHead)
			if out.Skipped != tc.wantSkip {
				t.Errorf("skipped = %q, want %q", out.Skipped, tc.wantSkip)
			}
			if strings.Join(out.Posted, ",") != strings.Join(tc.wantPosts, ",") {
				t.Errorf("posted = %v, want %v", out.Posted, tc.wantPosts)
			}
			if api.reads != tc.wantReads {
				t.Errorf("reads = %d, want %d", api.reads, tc.wantReads)
			}
			for _, h := range api.postHead {
				if h != tc.eventHead {
					t.Errorf("status went on %s, not the event head %s", h, tc.eventHead)
				}
			}
		})
	}
}

func TestHandleEventIsIdempotentAndFollowsTitleEdits(t *testing.T) {
	api := &recordingAPI{subject: Subject{HeadSHA: headA, Title: "✨ add thing", State: "open"}}
	g := newGate(api, ModeShadow)
	g.HandleEvent(context.Background(), "o/r", 7, headA)
	out := g.HandleEvent(context.Background(), "o/r", 7, headA)
	if out.Skipped != SkipUnchanged || len(api.posts) != 1 {
		t.Fatalf("second identical event: %+v posts=%d", out, len(api.posts))
	}
	api.subject.Title = "✨ add thing #12"
	g.HandleEvent(context.Background(), "o/r", 7, headA)
	if len(api.posts) != 2 || !strings.Contains(api.posts[1].Description, "PR Issue: failure") {
		t.Fatalf("an edited title must repost the new verdict: %+v", api.posts)
	}
}

func TestPostRefusalIsRecordedOnceAndNotRetried(t *testing.T) {
	api := &recordingAPI{subject: Subject{HeadSHA: headA, Title: "✨ x", State: "open"}, postErr: errors.Join(ErrForbidden, errors.New("403"))}
	g := newGate(api, ModeShadow)
	out := g.HandleEvent(context.Background(), "o/r", 7, headA)
	if !errors.Is(out.Err, ErrForbidden) || len(api.posts) != 1 {
		t.Fatalf("outcome %+v posts=%d, want one refused write", out, len(api.posts))
	}
	s := g.Snapshot()["o/r"]
	if s.Forbidden != 1 || s.LastPost != ResultForbidden || s.Posted != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestCompareWorkflowRecordsAgreementAndDisagreement(t *testing.T) {
	api := &recordingAPI{subject: Subject{HeadSHA: headA, Title: "📝 proposal", State: "open"}}
	g := newGate(api, ModeShadow)
	if _, ok := g.CompareWorkflow("o/r", headA, ContextPRType, "failure"); ok {
		t.Fatal("a comparison without a hive verdict must report none")
	}
	g.HandleEvent(context.Background(), "o/r", 7, headA)
	tests := []struct {
		name, workflow string
		agree          bool
	}{
		{ContextPRType, "failure", true},
		{ContextPRType, "success", false},
		{ContextPRIssue, "success", true},
		{ContextVerify, "success", true},
		{ContextVerify, "failure", false},
	}
	for _, tc := range tests {
		c, ok := g.CompareWorkflow("O/R", headA, tc.name, tc.workflow)
		if !ok || c.Agree != tc.agree {
			t.Errorf("%s workflow=%s: %+v ok=%v, want agree=%v", tc.name, tc.workflow, c, ok, tc.agree)
		}
	}
	if _, ok := g.CompareWorkflow("o/r", headA, "DCO", "success"); ok {
		t.Error("a non-gate check name must never be compared")
	}
	s := g.Snapshot()["o/r"]
	if s.Agreements != 3 || s.Disagreements != 2 || s.LastComparison == "" {
		t.Errorf("stats = %+v", s)
	}
}

func TestTryAcquireBoundsInflight(t *testing.T) {
	g := newGate(&recordingAPI{}, ModeShadow)
	for i := 0; i < maxInflight; i++ {
		if !g.TryAcquire() {
			t.Fatalf("slot %d refused", i)
		}
	}
	if g.TryAcquire() {
		t.Fatal("slot beyond the bound granted")
	}
	g.Release()
	if !g.TryAcquire() {
		t.Fatal("a released slot must be reusable")
	}
}

func TestEnforcePlanPostsRealNames(t *testing.T) {
	got := map[string]string{}
	for _, st := range Plan(ModeEnforce, "🐛 fix #3") {
		got[st.Context] = st.State
	}
	want := map[string]string{ContextVerify: StateSuccess, ContextPRType: StateSuccess, ContextPRIssue: StateFailure}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("enforce plan %s = %q, want %q", k, got[k], v)
		}
	}
	if len(Plan(ModeOff, "✨ x")) != 0 {
		t.Error("off must plan nothing")
	}
}

func TestPostedMemoryIsBounded(t *testing.T) {
	api := &recordingAPI{subject: Subject{HeadSHA: headA, Title: "✨ add thing", State: "open"}}
	g := newGate(api, ModeShadow)
	// Posting many distinct heads never grows the memory past the bound.
	for i := 0; i < maxSentEntries+50; i++ {
		head := fmt.Sprintf("%040x", i)
		api.subject.HeadSHA = head
		g.HandleEvent(context.Background(), "o/r", 7, head)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sent) > maxSentEntries {
		t.Fatalf("posted memory holds %d entries, want at most %d", len(g.sent), maxSentEntries)
	}
}

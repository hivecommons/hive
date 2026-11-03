package github

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/writeback"
)

// github.notifications.attention_pilot off (the default, and nothing wired):
// every issue note goes through the call site's own write, exactly as
// before — one plain comment per note, no status card.
func TestIssueStatusCardPilotOffUnchanged(t *testing.T) {
	ctx := context.Background()
	for name, wire := range map[string]func(*Client){
		"unwired": func(*Client) {},
		"off":     func(c *Client) { c.SetAttentionPilot(func() bool { return false }) },
	} {
		t.Run(name, func(t *testing.T) {
			s := &statusCommentServer{}
			c := testClient(t, s.start(t).URL)
			wire(c)
			legacyCalls := 0
			legacy := func(body string) func(context.Context) error {
				return func(ctx context.Context) error {
					legacyCalls++
					return c.CreateIssueComment(ctx, "o/r", 5, body)
				}
			}
			if _, err := c.PostIssueNote(ctx, "o/r", 5, prstatus.KeyIssueClose, "closed", legacy("closed")); err != nil {
				t.Fatal(err)
			}
			if _, err := c.PostIssueAttention(ctx, "o/r", 5, "confirm-9", "please confirm", legacy("please confirm")); err != nil {
				t.Fatal(err)
			}
			if legacyCalls != 2 || s.creates != 2 || s.edits != 0 {
				t.Fatalf("legacy=%d creates=%d edits=%d, want both notes posted by their own write", legacyCalls, s.creates, s.edits)
			}
			for _, cm := range s.comments {
				body, _ := cm["body"].(string)
				if prstatus.IsStatusComment(body) || strings.Contains(body, "hive:attention") {
					t.Fatalf("pilot off wrote a card or attention marker: %q", body)
				}
			}
		})
	}
}

// With the pilot on, several runtime writers on one issue — partial work,
// dependency progress, a stage regression, close narration — leave exactly
// one Hive status comment, created once; the rest is edits. None of them
// runs its legacy write.
func TestIssueStatusCardPilotFoldsRoutineNotes(t *testing.T) {
	ctx := context.Background()
	s := &statusCommentServer{}
	c := testClient(t, s.start(t).URL)
	c.SetAppBotLogin("hive-app[bot]")
	c.SetAttentionPilot(func() bool { return true })
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	noLegacy := func(context.Context) error {
		t.Error("legacy write ran with the attention pilot on")
		return nil
	}
	notes := map[string]string{
		prstatus.KeyIssuePartial(9):  mergedPRPartialStageMarker + " pr=9 -->\nPR #9 has merged.",
		prstatus.KeyStageRegressed:   "stage regressed",
		prstatus.KeyIssuePRLink:      "covered by #9",
		prstatus.KeyIssueClose:       CloseOnMergeMarker + "\nFixed by #9. Closing. Reply /reopen if the problem persists.",
		prstatus.KeyIssueReopened:    closeOnMergeReopenedMarker(9) + "\nReopened by a person after #9 merged.",
		prstatus.KeyIssuePartial(10): mergedPRPartialStageMarker + " pr=10 -->\nPR #10 has merged.",
	}
	// The blocked sweep's unblocked note folds too, with quiet mode off.
	// It runs first: it stays silent on an issue whose card already says
	// the merge closes it.
	c.postUnblockedNote(ctx, "o/r", 5, blockedUnblockedMarker+"\nUnblocked: o/r#12 is no longer open.", logger)
	var wg sync.WaitGroup
	for key, body := range notes {
		wg.Add(1)
		go func(key, body string) {
			defer wg.Done()
			if _, err := c.PostIssueNote(ctx, "o/r", 5, key, body, noLegacy); err != nil {
				t.Errorf("%s: %v", key, err)
			}
		}(key, body)
	}
	wg.Wait()

	if s.creates != 1 || len(s.comments) != 1 {
		t.Fatalf("creates=%d comments=%d, want one status comment", s.creates, len(s.comments))
	}
	body, _ := s.comments[0]["body"].(string)
	if !prstatus.IsStatusComment(body) {
		t.Fatalf("the one comment is not the status card: %q", body)
	}
	for key, want := range notes {
		if got, ok := prstatus.Get(body, key); !ok || got != strings.TrimSpace(want) {
			t.Errorf("section %s = %q, %v; want %q", key, got, ok, want)
		}
	}
	if _, ok := prstatus.Get(body, prstatus.KeyUnblocked); !ok {
		t.Error("unblocked note not folded into the card")
	}
	// Legacy marker readers still find their markers inside the card.
	if !strings.Contains(body, CloseOnMergeMarker) || !strings.Contains(body, closeOnMergeReopenedMarker(9)) {
		t.Errorf("legacy markers missing from card: %q", body)
	}

	// Routine progress after the card exists creates nothing.
	if _, err := c.PostIssueNote(ctx, "o/r", 5, prstatus.KeyStageRegressed, "stage regressed again", noLegacy); err != nil {
		t.Fatal(err)
	}
	if s.creates != 1 {
		t.Fatalf("creates=%d after routine progress, want still 1", s.creates)
	}
}

// A newly required action that arrives after the card exists is announced
// by exactly one explicit comment — an edit alone notifies nobody — and is
// recorded on the card. A repeat of the same cause posts nothing more.
func TestIssueStatusCardAttentionAfterCardExists(t *testing.T) {
	ctx := context.Background()
	s := &statusCommentServer{}
	c := testClient(t, s.start(t).URL)
	c.SetAppBotLogin("hive-app[bot]")
	c.SetAttentionPilot(func() bool { return true })
	noLegacy := func(context.Context) error {
		t.Error("legacy write ran with the attention pilot on")
		return nil
	}
	if _, err := c.PostIssueNote(ctx, "o/r", 5, prstatus.KeyIssuePRLink, "covered by #9", noLegacy); err != nil {
		t.Fatal(err)
	}
	if s.creates != 1 {
		t.Fatalf("creates=%d, want the card", s.creates)
	}
	ask := "Fix merged in #9; awaiting confirmation. Reply /fixed when verified."
	for i := 0; i < 3; i++ {
		posted, err := c.PostIssueAttention(ctx, "o/r", 5, "confirm-9", ask, noLegacy)
		if err != nil {
			t.Fatal(err)
		}
		if posted != (i == 0) {
			t.Fatalf("call %d: posted=%v", i, posted)
		}
	}
	if s.creates != 2 || len(s.comments) != 2 {
		t.Fatalf("creates=%d comments=%d, want the card plus exactly one attention comment", s.creates, len(s.comments))
	}
	card, _ := s.comments[0]["body"].(string)
	event, _ := s.comments[1]["body"].(string)
	if prstatus.IsStatusComment(event) || !strings.Contains(event, prstatus.IssueAttentionMarker("confirm-9")) || !strings.Contains(event, ask) {
		t.Fatalf("attention comment = %q", event)
	}
	if got, ok := prstatus.Get(card, prstatus.KeyIssueAttentionPrefix+"confirm-9"); !ok || !strings.Contains(got, ask) {
		t.Fatalf("attention section = %q, %v", got, ok)
	}

	// A different newly required action is its own single event.
	if posted, err := c.PostIssueAttention(ctx, "o/r", 5, "confirm-10", "confirm #10", noLegacy); err != nil || !posted {
		t.Fatalf("posted=%v err=%v", posted, err)
	}
	if s.creates != 3 {
		t.Fatalf("creates=%d, want one more for the new cause", s.creates)
	}
}

// With no card yet, creating the card is the one notifying write for the
// action; no separate attention comment follows, now or on a repeat.
func TestIssueStatusCardAttentionCreatesCardOnce(t *testing.T) {
	ctx := context.Background()
	s := &statusCommentServer{}
	c := testClient(t, s.start(t).URL)
	c.SetAppBotLogin("hive-app[bot]")
	c.SetAttentionPilot(func() bool { return true })
	noLegacy := func(context.Context) error {
		t.Error("legacy write ran with the attention pilot on")
		return nil
	}
	for i := 0; i < 2; i++ {
		if _, err := c.PostIssueAttention(ctx, "o/r", 5, "confirm-9", "confirm #9", noLegacy); err != nil {
			t.Fatal(err)
		}
	}
	if s.creates != 1 || len(s.comments) != 1 {
		t.Fatalf("creates=%d comments=%d, want only the card", s.creates, len(s.comments))
	}
	body, _ := s.comments[0]["body"].(string)
	if !prstatus.IsStatusComment(body) || !strings.Contains(body, prstatus.IssueAttentionMarker("confirm-9")) {
		t.Fatalf("card = %q", body)
	}
}

// When the write-back gate refuses the explicit attention comment, the
// refusal surfaces as writeback.ErrSuppressed before anything is written:
// no comment, no marker on the card, so the caller does not report the
// action as announced. Once the policy admits the write, the next pass
// posts the one announcement.
func TestIssueStatusCardAttentionGateRefusalRetries(t *testing.T) {
	ctx := context.Background()
	s := &statusCommentServer{}
	c := testClient(t, s.start(t).URL)
	c.SetAppBotLogin("hive-app[bot]")
	c.SetAttentionPilot(func() bool { return true })
	noLegacy := func(context.Context) error {
		t.Error("legacy write ran with the attention pilot on")
		return nil
	}
	if _, err := c.PostIssueNote(ctx, "o/r", 5, prstatus.KeyIssuePRLink, "covered by #9", noLegacy); err != nil {
		t.Fatal(err)
	}
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
		return writeback.RepoPauseInfo{By: "op", Reason: "paused"}, true
	}}, ""))
	posted, err := c.PostIssueAttention(ctx, "o/r", 5, "confirm-9", "confirm #9", noLegacy)
	if posted || !errors.Is(err, writeback.ErrSuppressed) {
		t.Fatalf("refused attention: posted=%v err=%v, want ErrSuppressed", posted, err)
	}
	if s.creates != 1 || s.edits != 0 {
		t.Fatalf("creates=%d edits=%d after refusal, want nothing written", s.creates, s.edits)
	}
	card, _ := s.comments[0]["body"].(string)
	if strings.Contains(card, prstatus.IssueAttentionMarker("confirm-9")) {
		t.Fatalf("refused announcement left its marker on the card: %q", card)
	}

	c.SetWritebackGate(nil)
	posted, err = c.PostIssueAttention(ctx, "o/r", 5, "confirm-9", "confirm #9", noLegacy)
	if err != nil || !posted {
		t.Fatalf("admitted attention: posted=%v err=%v", posted, err)
	}
	if s.creates != 2 {
		t.Fatalf("creates=%d, want the card plus one attention comment", s.creates)
	}
	if posted, err := c.PostIssueAttention(ctx, "o/r", 5, "confirm-9", "confirm #9", noLegacy); err != nil || posted || s.creates != 2 {
		t.Fatalf("repeat: posted=%v err=%v creates=%d", posted, err, s.creates)
	}
}

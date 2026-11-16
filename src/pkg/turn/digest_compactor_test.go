package turn

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func digestConversation(turns int) []Message {
	msgs := []Message{{Role: RoleSystem, Content: "primer"}, {Role: RoleUser, Content: "Triage issue #7"}}
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Name: "repo.search_code", Arguments: fmt.Sprintf(`{"query": "term%d"}`, i)}}},
			Message{Role: RoleTool, ToolCallID: id, Name: "repo.search_code", Content: "hits"},
		)
	}
	return msgs
}

func TestDigestCompactorPinsTaskAndDigestsDroppedCalls(t *testing.T) {
	c := &DigestCompactor{PinHead: 2, MaxRecent: 6}
	out, err := c.Compact(context.Background(), digestConversation(10), 0)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Content != "primer" || out[1].Content != "Triage issue #7" {
		t.Fatalf("head not pinned: %+v", out[:2])
	}
	d := out[2]
	if d.Metadata[DigestMetadataKey] == "" || d.Role != RoleUser {
		t.Fatalf("digest message missing: %+v", d)
	}
	if !strings.Contains(d.Content, `- repo.search_code {"query": "term0"}`) || strings.Contains(d.Content, "term9") {
		t.Fatalf("digest content = %q", d.Content)
	}
	if out[3].Role == RoleTool {
		t.Fatal("window starts with an orphaned tool result")
	}
	if len(out) != 3+6 {
		t.Fatalf("len = %d", len(out))
	}
}

func TestDigestCompactorSkipsOrphanedToolResult(t *testing.T) {
	// MaxRecent 5 over (assistant, tool) pairs lands the window on a tool
	// message; it must be skipped, not kept without its call.
	out, _ := (&DigestCompactor{PinHead: 2, MaxRecent: 5}).Compact(context.Background(), digestConversation(10), 0)
	if out[3].Role != RoleAssistant {
		t.Fatalf("first recent message = %s, want assistant", out[3].Role)
	}
}

func TestDigestCompactorCarriesDigestForward(t *testing.T) {
	c := &DigestCompactor{PinHead: 2, MaxRecent: 4}
	msgs := digestConversation(6)
	once, _ := c.Compact(context.Background(), msgs, 0)
	// Simulate more turns after the first compaction, then compact again.
	more := append(once, digestConversation(12)[2+12:]...)
	twice, _ := c.Compact(context.Background(), more, 0)
	d := twice[2].Content
	if !strings.Contains(d, "term0") || strings.Count(d, "- repo.search_code") < 4 {
		t.Fatalf("earlier digest not carried forward: %q", d)
	}
	if strings.Count(twice[2].Content, digestHeader) != 1 {
		t.Fatal("digest nested inside digest")
	}
}

func TestDigestCompactorShortConversationUnchanged(t *testing.T) {
	msgs := digestConversation(2)
	out, _ := (&DigestCompactor{}).Compact(context.Background(), msgs, 0)
	if len(out) != len(msgs) {
		t.Fatal("short conversation must be returned unchanged")
	}
}

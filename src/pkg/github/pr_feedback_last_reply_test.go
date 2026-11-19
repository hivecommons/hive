package github

import (
	"testing"
	"time"
)

// The stranded-feedback check (#19320) reads when the hive last acted on a
// PR: its own comments, reviews and thread replies count; the sweep's own
// needs-human and review-rule notes, and people's comments, do not.
func TestLastHiveReplyAt(t *testing.T) {
	c := humanCommentTestClient(t, "http://127.0.0.1:1")
	at := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	var raw rawPRConversation
	if got := LastHiveReplyAt(raw, c.isHiveAuthor); !got.IsZero() {
		t.Fatalf("empty PR = %v, want zero", got)
	}
	raw.Comments.Nodes = []rawPRComment{
		rawComment(1, "hive[bot]", "Bot", "NONE", "opened", at),
		rawComment(2, "nate", "User", "MEMBER", "please revise", at.Add(3*time.Hour)),
		rawComment(3, "hive[bot]", "Bot", "NONE", FeedbackNeedsHumanMarker+"\nask a person", at.Add(4*time.Hour)),
		rawComment(4, "hive[bot]", "Bot", "NONE", "<!-- hive:review-rule-ack id=R-1 -->\nRecorded", at.Add(5*time.Hour)),
	}
	if got := LastHiveReplyAt(raw, c.isHiveAuthor); !got.Equal(at) {
		t.Fatalf("got %v, want the hive's own comment only", got)
	}
	raw.ReviewThreads.Nodes = []rawPRCommentThread{{ID: "T", Comments: struct {
		Nodes []rawPRComment `json:"nodes"`
	}{Nodes: []rawPRComment{rawComment(5, "hive[bot]", "Bot", "NONE", "fixed in abc", at.Add(2*time.Hour))}}}}
	if got := LastHiveReplyAt(raw, c.isHiveAuthor); !got.Equal(at.Add(2 * time.Hour)) {
		t.Fatalf("got %v, want the thread reply", got)
	}
}

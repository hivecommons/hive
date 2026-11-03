package github

import (
	"context"
	"strings"
	"testing"
)

// The first write creates the marker comment; later writes edit it in
// place, hand the renderer the previous body, and skip the edit when the
// renderer returns the same or an empty body.
func TestEditIssueMarkerCommentCreatesThenEdits(t *testing.T) {
	s := &statusCommentServer{}
	srv := s.start(t)
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	ctx := context.Background()
	const marker = "<!-- hive:test-ledger -->"

	res, err := c.EditIssueMarkerComment(ctx, "o/r", 7, marker, func(prev string) string {
		if prev != "" {
			t.Fatalf("first render got previous %q", prev)
		}
		return marker + "\nv1"
	})
	if err != nil || !res.Created || res.Found || res.Edited {
		t.Fatalf("first write = %+v, %v", res, err)
	}
	res, err = c.EditIssueMarkerComment(ctx, "o/r", 7, marker, func(prev string) string {
		if !strings.HasSuffix(prev, "v1") {
			t.Fatalf("second render got previous %q", prev)
		}
		return marker + "\nv2"
	})
	if err != nil || !res.Found || !res.Edited || res.Created {
		t.Fatalf("second write = %+v, %v", res, err)
	}
	res, err = c.EditIssueMarkerComment(ctx, "o/r", 7, marker, func(prev string) string { return prev })
	if err != nil || !res.Found || res.Edited {
		t.Fatalf("unchanged body must not edit: %+v, %v", res, err)
	}
	res, err = c.EditIssueMarkerComment(ctx, "o/r", 7, marker, func(string) string { return "  " })
	if err != nil || !res.Found || res.Edited {
		t.Fatalf("empty body must not edit: %+v, %v", res, err)
	}
	if s.creates != 1 || s.edits != 1 || len(s.comments) != 1 {
		t.Fatalf("creates=%d edits=%d comments=%d, want 1/1/1", s.creates, s.edits, len(s.comments))
	}
	if got := s.comments[0]["body"]; got != marker+"\nv2" {
		t.Fatalf("comment body = %q", got)
	}
}

// A marker comment by someone other than the hive is never edited: the
// hive posts its own instead. An empty first render posts nothing.
func TestEditIssueMarkerCommentIgnoresForeignAuthorAndEmptyBody(t *testing.T) {
	s := &statusCommentServer{}
	s.comments = append(s.comments, map[string]any{"id": 9, "body": "<!-- hive:test-ledger -->\nforged", "user": map[string]any{"login": "mallory", "type": "User"}})
	srv := s.start(t)
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	ctx := context.Background()
	const marker = "<!-- hive:test-ledger -->"

	res, err := c.EditIssueMarkerComment(ctx, "o/r", 7, marker, func(string) string { return "" })
	if err != nil || res.Found || res.Created {
		t.Fatalf("empty first render = %+v, %v", res, err)
	}
	res, err = c.EditIssueMarkerComment(ctx, "o/r", 7, marker, func(prev string) string {
		if prev != "" {
			t.Fatalf("forged comment must not be offered as previous: %q", prev)
		}
		return marker + "\nours"
	})
	if err != nil || !res.Created {
		t.Fatalf("write past forged comment = %+v, %v", res, err)
	}
	if s.creates != 1 || s.edits != 0 {
		t.Fatalf("creates=%d edits=%d, want 1/0", s.creates, s.edits)
	}

	if _, err := (*Client)(nil).EditIssueMarkerComment(ctx, "o/r", 7, marker, func(string) string { return "x" }); err == nil {
		t.Fatal("nil client must error")
	}
	if _, err := c.EditIssueMarkerComment(ctx, "o/r", 7, marker, nil); err == nil {
		t.Fatal("nil renderer must error")
	}
}

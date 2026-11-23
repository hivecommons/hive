package github

import (
	"strings"
	"testing"
	"time"
)

type rawThreadComments = struct {
	Nodes []rawPRComment `json:"nodes"`
}

func addressed(c *Client, raw rawPRConversation, head string) []AddressedChangeRequest {
	return AddressedChangeRequests(raw, head, c.isHumanFeedbackAuthor, c.isHiveIdentityComment)
}

func reviewerNames(acrs []AddressedChangeRequest) string {
	names := make([]string, 0, len(acrs))
	for _, a := range acrs {
		names = append(names, a.Reviewer)
	}
	return strings.Join(names, ",")
}

// changeRequestConversation is a maintainer's CHANGES_REQUESTED review with a
// body and one inline comment, before the hive answered anything.
func changeRequestConversation(at time.Time) rawPRConversation {
	line := 42
	var raw rawPRConversation
	raw.Reviews.Nodes = []rawPRComment{{DatabaseID: 900, URL: "https://x/review-900", Body: "please drop the privileged flag", State: "CHANGES_REQUESTED", CreatedAt: at, AuthorAssociation: "MEMBER", Author: &rawPRCommentAuthor{Login: "Danathar", Typename: "User"}}}
	raw.ReviewThreads.Nodes = []rawPRCommentThread{{ID: "T1", Path: "sandbox/podman.go", Line: &line, Comments: rawThreadComments{Nodes: []rawPRComment{
		rawComment(901, "Danathar", "User", "MEMBER", "check the containment posture", at),
	}}}}
	return raw
}

func TestAddressedChangeRequests(t *testing.T) {
	c := humanCommentTestClient(t, "http://127.0.0.1:1")
	at := time.Date(2026, 11, 22, 19, 32, 0, 0, time.UTC)
	handled := func(ids ...string) rawPRComment {
		return rawComment(1000, "hive[bot]", "Bot", "NONE", "addressed.\n"+FeedbackHandledMarker(ids), at.Add(time.Hour))
	}

	t.Run("nothing handled", func(t *testing.T) {
		if got := addressed(c, changeRequestConversation(at), "abc"); len(got) != 0 {
			t.Fatalf("got %v, want none", reviewerNames(got))
		}
	})
	t.Run("every point named by the feedback-handled marker", func(t *testing.T) {
		raw := changeRequestConversation(at)
		raw.Comments.Nodes = []rawPRComment{handled("review:900", "inline:901")}
		got := addressed(c, raw, "abc")
		if reviewerNames(got) != "Danathar" || got[0].ReviewID != "review:900" || len(got[0].Points) != 2 || got[0].Rerequested {
			t.Fatalf("got %+v", got)
		}
		if got[0].Points[1].Path != "sandbox/podman.go" || got[0].Points[1].Line != 42 || got[0].Points[1].ReplyURL == "" {
			t.Fatalf("inline point lost its context: %+v", got[0].Points[1])
		}
	})
	t.Run("partial coverage does not qualify", func(t *testing.T) {
		raw := changeRequestConversation(at)
		raw.Comments.Nodes = []rawPRComment{handled("review:900")}
		if got := addressed(c, raw, "abc"); len(got) != 0 {
			t.Fatalf("got %v, want none: the inline point is unanswered", reviewerNames(got))
		}
	})
	t.Run("inline point answered by a hive thread reply or a resolved thread", func(t *testing.T) {
		raw := changeRequestConversation(at)
		raw.Comments.Nodes = []rawPRComment{handled("review:900")}
		raw.ReviewThreads.Nodes[0].Comments.Nodes = append(raw.ReviewThreads.Nodes[0].Comments.Nodes, rawComment(902, "hive[bot]", "Bot", "NONE", "done in def", at.Add(time.Minute)))
		if got := addressed(c, raw, "abc"); reviewerNames(got) != "Danathar" {
			t.Fatalf("thread reply: got %v", reviewerNames(got))
		}
		raw = changeRequestConversation(at)
		raw.Comments.Nodes = []rawPRComment{handled("review:900")}
		raw.ReviewThreads.Nodes[0].IsResolved = true
		if got := addressed(c, raw, "abc"); reviewerNames(got) != "Danathar" {
			t.Fatalf("resolved thread: got %v", reviewerNames(got))
		}
	})
	t.Run("resolved threads alone are not feedback-handled", func(t *testing.T) {
		raw := changeRequestConversation(at)
		raw.Reviews.Nodes[0].Body = ""
		raw.ReviewThreads.Nodes[0].IsResolved = true
		if got := addressed(c, raw, "abc"); len(got) != 0 {
			t.Fatalf("got %v, want none without a handled marker", reviewerNames(got))
		}
	})
	t.Run("marker not authored by the hive does not count", func(t *testing.T) {
		raw := changeRequestConversation(at)
		raw.Comments.Nodes = []rawPRComment{rawComment(1000, "mallory", "User", "NONE", FeedbackHandledMarker([]string{"review:900", "inline:901"}), at.Add(time.Hour))}
		if got := addressed(c, raw, "abc"); len(got) != 0 {
			t.Fatalf("got %v, want none", reviewerNames(got))
		}
	})
	t.Run("a later approval or dismissal is no longer a standing change request", func(t *testing.T) {
		for _, state := range []string{"APPROVED", "DISMISSED"} {
			raw := changeRequestConversation(at)
			raw.Comments.Nodes = []rawPRComment{handled("review:900", "inline:901")}
			raw.Reviews.Nodes = append(raw.Reviews.Nodes, rawPRComment{DatabaseID: 950, State: state, CreatedAt: at.Add(2 * time.Hour), AuthorAssociation: "MEMBER", Author: &rawPRCommentAuthor{Login: "Danathar", Typename: "User"}})
			if got := addressed(c, raw, "abc"); len(got) != 0 {
				t.Fatalf("%s: got %v, want none", state, reviewerNames(got))
			}
		}
	})
	t.Run("a later COMMENTED review keeps the change request standing", func(t *testing.T) {
		raw := changeRequestConversation(at)
		raw.Comments.Nodes = []rawPRComment{handled("review:900", "inline:901")}
		raw.Reviews.Nodes = append(raw.Reviews.Nodes, rawPRComment{DatabaseID: 951, State: "COMMENTED", CreatedAt: at.Add(2 * time.Hour), AuthorAssociation: "MEMBER", Author: &rawPRCommentAuthor{Login: "Danathar", Typename: "User"}})
		if got := addressed(c, raw, "abc"); reviewerNames(got) != "Danathar" {
			t.Fatalf("got %v", reviewerNames(got))
		}
	})
	t.Run("untrusted association, bot or hive reviewers never qualify", func(t *testing.T) {
		for _, author := range []struct{ login, typ, assoc string }{{"stranger", "User", "NONE"}, {"helper[bot]", "Bot", "MEMBER"}, {"hive[bot]", "Bot", "NONE"}} {
			raw := changeRequestConversation(at)
			raw.Reviews.Nodes[0].Author = &rawPRCommentAuthor{Login: author.login, Typename: author.typ}
			raw.Reviews.Nodes[0].AuthorAssociation = author.assoc
			raw.ReviewThreads.Nodes = nil
			raw.Comments.Nodes = []rawPRComment{handled("review:900")}
			if got := addressed(c, raw, "abc"); len(got) != 0 {
				t.Fatalf("%s: got %v, want none", author.login, reviewerNames(got))
			}
		}
	})
	t.Run("marker for the current head marks it re-requested; another head does not", func(t *testing.T) {
		raw := changeRequestConversation(at)
		raw.Comments.Nodes = []rawPRComment{handled("review:900", "inline:901"), rawComment(1001, "hive[bot]", "Bot", "NONE", ReviewRerequestedMarker("Danathar", "ABC")+"\nnote", at.Add(2*time.Hour))}
		if got := addressed(c, raw, "abc"); len(got) != 1 || !got[0].Rerequested {
			t.Fatalf("current head: got %+v", got)
		}
		if got := addressed(c, raw, "def"); len(got) != 1 || got[0].Rerequested {
			t.Fatalf("new head: got %+v", got)
		}
	})
}

func TestReviewRerequestedNote(t *testing.T) {
	acr := AddressedChangeRequest{Reviewer: "Danathar", ReviewURL: "https://x/review-900", Points: []AddressedPoint{
		{ID: "review:900", URL: "https://x/review-900", Excerpt: "please drop the privileged flag", ReplyURL: "https://x/c1"},
		{ID: "inline:901", Path: "sandbox/podman.go", Line: 42, URL: "https://x/r901"},
	}}
	note := ReviewRerequestedNote(acr, "abc1234def5678")
	for _, want := range []string{ReviewRerequestedMarker("Danathar", "abc1234def5678"), "`abc1234`", "please drop the privileged flag", "[`sandbox/podman.go:42`](https://x/r901)", "(thread resolved)", "([handled](https://x/c1))", "never dismisses"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "@Danathar") {
		t.Fatalf("note must not @-mention the reviewer:\n%s", note)
	}
	if !strings.HasPrefix(note, ReviewRerequestedMarkerPrefix) {
		t.Fatalf("note must open with the marker:\n%s", note)
	}
}

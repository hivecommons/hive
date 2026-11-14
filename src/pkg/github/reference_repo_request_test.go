package github

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func derivedReadOnlyFixture() []config.ReferenceRepo {
	return []config.ReferenceRepo{
		{Repo: "acme/docs", DerivedFrom: "o/r#7", CredentialState: config.ReferenceCredentialReadOnly, CredentialReason: "the hive's GitHub App is not installed on acme"},
		{Repo: "acme/site", DerivedFrom: "o/r#7", CredentialState: config.ReferenceCredentialReadOnly, CredentialReason: "the hive's GitHub App is not installed on acme"},
	}
}

func TestDerivedReadOnlyMarkerIsASetKey(t *testing.T) {
	a := DerivedReadOnlyMarker([]string{"Acme/Site", "acme/docs", " acme/docs "})
	b := DerivedReadOnlyMarker([]string{"acme/docs", "acme/site"})
	if a != b {
		t.Fatalf("marker must not depend on order, case or duplicates:\n%s\n%s", a, b)
	}
	if !strings.HasPrefix(a, DerivedReadOnlyMarkerPrefix) || !strings.Contains(a, "repos=acme/docs,acme/site") || !strings.Contains(a, "state=read-only") || !strings.HasSuffix(a, "-->") {
		t.Fatalf("marker = %q", a)
	}
	if DerivedReadOnlyMarker([]string{"acme/docs"}) == a {
		t.Fatal("a different repo set must yield a different marker")
	}
}

func TestDerivedReadOnlyCommentBody(t *testing.T) {
	body := DerivedReadOnlyComment(derivedReadOnlyFixture())
	for _, want := range []string{DerivedReadOnlyMarkerPrefix, "`acme/docs`", "`acme/site`", "not installed on acme", "will not open issues or pull requests", AttributionTrailerPrefix} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "@") {
		t.Fatalf("comment must not mention anyone:\n%s", body)
	}
	if DerivedReadOnlyComment(nil) != "" {
		t.Fatal("no read-only entries must render nothing")
	}
}

// hivecommons/hive#18158: exactly one notice per (request issue, repo set),
// however many reloads happen; a wider set posts a second one.
func TestCommentDerivedReadOnlyPostsOnce(t *testing.T) {
	s := &statusCommentServer{}
	srv := s.start(t)
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive-app[bot]")
	ctx := context.Background()
	readOnly := derivedReadOnlyFixture()

	posted, err := c.CommentDerivedReadOnly(ctx, "o/r", 7, readOnly)
	if err != nil || !posted {
		t.Fatalf("first notice posted=%v err=%v", posted, err)
	}
	for i := 0; i < 3; i++ {
		posted, err = c.CommentDerivedReadOnly(ctx, "o/r", 7, readOnly)
		if err != nil || posted {
			t.Fatalf("reload %d reposted: posted=%v err=%v", i, posted, err)
		}
	}
	if s.creates != 1 || s.edits != 0 || len(s.comments) != 1 {
		t.Fatalf("creates=%d edits=%d comments=%d, want 1/0/1", s.creates, s.edits, len(s.comments))
	}
	body, _ := s.comments[0]["body"].(string)
	if !strings.Contains(body, DerivedReadOnlyMarker([]string{"acme/docs", "acme/site"})) {
		t.Fatalf("comment body lacks the marker:\n%s", body)
	}

	// Same set in another order is the same notice.
	posted, err = c.CommentDerivedReadOnly(ctx, "o/r", 7, []config.ReferenceRepo{readOnly[1], readOnly[0]})
	if err != nil || posted || s.creates != 1 {
		t.Fatalf("reordered set reposted: posted=%v err=%v creates=%d", posted, err, s.creates)
	}
	// A later grant that adds a repo is a new repo set and a new notice.
	wider := append(readOnly, config.ReferenceRepo{Repo: "acme/legacy", DerivedFrom: "o/r#7", CredentialState: config.ReferenceCredentialReadOnly})
	posted, err = c.CommentDerivedReadOnly(ctx, "o/r", 7, wider)
	if err != nil || !posted || s.creates != 2 {
		t.Fatalf("wider set: posted=%v err=%v creates=%d", posted, err, s.creates)
	}
	// Nothing read-only, nothing posted.
	posted, err = c.CommentDerivedReadOnly(ctx, "o/r", 7, nil)
	if err != nil || posted || s.creates != 2 {
		t.Fatalf("empty set: posted=%v err=%v creates=%d", posted, err, s.creates)
	}
	if _, err := (*Client)(nil).CommentDerivedReadOnly(ctx, "o/r", 7, readOnly); err == nil {
		t.Fatal("nil client must refuse")
	}
}

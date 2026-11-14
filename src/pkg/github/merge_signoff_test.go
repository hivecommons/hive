package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// Signed-off-by through the hive's squash merges (hivecommons/hive#18203).

func TestSquashSignoffMessage(t *testing.T) {
	// The #18079 shape: one signed branch commit whose GitHub default squash
	// body came out as Co-authored-by lines only (f33758f5a).
	scanner := "[scanner] fix: de-flake two tests\n\n" +
		"Signed-off-by: scanner <280983584+hivecommons-hive[bot]@users.noreply.github.com>\n\n" +
		"Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
	t.Run("empty message is rebuilt from the branch and keeps the sign-off", func(t *testing.T) {
		got, err := squashSignoffMessage([]*gh.RepositoryCommit{commitWithMessage(scanner)}, "")
		if err != nil {
			t.Fatalf("squashSignoffMessage: %v", err)
		}
		want := "Signed-off-by: scanner <280983584+hivecommons-hive[bot]@users.noreply.github.com>\n\n" +
			"Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
		if got != want {
			t.Fatalf("message =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("a message without the sign-off gains it as a trailer", func(t *testing.T) {
		got, err := squashSignoffMessage([]*gh.RepositoryCommit{commitWithMessage(scanner)}, "Co-authored-by: dana <4+dana@users.noreply.github.com>")
		if err != nil {
			t.Fatalf("squashSignoffMessage: %v", err)
		}
		want := "Co-authored-by: dana <4+dana@users.noreply.github.com>\n\n" +
			"Signed-off-by: scanner <280983584+hivecommons-hive[bot]@users.noreply.github.com>"
		if got != want {
			t.Fatalf("message =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("every distinct branch sign-off is kept once", func(t *testing.T) {
		commits := []*gh.RepositoryCommit{
			commitWithMessage("One\n\nSigned-off-by: A <a@example.com>"),
			commitWithMessage("Two\n\nsigned-off-by: a <A@example.com>\nSigned-off-by: B <b@example.com>"),
		}
		got, err := squashSignoffMessage(commits, "Squash body")
		if err != nil {
			t.Fatalf("squashSignoffMessage: %v", err)
		}
		want := "Squash body\n\nSigned-off-by: A <a@example.com>\nSigned-off-by: B <b@example.com>"
		if got != want {
			t.Fatalf("message =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("merge-from-base commits do not count", func(t *testing.T) {
		mergeFromBase := &gh.RepositoryCommit{
			Commit:  &gh.Commit{Message: gh.Ptr("Merge v5\n\nSigned-off-by: M <m@example.com>")},
			Parents: []*gh.Commit{{SHA: gh.Ptr("p1")}, {SHA: gh.Ptr("p2")}},
		}
		_, err := squashSignoffMessage([]*gh.RepositoryCommit{commitWithMessage("Fix"), mergeFromBase}, "")
		if !errors.Is(err, ErrSquashUnsigned) {
			t.Fatalf("err = %v, want ErrSquashUnsigned", err)
		}
	})
	t.Run("no sign-off on the branch refuses the squash", func(t *testing.T) {
		_, err := squashSignoffMessage([]*gh.RepositoryCommit{commitWithMessage("Fix\n\nCo-authored-by: x <x@example.com>")}, "Co-authored-by: x <x@example.com>")
		if !errors.Is(err, ErrSquashUnsigned) {
			t.Fatalf("err = %v, want ErrSquashUnsigned", err)
		}
	})
}

func TestMergeCreditorSquashSignoff(t *testing.T) {
	ctx := context.Background()
	off := func(string) bool { return false }
	t.Run("applies with issue-author credit off", func(t *testing.T) {
		api := &mergeCreditAPI{commits: []string{"Fix\n\nSigned-off-by: K <k@example.com>"}}
		m, _ := newMergeCreditorForTest(t, api, off)
		got, err := m.SquashSignoff(ctx, "acme", "widget", 7, "squash", "")
		if err != nil || got != "Signed-off-by: K <k@example.com>" {
			t.Fatalf("SquashSignoff = %q, %v", got, err)
		}
	})
	t.Run("a signed message costs no request", func(t *testing.T) {
		api := &mergeCreditAPI{}
		m, _ := newMergeCreditorForTest(t, api, off)
		msg := "Body\n\nSigned-off-by: K <k@example.com>"
		if got, err := m.SquashSignoff(ctx, "acme", "widget", 7, "squash", msg); err != nil || got != msg || api.listGets.Load() != 0 {
			t.Fatalf("SquashSignoff = %q, %v after %d commit lists", got, err, api.listGets.Load())
		}
	})
	t.Run("merge and rebase are untouched", func(t *testing.T) {
		api := &mergeCreditAPI{}
		m, _ := newMergeCreditorForTest(t, api, off)
		for _, method := range []string{"merge", "rebase"} {
			if got, err := m.SquashSignoff(ctx, "acme", "widget", 7, method, ""); err != nil || got != "" {
				t.Fatalf("%s: SquashSignoff = %q, %v", method, got, err)
			}
		}
		if api.listGets.Load() != 0 {
			t.Fatalf("merge/rebase read the commits %d times", api.listGets.Load())
		}
	})
	t.Run("unsigned branch is refused", func(t *testing.T) {
		api := &mergeCreditAPI{commits: []string{"Fix"}}
		m, _ := newMergeCreditorForTest(t, api, off)
		if _, err := m.SquashSignoff(ctx, "acme", "widget", 7, "squash", ""); !errors.Is(err, ErrSquashUnsigned) {
			t.Fatalf("err = %v, want ErrSquashUnsigned", err)
		}
	})
	t.Run("nil creditor passes the message through", func(t *testing.T) {
		var m *MergeCreditor
		if got, err := m.SquashSignoff(ctx, "acme", "widget", 7, "squash", "x"); err != nil || got != "x" {
			t.Fatalf("nil: %q, %v", got, err)
		}
	})
}

// MergePRVia refuses to squash a branch with no sign-off and sends no merge.
func TestMergePRViaRefusesUnsignedSquash(t *testing.T) {
	green := greenFixture()
	merges := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/42":
			json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "state": "open", "title": "Fix the widget", "body": "",
				"user": map[string]any{"login": "karibhou"},
				"head": map[string]any{"sha": "abc123", "ref": "fix/widget"},
				"base": map[string]any{"ref": "v5"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/42/commits":
			json.NewEncoder(w).Encode([]map[string]any{{"sha": "c0", "commit": map[string]any{"message": "Fix the widget"}, "parents": []map[string]any{{"sha": "p"}}}})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/o/r/pulls/42/merge":
			merges++
			_, _ = io.WriteString(w, `{"sha":"deadbeef","merged":true}`)
		default:
			if green.serveCI(w, r) {
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	_, err := c.MergePRVia(context.Background(), "o/r", 42, "squash", "", PRAuditPathForge)
	if !errors.Is(err, ErrSquashUnsigned) || !strings.Contains(err.Error(), "o/r#42") {
		t.Fatalf("MergePRVia err = %v, want ErrSquashUnsigned for o/r#42", err)
	}
	if merges != 0 {
		t.Fatalf("merges = %d, want 0", merges)
	}
}

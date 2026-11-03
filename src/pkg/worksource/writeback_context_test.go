package worksource_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestSolicitedWorksourceContextDoesNotAuthorizeAnotherItem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"number":43,"user":{"login":"human"},"labels":[]}`))
	}))
	defer srv.Close()
	client := github.NewClientForTest(srv.URL, "org", []string{"repo"}, testLogger())
	client.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 1 }}, ""))
	source := worksource.NewGitHubIssuesSource(client)
	ref := worksource.Ref{Repo: "org/repo", Number: 42}
	ctx := worksource.WithSolicitedWriteback(context.Background(), ref, "verified-owner")
	other := worksource.Ref{Repo: "org/repo", Number: 43}
	if err := source.(worksource.Commenter).AddComment(ctx, other, "unrelated design"); !errors.Is(err, writeback.ErrSuppressed) {
		t.Fatalf("solicitation leaked to another item: %v", err)
	}
	client.SetRepoPausedFunc(func(string) bool { return true })
	if err := source.(worksource.LabelMutator).AddLabel(ctx, ref, "hive-design"); !errors.Is(err, writeback.ErrSuppressed) {
		t.Fatalf("legacy operator pause bypassed: %v", err)
	}
}

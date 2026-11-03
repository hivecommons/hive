package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestAlreadyDoneCloseRespectsWritebackBeforeAnyMutation(t *testing.T) {
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		_, _ = w.Write([]byte(`{"number":42,"user":{"login":"visitor"},"labels":[]}`))
	}))
	defer srv.Close()
	client := ghpkg.NewClientForTest(srv.URL, "org", []string{"repo"}, nil)
	gate := writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 1 }}, "")
	client.SetWritebackGate(gate)
	hub, server := covK2Hub(t)
	server.deps.GHClient = client
	err := hub.markAlreadyDoneIssue(context.Background(), "org/repo", 42, ghpkg.IssueClaim{PRNumber: 43, MergedPR: true}, "contributor", true)
	if !errors.Is(err, writeback.ErrSuppressed) || writes != 0 {
		t.Fatalf("err=%v writes=%d", err, writes)
	}
	if suppressed := gate.Recent(); len(suppressed) != 1 || suppressed[0].Kind != writeback.KindIssueClose || suppressed[0].Number != 42 {
		t.Fatalf("suppressed=%+v", suppressed)
	}
}

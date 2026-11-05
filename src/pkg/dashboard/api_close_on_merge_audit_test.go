package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestCloseOnMergeWrongClosesEndpoint(t *testing.T) {
	prev := closeOnMergeWrongClosesFunc
	t.Cleanup(func() { closeOnMergeWrongClosesFunc = prev })
	var gotSince time.Time
	closeOnMergeWrongClosesFunc = func(_ context.Context, _ *ghpkg.Client, since time.Time) ([]ghpkg.CloseOnMergeWrongClose, error) {
		gotSince = since
		return []ghpkg.CloseOnMergeWrongClose{{Repo: "projectbluefin/utah-packages", Issue: 490, PR: 860, Reason: "pr_leaves_issue_open"}}, nil
	}
	s, deps := apiServer(t)
	deps.GHClient = &ghpkg.Client{}
	rec := doGet(s, "/api/close-on-merge/wrong-closes?days=7")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Days     int                            `json:"days"`
		Complete bool                           `json:"complete"`
		Items    []ghpkg.CloseOnMergeWrongClose `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Days != 7 || !resp.Complete || len(resp.Items) != 1 || resp.Items[0].Issue != 490 {
		t.Fatalf("resp = %+v", resp)
	}
	if d := time.Since(gotSince); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("since = %v, want 7 days ago", gotSince)
	}
	for _, bad := range []string{"0", "91", "x"} {
		if rec := doGet(s, "/api/close-on-merge/wrong-closes?days="+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("days=%s: code = %d, want 400", bad, rec.Code)
		}
	}
}

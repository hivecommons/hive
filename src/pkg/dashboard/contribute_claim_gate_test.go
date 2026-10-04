package dashboard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestRelayClaimMarkerRefusesNeedsHuman(t *testing.T) {
	hub, s := covK2Hub(t)
	enableIssueClaims(s)
	recorder := &claimRecorder{}
	hub.claimCommenter = recorder
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"number":42,"labels":[{"name":"needs-human"}]}`)
	}))
	defer api.Close()
	s.deps.GHClient = github.NewClientForTest(api.URL, "o", []string{"r"}, hub.logger)
	claim, posted := hub.recordAgentClaim(context.Background(), issueClaimConn("contributor"), "task-1", "o/r", 42, time.Now())
	if posted || claim.Identity != "" || len(recorder.all()) != 0 {
		t.Fatalf("needs-human received legacy claim marker: %+v, posted=%v", claim, posted)
	}
}

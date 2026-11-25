package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// needs-human on the forge (and not yet on the cached snapshot) refuses the
// assignment before any claim is recorded or posted: the live gate runs on
// the wired GitHub client ahead of the lease, the claim and the mint.
func TestRelayClaimMarkerRefusesNeedsHuman(t *testing.T) {
	hub, s := covK2Hub(t)
	enableIssueClaims(s)
	recorder := &claimRecorder{}
	hub.claimCommenter = recorder
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"number":42,"state":"open","labels":[{"name":"needs-human"}]}`)
	}))
	defer api.Close()
	s.deps.GHClient = github.NewClientForTest(api.URL, "myorg", []string{"repo1"}, hub.logger)
	setStatusIssues(s, intgIssue(42, "needs a person", "someone", nil))
	c := issueClaimConn("contributor")
	hub.mu.Lock()
	hub.connections["conn-needs-human"] = c
	hub.mu.Unlock()

	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_unavailable" {
		t.Fatalf("selectTask = %+v, want task_unavailable for a needs-human issue", msg)
	}
	if len(recorder.all()) != 0 {
		t.Fatalf("needs-human received a claim marker: %+v", recorder.all())
	}
}

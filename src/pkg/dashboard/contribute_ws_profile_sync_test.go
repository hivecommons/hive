package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func registerAndDialContributor(t *testing.T, s *Server, ts *httptest.Server, user string, n int) (map[string]string, []*websocket.Conn) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(`{"github_username":"`+user+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("register: status %d body %s", w.Code, w.Body.String())
	}
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	var conns []*websocket.Conn
	for i := 0; i < n; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		readMsg(t, conn)
		if err := conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"}); err != nil {
			t.Fatalf("auth write: %v", err)
		}
		if m := readMsg(t, conn); m.Type != "auth_ok" {
			t.Fatalf("auth: %+v", m)
		}
		conns = append(conns, conn)
	}
	live := 0
	s.contributeHub.mu.RLock()
	for _, c := range s.contributeHub.connections {
		c.mu.Lock()
		if c.profile != nil && c.profile.ContributorID == reg["contributor_id"] {
			live++
		}
		c.mu.Unlock()
	}
	s.contributeHub.mu.RUnlock()
	if live != n {
		t.Fatalf("expected %d connected contributors, got %d", n, live)
	}
	return reg, conns
}

func completeVerifiedPRTask(t *testing.T, s *Server, conn *websocket.Conn, issue int) {
	t.Helper()
	setStatusIssues(s, intgIssue(issue, "profile sync regression", "someone", nil))
	if err := conn.WriteJSON(WSMessage{Type: "ready", Seq: issue}); err != nil {
		t.Fatalf("ready write: %v", err)
	}
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}
	prURL := "https://github.com/myorg/repo1/pull/" + itoa(900+issue)
	if err := conn.WriteJSON(WSMessage{Type: "task_complete", TaskID: assign.TaskID, Result: "pr_created", PRURL: prURL}); err != nil {
		t.Fatalf("complete write: %v", err)
	}
}

func waitForContributorProfile(t *testing.T, contributorID, reason string, match func(*ContributorProfile) bool) *ContributorProfile {
	t.Helper()
	deadline := time.NewTimer(contributorPollTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(contributorPollInterval)
	defer tick.Stop()
	for {
		p := findContributor(contributorID)
		if p != nil && match(p) {
			return p
		}
		select {
		case <-deadline.C:
			if p == nil {
				t.Fatalf("timed out waiting for %s; contributor %s not found", reason, contributorID)
			}
			t.Fatalf("timed out waiting for %s; last profile: completed=%d withPR=%d failed=%d tier=%s version=%d",
				reason, p.TasksCompleted, p.TasksWithPR, p.TasksFailed, p.TrustTier, p.Version)
		case <-tick.C:
		}
	}
}

func liveTrustTiers(s *Server, contributorID string) []string {
	var tiers []string
	s.contributeHub.mu.RLock()
	for _, c := range s.contributeHub.connections {
		c.mu.Lock()
		if c.profile != nil && c.profile.ContributorID == contributorID {
			tiers = append(tiers, c.profile.TrustTier)
		}
		c.mu.Unlock()
	}
	s.contributeHub.mu.RUnlock()
	return tiers
}

func TestContributorTrustChangeRefreshesLiveProfileAndKeepsCounting(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = verifyDepsFor(t, "myorg", "repo1", "live-sync-a")
	reg, conns := registerAndDialContributor(t, s, ts, "live-sync-a", 1)
	defer conns[0].Close()
	id := reg["contributor_id"]

	completeVerifiedPRTask(t, s, conns[0], 1)
	waitForContributorProfile(t, id, "initial verified completion", func(p *ContributorProfile) bool {
		return p.TasksCompleted == 1 && p.TasksWithPR == 1
	})

	req := httptest.NewRequest(http.MethodPut, "/api/contributors/"+id+"/trust", strings.NewReader(`{"tier":"trusted"}`))
	req.SetPathValue("id", id)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	s.handleContributorTrust(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("trust change: status %d body %s", w.Code, w.Body.String())
	}
	if tiers := liveTrustTiers(s, id); !reflect.DeepEqual(tiers, []string{"trusted"}) {
		t.Fatalf("expected live connection tier to refresh to trusted, got %v", tiers)
	}

	completeVerifiedPRTask(t, s, conns[0], 2)
	p := waitForContributorProfile(t, id, "post-trust-change completion", func(p *ContributorProfile) bool {
		return p.TrustTier == "trusted" && p.TasksCompleted == 2 && p.TasksWithPR == 2
	})
	if p.Version < 2 {
		t.Fatalf("expected version to advance, got %d", p.Version)
	}
}

func TestContributorProfileConflictsRetryAcrossConnections(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = verifyDepsFor(t, "myorg", "repo1", "live-sync-b")
	reg, conns := registerAndDialContributor(t, s, ts, "live-sync-b", 3)
	for _, conn := range conns {
		defer conn.Close()
	}
	id := reg["contributor_id"]

	for i, conn := range conns {
		completeVerifiedPRTask(t, s, conn, 10+i)
		want := i + 1
		waitForContributorProfile(t, id, "completion from each connected relay", func(p *ContributorProfile) bool {
			return p.TasksCompleted == want && p.TasksWithPR == want
		})
	}
}

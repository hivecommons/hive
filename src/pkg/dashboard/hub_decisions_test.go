package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
)

// #7330: the hub's own decisions about a contributor were slog lines and nothing
// else, so a contributor whose reports were being silently dropped looked
// exactly like one whose relay never sent them. These tests hold three things:
// the ring stays bounded, the gate refuses rather than degrades, and the
// motivating case — a fenced task_failed — is actually readable from the
// endpoint afterwards.

func TestHubDecisionRing_BoundsPerUser(t *testing.T) {
	var l hubDecisionLog
	for i := 0; i < hubDecisionsPerUser+50; i++ {
		l.record(HubDecision{Username: "alice", Event: decisionRefused, Detail: "n=" + strconv.Itoa(i)})
	}
	got, since := l.forUser("alice", 0)
	if len(got) != hubDecisionsPerUser {
		t.Fatalf("ring held %d, want it capped at %d", len(got), hubDecisionsPerUser)
	}
	if since.IsZero() {
		t.Error("since must be stamped on first write")
	}
	// Newest first, and the oldest entries are the ones evicted.
	if got[0].Detail != "n="+strconv.Itoa(hubDecisionsPerUser+49) {
		t.Errorf("newest entry = %q, want the last one recorded", got[0].Detail)
	}
	for _, d := range got {
		if d.Detail == "n=0" {
			t.Error("the oldest entry survived eviction")
		}
	}
}

func TestHubDecisionRing_BoundsDistinctUsers(t *testing.T) {
	var l hubDecisionLog
	for i := 0; i < hubDecisionUsers+25; i++ {
		l.record(HubDecision{Username: "user" + strconv.Itoa(i), Event: decisionAbandoned})
	}
	l.mu.Lock()
	n := len(l.byUser)
	l.mu.Unlock()
	if n != hubDecisionUsers {
		t.Fatalf("tracked %d logins, want %d — a stream of new logins must not grow the ring forever", n, hubDecisionUsers)
	}
	// The earliest logins are the ones dropped; the most recent survive.
	if got, _ := l.forUser("user0", 0); len(got) != 0 {
		t.Error("the least recently active login should have been evicted")
	}
	if got, _ := l.forUser("user"+strconv.Itoa(hubDecisionUsers+24), 0); len(got) != 1 {
		t.Error("the most recent login must still be present")
	}
}

func TestHubDecisionRing_IgnoresUnusableEntries(t *testing.T) {
	var l hubDecisionLog
	l.record(HubDecision{Username: "", Event: decisionRefused})      // no login to key on
	l.record(HubDecision{Username: "alice", Event: ""})              // no event
	l.record(HubDecision{Username: "   ", Event: decisionAbandoned}) // blank login
	l.mu.Lock()
	n := len(l.byUser)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("recorded %d entries, want 0 — an unkeyable decision must be dropped, not stored", n)
	}
	// A nil ring is a no-op rather than a panic: the call sites are on the
	// connection teardown path.
	var nilLog *hubDecisionLog
	nilLog.record(HubDecision{Username: "alice", Event: decisionRefused})
	if got, _ := nilLog.forUser("alice", 0); len(got) != 0 {
		t.Error("a nil ring must read empty")
	}
}

func TestHubDecisionViewer_OwnerAndReadWriteOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		role      string
		authToken string
		want      bool
	}{
		{"owner", config.RoleOwner, "tok", true},
		{"read-write", config.RoleReadWrite, "tok", true},
		{"read-only", "read", "tok", false},
		{"unknown role", "something", "tok", false},
		// Empty role mirrors requestRoleAllowsOwner: anonymous behind a
		// boundary gets nothing; on a genuinely open spoke the whole dashboard
		// is already anonymous.
		{"empty role behind a token boundary", "", "tok", false},
		{"empty role on an open spoke", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{authToken: tc.authToken}
			r := httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?username=alice", nil)
			if tc.role != "" {
				r.Header.Set("X-Hive-Role", tc.role)
			}
			if got := s.hubDecisionViewer(r); got != tc.want {
				t.Errorf("hubDecisionViewer = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHandleContributeDecisions(t *testing.T) {
	s := &Server{authToken: "tok", contributeHub: &ContributeWSHub{}}
	s.contributeHub.recordDecision("alice", decisionStaleGenRejected, "ct-1", "myorg/repo1", 7,
		"task_failed fenced: client_gen 0 no longer matches the assignment")

	get := func(role, query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?"+query, nil)
		if role != "" {
			r.Header.Set("X-Hive-Role", role)
		}
		rr := httptest.NewRecorder()
		s.handleContributeDecisions(rr, r)
		return rr
	}

	// The gate REFUSES rather than returning a stripped list — there is nothing
	// useful left once the protocol fields are gone.
	if rr := get("read", "username=alice"); rr.Code != http.StatusForbidden {
		t.Errorf("read-only got %d, want 403", rr.Code)
	}
	if rr := get("", "username=alice"); rr.Code != http.StatusForbidden {
		t.Errorf("anonymous behind a boundary got %d, want 403", rr.Code)
	}
	// A 403 must not leak the decision text it refused to serve.
	if rr := get("read", "username=alice"); strings.Contains(rr.Body.String(), "client_gen") {
		t.Errorf("the refusal body leaked decision detail: %s", rr.Body.String())
	}

	if rr := get(config.RoleOwner, ""); rr.Code != http.StatusBadRequest {
		t.Errorf("missing username got %d, want 400", rr.Code)
	}

	rr := get(config.RoleOwner, "username=alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("owner got %d, want 200", rr.Code)
	}
	var body struct {
		Username     string        `json:"username"`
		Returned     int           `json:"returned"`
		Since        string        `json:"since"`
		InMemoryOnly bool          `json:"in_memory_only"`
		Decisions    []HubDecision `json:"decisions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if body.Username != "alice" || body.Returned != 1 || len(body.Decisions) != 1 {
		t.Fatalf("body = %+v", body)
	}
	if !body.InMemoryOnly || body.Since == "" {
		t.Error("the response must say the ring is in-memory and since when, or an empty list is unreadable")
	}
	d := body.Decisions[0]
	if d.Event != decisionStaleGenRejected || d.TaskID != "ct-1" || d.Repo != "myorg/repo1" || d.Number != 7 {
		t.Errorf("decision = %+v", d)
	}
	if !strings.Contains(d.Detail, "client_gen 0") {
		t.Errorf("detail lost the structured field: %q", d.Detail)
	}
	if d.TS == "" {
		t.Error("record must stamp ts")
	}

	// A contributor with no decisions is a 200 with an empty list — "nothing
	// since boot" is an answer.
	if rr := get(config.RoleOwner, "username=nobody"); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `"returned":0`) {
		t.Errorf("unknown user = %d %s", rr.Code, rr.Body.String())
	}
}

func TestHandleContributeDecisions_NoHubIsEmptyNotAnError(t *testing.T) {
	s := &Server{authToken: "tok"} // contributeHub nil
	r := httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?username=alice", nil)
	r.Header.Set("X-Hive-Role", config.RoleOwner)
	rr := httptest.NewRecorder()
	s.handleContributeDecisions(rr, r)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"returned":0`) {
		t.Fatalf("no hub = %d %s", rr.Code, rr.Body.String())
	}
}

// decisionUsername must return the GitHub LOGIN, not identityOf's
// ContributorID[#session] — the ring is keyed by login and the endpoint is
// queried by login, so keying on the wrong one produces an endpoint that
// always answers empty for a contributor the operator can see being refused.
func TestDecisionUsername_IsTheLoginNotTheContributorID(t *testing.T) {
	c := &ContributorConnection{
		profile: &ContributorProfile{GitHubUsername: "alice", ContributorID: "cid-123"},
		session: "s1",
	}
	if got := decisionUsername(c); got != "alice" {
		t.Errorf("decisionUsername = %q, want the GitHub login", got)
	}
	if id := identityOf(c); id == decisionUsername(c) {
		t.Fatalf("this test is vacuous: identityOf and decisionUsername both gave %q", id)
	}
	if got := decisionUsername(nil); got != "" {
		t.Errorf("nil connection = %q, want empty", got)
	}
	if got := decisionUsername(&ContributorConnection{}); got != "" {
		t.Errorf("profile-less connection = %q, want empty", got)
	}
}

// The motivating case of #7330, end to end through the REAL WebSocket handler:
// a task_failed that the #2568 generation fence drops. Before this change the
// hub logged it and an operator reading the dashboard saw a task that was
// picked up and handed back with no failure anywhere — indistinguishable from a
// relay that never reported. Now the rejection is readable by username.
func TestHubDecisions_StaleGenTaskFailedIsReadableAfterwards(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"fenced-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register response: %v", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "litellm", Model: "Qwen/Qwen3.6-35B-A3B"})
	readMsg(t, conn) // auth_ok

	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name: "repo1", Full: "myorg/repo1",
			ActionableIssues: []any{noWorkIssue(94, "fenced issue", time.Now().Add(-24*time.Hour))},
		}},
	}
	s.statusMu.Unlock()

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}

	// Establish a non-zero generation on this connection, so the fence is armed
	// (generationAccepted only drops a 0 once a non-zero has been seen).
	conn.WriteJSON(WSMessage{Type: "task_progress", TaskID: assign.TaskID, TaskGen: assign.TaskGen})
	// Then report the failure the way the relay's up-front rejection paths do:
	// with no task_gen at all. This is the shape that silently vanished.
	conn.WriteJSON(WSMessage{
		Type: "task_failed", TaskID: assign.TaskID,
		Reason: "already has active task", FailureKind: "environment",
	})

	// The handler records asynchronously relative to this goroutine. Polled via
	// testutil.EventuallyValue rather than a sleep — internal/testutil's sleep
	// ratchet counts a new time.Sleep in a test as a regression.
	decisions := testutil.EventuallyValue(t, 5*time.Second, func() ([]HubDecision, bool) {
		got, _ := s.contributeHub.decisions.forUser("fenced-user", 0)
		return got, len(got) > 0
	}, "no hub decision recorded for fenced-user")
	var fenced *HubDecision
	for i := range decisions {
		if decisions[i].Event == decisionStaleGenRejected {
			fenced = &decisions[i]
			break
		}
	}
	if fenced == nil {
		t.Fatalf("the fenced task_failed left no decision: %+v", decisions)
	}
	if !strings.Contains(fenced.Detail, "task_failed") {
		t.Errorf("detail does not say which message was fenced: %q", fenced.Detail)
	}
	if fenced.TaskID != assign.TaskID {
		t.Errorf("decision task = %q, want %q", fenced.TaskID, assign.TaskID)
	}

	// And it is reachable the way an operator would reach it: the gated endpoint.
	r := httptest.NewRequest(http.MethodGet, "/api/contribute/decisions?username=fenced-user", nil)
	r.Header.Set("X-Hive-Role", config.RoleOwner)
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("endpoint status %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), decisionStaleGenRejected) {
		t.Errorf("endpoint did not serve the fence: %s", rr.Body.String())
	}
}

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// Contributor repository scope (hivecommons/hive#12805, RFC #12570 §4).

const (
	scopeIDReview    = "github/github.com/myorg/review"
	scopeIDTestsuite = "github/github.com/myorg/testsuite"
)

func ackScope(c *ContributorConnection, version int, mode string, ids ...string) {
	scope, _, reason := parseRepoScope(WSMessage{Type: "repo_scope", ScopeVersion: version, Mode: mode, Repositories: ids})
	if reason != "" {
		panic("test scope must be valid: " + reason)
	}
	c.mu.Lock()
	c.repoScope = scope
	c.repoScopeInvalid = false
	c.mu.Unlock()
}

func TestServerCapabilitiesAdvertisesRepoScope(t *testing.T) {
	if !testContainsString(serverCapabilities(), capRepoScope) {
		t.Fatalf("serverCapabilities() = %v, missing %q", serverCapabilities(), capRepoScope)
	}
	if contributorProtocolVersion != "1.4" {
		t.Fatalf("repo scope is negotiated on a capability token; protocol version must stay 1.4, got %s", contributorProtocolVersion)
	}
}

func TestParseRepoScope(t *testing.T) {
	cases := []struct {
		name   string
		msg    WSMessage
		reason string
		ids    []string
	}{
		{"all", WSMessage{ScopeVersion: 1, Mode: "all"}, "", nil},
		{"selected normalizes case and dedupes", WSMessage{ScopeVersion: 2, Mode: "selected",
			Repositories: []string{"GitHub/GitHub.com/MyOrg/Review", "github/github.com/myorg/review", " github/github.com/myorg/testsuite "}},
			"", []string{scopeIDReview, scopeIDTestsuite}},
		{"selected empty is valid", WSMessage{ScopeVersion: 1, Mode: "selected"}, "", []string{}},
		{"zero version", WSMessage{Mode: "all"}, repoScopeReasonInvalidVersion, nil},
		{"negative version", WSMessage{ScopeVersion: -1, Mode: "all"}, repoScopeReasonInvalidVersion, nil},
		{"unknown mode", WSMessage{ScopeVersion: 1, Mode: "some"}, repoScopeReasonInvalidMode, nil},
		{"missing mode", WSMessage{ScopeVersion: 1}, repoScopeReasonInvalidMode, nil},
		{"bare full name", WSMessage{ScopeVersion: 1, Mode: "selected", Repositories: []string{"myorg/review"}}, repoScopeReasonInvalidID, nil},
		{"empty segment", WSMessage{ScopeVersion: 1, Mode: "selected", Repositories: []string{"github//myorg/review"}}, repoScopeReasonInvalidID, nil},
		{"dot segment", WSMessage{ScopeVersion: 1, Mode: "selected", Repositories: []string{"github/github.com/../review"}}, repoScopeReasonInvalidID, nil},
		{"whitespace", WSMessage{ScopeVersion: 1, Mode: "selected", Repositories: []string{"github/github.com/my org/review"}}, repoScopeReasonInvalidID, nil},
		{"too many", WSMessage{ScopeVersion: 1, Mode: "selected", Repositories: make([]string, repoScopeMaxRepositories+1)}, repoScopeReasonTooMany, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, ids, reason := parseRepoScope(tc.msg)
			if reason != tc.reason {
				t.Fatalf("reason = %q, want %q", reason, tc.reason)
			}
			if reason != "" {
				if scope != nil {
					t.Fatalf("a rejected scope must not be returned: %+v", scope)
				}
				return
			}
			if tc.ids != nil && !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("ids = %v, want %v", ids, tc.ids)
			}
		})
	}
}

// The headline acceptance criterion: while the hub's highest-priority item
// (myorg/review#638, first in scan order) is outside the contributor's scope,
// the contributor is served its selected lower-priority item instead of being
// offered — and having to reject — the out-of-scope one.
func TestRepoScope_SelectedLowerPriorityWorkIsServed(t *testing.T) {
	hub, s := covK2Hub(t)
	twoRepoStatus(s)

	conn := lockScopeConn(hub, "alice", "c-alice")
	ackScope(conn, 1, repoScopeModeSelected, scopeIDTestsuite)
	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" || msg.Repo != "myorg/testsuite" || msg.Number != 866 || msg.Forge != repoScopeDefaultForge || msg.Host == "" {
		t.Fatalf("expected the selected testsuite#866, got %+v", msg)
	}

	// The out-of-scope item was never leased: an unrestricted contributor still
	// gets it.
	other := lockScopeConn(hub, "bob", "c-bob")
	msg = hub.selectTask(other)
	if msg == nil || msg.Type != "task_assign" || msg.Repo != "myorg/review" || msg.Number != 638 {
		t.Fatalf("out-of-scope review#638 must stay available to others, got %+v", msg)
	}
}

func TestRepoScope_OutOfScopeNeverLeasedOrMinted(t *testing.T) {
	hub, s := covK2Hub(t)
	twoRepoStatus(s)
	auth, mints := newRepoRejectingAppAuth(t, "no-such-repo")
	s.deps.GHAppAuth = auth

	conn := lockScopeConn(hub, "carol", "c-carol")
	ackScope(conn, 1, repoScopeModeSelected, "github/github.com/myorg/elsewhere")
	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableNoSelectedWork {
		t.Fatalf("expected task_unavailable/%s, got %+v", taskUnavailableNoSelectedWork, msg)
	}
	conn.mu.Lock()
	task := conn.currentTask
	conn.mu.Unlock()
	if task != nil {
		t.Fatalf("no task may be committed for an out-of-scope item: %+v", task)
	}
	if got := len(hub.leases); got != 0 {
		t.Fatalf("out-of-scope items must never be leased: %d lease(s)", got)
	}
	if got := mints.Load(); got != 0 {
		t.Fatalf("out-of-scope items must never be minted for: %d mint(s)", got)
	}
	if hour, day := hub.rateWindowCounts(identityOf(conn), time.Now()); hour != 0 || day != 0 {
		t.Fatalf("no allowance may be consumed: hour=%d day=%d", hour, day)
	}
}

// Connections that never send repo_scope — and those that send mode "all" —
// are served exactly as before: the head of the queue first.
func TestRepoScope_NoScopeAndModeAllUnchanged(t *testing.T) {
	for _, mode := range []string{"", repoScopeModeAll} {
		t.Run("mode="+mode, func(t *testing.T) {
			hub, s := covK2Hub(t)
			twoRepoStatus(s)
			conn := lockScopeConn(hub, "dora", "c-dora")
			if mode != "" {
				ackScope(conn, 1, mode)
			}
			msg := hub.selectTask(conn)
			if msg == nil || msg.Type != "task_assign" || msg.Repo != "myorg/review" || msg.Number != 638 {
				t.Fatalf("expected the head of the queue review#638, got %+v", msg)
			}
		})
	}
}

func TestRepoScope_InvalidScopeFailsClosed(t *testing.T) {
	hub, s := covK2Hub(t)
	twoRepoStatus(s)
	server, client := wsPipe(t)
	conn := lockScopeConn(hub, "erin", "c-erin")
	conn.ws = server
	sess := &wsSession{h: hub, conn: server, contributor: conn}

	// A previously acknowledged valid scope is dropped by a malformed one.
	ackScope(conn, 1, repoScopeModeAll)
	sess.handleRepoScope(WSMessage{Type: "repo_scope", ScopeVersion: 2, Mode: "selected", Repositories: []string{"myorg/review"}})
	ack := readWire(t, client)
	if ack["type"] != "repo_scope_ack" || ack["accepted"] != false || ack["reason"] != repoScopeReasonInvalidID {
		t.Fatalf("expected a rejecting repo_scope_ack, got %v", ack)
	}
	if ack["scope_version"] != float64(2) {
		t.Fatalf("ack must echo scope_version, got %v", ack)
	}

	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableRepoScopeInvalid {
		t.Fatalf("expected task_unavailable/%s, got %+v", taskUnavailableRepoScopeInvalid, msg)
	}
	if got := len(hub.leases); got != 0 {
		t.Fatalf("an invalid scope must lease nothing: %d lease(s)", got)
	}

	// A valid scope reopens the connection.
	sess.handleRepoScope(WSMessage{Type: "repo_scope", ScopeVersion: 3, Mode: "selected", Repositories: []string{scopeIDTestsuite}})
	ack = readWire(t, client)
	if ack["type"] != "repo_scope_ack" || ack["accepted"] != true || ack["scope_version"] != float64(3) {
		t.Fatalf("expected an accepting repo_scope_ack, got %v", ack)
	}
	msg = hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" || msg.Repo != "myorg/testsuite" {
		t.Fatalf("expected testsuite work after a valid scope, got %+v", msg)
	}
}

func TestRepoScope_AckListsUnknownRepositories(t *testing.T) {
	hub, s := covK2Hub(t)
	twoRepoStatus(s)
	server, client := wsPipe(t)
	conn := lockScopeConn(hub, "fay", "c-fay")
	conn.ws = server
	sess := &wsSession{h: hub, conn: server, contributor: conn}

	sess.handleRepoScope(WSMessage{Type: "repo_scope", ScopeVersion: 1, Mode: "selected",
		Repositories: []string{scopeIDReview, "github/github.com/myorg/gone"}})
	ack := readWire(t, client)
	if ack["accepted"] != true {
		t.Fatalf("a well-formed scope is accepted even with unknown ids, got %v", ack)
	}
	unknown, _ := ack["unknown"].([]any)
	if len(unknown) != 1 || unknown[0] != "github/github.com/myorg/gone" {
		t.Fatalf("unknown = %v, want [github/github.com/myorg/gone]", ack["unknown"])
	}
}

// ready carries scope_version: the hub selects only under the version it
// acknowledged, closing the relay's reload race.
func TestRepoScope_ReadyVersionMismatchWaits(t *testing.T) {
	hub, s := covK2Hub(t)
	twoRepoStatus(s)
	server, client := wsPipe(t)
	conn := lockScopeConn(hub, "gus", "c-gus")
	conn.ws = server
	sess := &wsSession{h: hub, conn: server, contributor: conn}
	ackScope(conn, 1, repoScopeModeSelected, scopeIDTestsuite)

	if stop := sess.handleReady(WSMessage{Type: "ready", ScopeVersion: 2}); stop {
		t.Fatal("a version mismatch must not stop the session")
	}
	frame := readWire(t, client)
	if frame["type"] != "task_unavailable" || frame["reason"] != taskUnavailableNoSelectedWork {
		t.Fatalf("expected task_unavailable/%s on a version mismatch, got %v", taskUnavailableNoSelectedWork, frame)
	}
	if got := len(hub.leases); got != 0 {
		t.Fatalf("a version mismatch must lease nothing: %d lease(s)", got)
	}

	if stop := sess.handleReady(WSMessage{Type: "ready", ScopeVersion: 1}); stop {
		t.Fatal("a matching ready must not stop the session")
	}
	frame = readWire(t, client)
	if frame["type"] != "task_assign" || frame["repo"] != "myorg/testsuite" {
		t.Fatalf("expected a testsuite task_assign under the acknowledged version, got %v", frame)
	}
}

func scopeDeclineConn(hub *ContributeWSHub, username, id string, at time.Time) *ContributorConnection {
	c := lockScopeConn(hub, username, id)
	c.currentTask = &WSTaskAssign{TaskID: "ct-myorg/review-638-1", Kind: "issue", Repo: "myorg/review", Number: 638}
	c.currentTaskGen = 7
	c.taskAssignedAt = at
	c.lastLeaseRenew = at
	c.pendingToken = "ghs_scope_decline"
	hub.recordAssignment(identityOf(c), at)
	return c
}

// A scope decline releases the lease and books nothing: no failure cooldown
// and no consumed hourly/daily allowance.
func TestRepoScope_DeclineRollsBackAllowance(t *testing.T) {
	hub, _ := covK2Hub(t)
	at := time.Now()
	conn := scopeDeclineConn(hub, "hana", "c-hana", at)
	sess := &wsSession{h: hub, contributor: conn}

	sess.handleTaskDeclined(WSMessage{Type: "task_declined", TaskID: "ct-myorg/review-638-1", TaskGen: 7, Reason: taskDeclinedOutOfScope})

	conn.mu.Lock()
	task := conn.currentTask
	conn.mu.Unlock()
	if task != nil {
		t.Fatalf("the declined task must be released: %+v", task)
	}
	if hour, day := hub.rateWindowCounts(identityOf(conn), time.Now()); hour != 0 || day != 0 {
		t.Fatalf("a scope decline must not consume allowance: hour=%d day=%d", hour, day)
	}
	if hub.isTaskInFailureCooldown("myorg/review", 638) {
		t.Fatal("a scope decline must not book a failure cooldown")
	}
	if streak := hub.recentFailureCountKey("myorg/review#638"); streak != 0 {
		t.Fatalf("a scope decline must not count as a failure, got %d", streak)
	}

	// A quota-preflight decline (#6833) keeps its existing accounting.
	other := scopeDeclineConn(hub, "ivan", "c-ivan", at)
	(&wsSession{h: hub, contributor: other}).handleTaskDeclined(WSMessage{Type: "task_declined", TaskID: "ct-myorg/review-638-1", TaskGen: 7, Reason: "quota_preflight"})
	if hour, _ := hub.rateWindowCounts(identityOf(other), time.Now()); hour != 1 {
		t.Fatalf("a non-scope decline keeps the slot it consumed, got hour=%d", hour)
	}
}

func TestContributeRepositories_ListsEligibleRepos(t *testing.T) {
	_, s := covK2Hub(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{
		{Name: "review", Full: "myorg/review"},
		{Name: "Testsuite", Full: "MyOrg/Testsuite"},
		{Name: "secret", Full: "myorg/secret"},
		{Name: "archive-old", Full: "myorg/archive-old"},
	}}
	s.statusMu.Unlock()
	s.deps.Config.Hub.DisabledRepos = []string{"secret", "myorg/archive-*"}

	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/contribute/repositories", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Repositories []contributeRepository `json:"repositories"`
		GeneratedAt  string                 `json:"generated_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []contributeRepository{
		{ID: scopeIDReview, Forge: "github", Host: "github.com", FullName: "myorg/review"},
		{ID: scopeIDTestsuite, Forge: "github", Host: "github.com", FullName: "MyOrg/Testsuite"},
	}
	if !reflect.DeepEqual(body.Repositories, want) {
		t.Fatalf("repositories = %+v, want %+v", body.Repositories, want)
	}
	if _, err := time.Parse(time.RFC3339, body.GeneratedAt); err != nil {
		t.Fatalf("generated_at %q is not RFC3339: %v", body.GeneratedAt, err)
	}

	// The advertised ids are exactly the ids selection matches on.
	hub, s2 := covK2Hub(t)
	twoRepoStatus(s2)
	for _, r := range s2.contributeEligibleRepositories() {
		conn := lockScopeConn(hub, "u-"+r.FullName, "c-"+r.FullName)
		ackScope(conn, 1, repoScopeModeSelected, r.ID)
		if msg := hub.selectTask(conn); msg == nil || msg.Type != "task_assign" || msg.Repo != r.FullName {
			t.Fatalf("scope %s must select %s work, got %+v", r.ID, r.FullName, msg)
		}
	}
}

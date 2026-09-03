package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/config"
)

// Credential-after-acceptance (kubestellar/hive#2537).
//
// The contributor's scoped GitHub credential used to travel INSIDE the
// task_assign message, so an agent received "here is who authored the work" and
// "here is a credential" before any acceptance decision. These tests pin the new
// contract: task_assign carries only the DECIDE metadata (repo/number/title/url/
// labels/prompt) and NO github_token, and the scoped, expiring token is delivered
// (via the token_refresh wire shape) only AFTER the task's acceptance decision —
// automatically in the default auto-accept mode, or on an explicit task_accepted
// in the opt-in explicit mode. The token itself is unchanged: same per-tier mint,
// same wsTokenTTL expiry — only its timing moved.
//
// All state exercised here is per-ContributorConnection (pendingToken /
// credentialDelivered); no package-global is mutated, so the -race coverage job
// (#2510/#2652) sees no shared write.

// wsPipe stands up a real gorilla/websocket server/client pair and returns the
// SERVER-side *websocket.Conn (what the hub writes to) plus the CLIENT conn (what
// the relay would read). Mirrors the pattern in contribute_ws_token_refresh_test.go
// so deliverTaskCredential / sendTokenRefresh write a real frame we can read back.
func wsPipe(t *testing.T) (server *websocket.Conn, client *websocket.Conn) {
	t.Helper()
	connReady := make(chan struct{})
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		server = c
		close(connReady)
		// Keep the handler alive so the conn stays open for the reads below.
		time.Sleep(3 * time.Second)
	}))
	t.Cleanup(srv.Close)

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	<-connReady
	return server, c
}

// readWire reads one frame off the client and decodes it into a bare map so
// assertions are against the ON-THE-WIRE field names (github_token,
// token_expires_at) the relay actually consumes.
func readWire(t *testing.T, client *websocket.Conn) map[string]any {
	t.Helper()
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return wire
}

// TestCredentialAccept_TaskAssignHasNoToken is the core #2537 assertion: the
// task_assign selectTask produces carries the decide-metadata but NO credential.
// The token is still MINTED (held pending on the connection), just not shipped in
// the assignment.
func TestCredentialAccept_TaskAssignHasNoToken(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	s.deps.GHAppAuth = newSucceedingAppAuth(t, "ghs_scoped_for_assign")

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "amy", ContributorID: "c-amy", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected a task_assign, got %+v", msg)
	}

	// The credential must NOT be in the assignment message.
	if msg.GitHubToken != "" {
		t.Fatalf("#2537 violated: task_assign carried a github_token (%q); it must be delivered after acceptance", msg.GitHubToken)
	}
	if msg.TokenExpiresAt != "" {
		t.Fatalf("#2537: task_assign must not carry token_expires_at, got %q", msg.TokenExpiresAt)
	}
	// But the decide-metadata IS present.
	if msg.Repo == "" || msg.Number == 0 || msg.Title == "" || msg.Prompt == "" {
		t.Fatalf("task_assign should carry decide-metadata (repo/number/title/prompt), got %+v", msg)
	}

	// The token was minted and is held pending (undelivered) for post-acceptance.
	conn.mu.Lock()
	pending := conn.pendingToken
	delivered := conn.credentialDelivered
	conn.mu.Unlock()
	if pending == "" {
		t.Fatalf("expected the minted scoped token to be held pending on the connection")
	}
	if delivered {
		t.Fatalf("credential must not be marked delivered before any acceptance")
	}

	// Also assert the raw JSON of the assignment never contains a token — a
	// defense against a future field re-introducing it on the wire.
	blob, _ := json.Marshal(msg)
	if strings.Contains(string(blob), "github_token") || strings.Contains(string(blob), "ghs_") {
		t.Fatalf("task_assign JSON leaked a credential:\n%s", string(blob))
	}
}

// TestCredentialAccept_DeliverAfterAcceptance verifies the post-acceptance
// delivery: deliverTaskCredential ships the SAME scoped token via the
// token_refresh wire shape (github_token + token_expires_at, ~wsTokenTTL out), and
// flips credentialDelivered so the ordering "acceptance recorded, THEN credential
// sent" is observable and idempotent.
func TestCredentialAccept_DeliverAfterAcceptance(t *testing.T) {
	hub := &ContributeWSHub{logger: covBLogger()}
	server, client := wsPipe(t)

	const scoped = "ghs_scoped_delivered_after_accept"
	conn := &ContributorConnection{
		ws:           server,
		profile:      &ContributorProfile{TrustTier: "contributor", GitHubUsername: "ben"},
		currentTask:  &WSTaskAssign{TaskID: "ct-1"},
		pendingToken: scoped,
	}

	before := time.Now()
	if ok := hub.deliverTaskCredential(conn, "auto_accept"); !ok {
		t.Fatalf("deliverTaskCredential should have delivered the pending token")
	}

	wire := readWire(t, client)
	if wire["type"] != "token_refresh" {
		t.Fatalf("credential must be delivered via the token_refresh wire shape, got type=%v", wire["type"])
	}
	if wire["github_token"] != scoped {
		t.Fatalf("delivered token = %v, want the SAME scoped token %q", wire["github_token"], scoped)
	}
	expStr, _ := wire["token_expires_at"].(string)
	exp, err := time.Parse(time.RFC3339, expStr)
	if err != nil {
		t.Fatalf("token_expires_at missing/unparseable: %v", wire["token_expires_at"])
	}
	// Still expiring on the same TTL as before — the token is unchanged, only its
	// timing moved.
	wantExp := before.Add(wsTokenTTL)
	if exp.Before(wantExp.Add(-2*time.Minute)) || exp.After(wantExp.Add(2*time.Minute)) {
		t.Fatalf("delivered token expiry %v not ~wsTokenTTL (%s) out from send", exp, wsTokenTTL)
	}

	// Delivery is recorded and the pending token consumed.
	conn.mu.Lock()
	delivered := conn.credentialDelivered
	pending := conn.pendingToken
	conn.mu.Unlock()
	if !delivered {
		t.Fatalf("credentialDelivered must be set after delivery")
	}
	if pending != "" {
		t.Fatalf("pendingToken must be cleared after delivery, got %q", pending)
	}

	// Idempotent: a second call (e.g. a duplicate task_accepted) delivers nothing.
	if ok := hub.deliverTaskCredential(conn, "explicit_accept"); ok {
		t.Fatalf("deliverTaskCredential must be a no-op once the credential was delivered")
	}
}

// TestCredentialAccept_AutoAcceptDefaultOrdering exercises the DEFAULT
// (auto-accept) mode end to end through the same seams the ready handler uses:
// selectTask holds the credential pending, then — because the hub is NOT in
// explicit mode — the credential is delivered immediately, with NO human step, and
// the ordering is provable (task assigned/pending BEFORE the credential is sent).
func TestCredentialAccept_AutoAcceptDefaultOrdering(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	s.deps.GHAppAuth = newSucceedingAppAuth(t, "ghs_auto_accept_scoped")

	// Default config: explicit-accept unset → auto-accept.
	if hub.requireExplicitAccept() {
		t.Fatalf("default mode must be auto-accept (explicit-accept off)")
	}

	server, client := wsPipe(t)
	conn := &ContributorConnection{
		ws:       server,
		profile:  &ContributorProfile{GitHubUsername: "cara", ContributorID: "c-cara", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" || msg.GitHubToken != "" {
		t.Fatalf("expected a credential-free task_assign, got %+v", msg)
	}

	// Acceptance is automatic — the ready handler calls deliverTaskCredential right
	// after sending task_assign. Model that here: the assignment is already
	// committed to the connection (currentTask + pendingToken set), THEN deliver.
	conn.mu.Lock()
	haveTask := conn.currentTask != nil
	havePending := conn.pendingToken != ""
	deliveredBefore := conn.credentialDelivered
	conn.mu.Unlock()
	if !haveTask || !havePending {
		t.Fatalf("assignment must be recorded (task+pending) before credential delivery")
	}
	if deliveredBefore {
		t.Fatalf("credential must not be delivered before the (auto)acceptance step")
	}

	if ok := hub.deliverTaskCredential(conn, "auto_accept"); !ok {
		t.Fatalf("auto-accept should deliver the credential")
	}
	wire := readWire(t, client)
	if wire["type"] != "token_refresh" || wire["github_token"] != "ghs_auto_accept_scoped" {
		t.Fatalf("auto-accept must deliver the scoped token via token_refresh, got %v", wire)
	}
}

// TestCredentialAccept_ExplicitModeWithholdsUntilAccept covers the opt-in
// explicit-accept mode: the credential is withheld at assignment time and
// delivered only when a matching task_accepted arrives; a task that is never
// accepted never receives a credential, and a task_accepted for a DIFFERENT
// task_id is ignored.
func TestCredentialAccept_ExplicitModeWithholdsUntilAccept(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	s.deps.GHAppAuth = newSucceedingAppAuth(t, "ghs_explicit_scoped")

	// Opt into explicit acceptance.
	explicit := true
	s.deps.Config.Hub.ContributeRequireExplicitAccept = &explicit
	if !hub.requireExplicitAccept() {
		t.Fatalf("explicit-accept mode should be on after setting the toggle")
	}

	server, client := wsPipe(t)
	conn := &ContributorConnection{
		ws:       server,
		profile:  &ContributorProfile{GitHubUsername: "dan", ContributorID: "c-dan", TrustTier: "trusted"},
		lastPong: time.Now(),
	}

	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_assign" || msg.GitHubToken != "" {
		t.Fatalf("expected a credential-free task_assign, got %+v", msg)
	}
	assignedTaskID := msg.TaskID

	// In explicit mode the ready handler does NOT auto-deliver: the credential is
	// still pending and undelivered.
	conn.mu.Lock()
	pending := conn.pendingToken
	delivered := conn.credentialDelivered
	conn.mu.Unlock()
	if pending == "" || delivered {
		t.Fatalf("explicit mode must withhold: pending=%q delivered=%v", pending, delivered)
	}

	// A task_accepted for the WRONG task must NOT release the credential.
	if ok := hub.acceptTaskCredential(conn, "ct-some-other-task"); ok {
		t.Fatalf("a mismatched task_accepted must not deliver the credential")
	}
	conn.mu.Lock()
	stillPending := conn.pendingToken != "" && !conn.credentialDelivered
	conn.mu.Unlock()
	if !stillPending {
		t.Fatalf("credential must remain withheld after a mismatched task_accepted")
	}

	// The MATCHING task_accepted delivers the scoped token.
	if ok := hub.acceptTaskCredential(conn, assignedTaskID); !ok {
		t.Fatalf("matching task_accepted should deliver the credential")
	}
	wire := readWire(t, client)
	if wire["type"] != "token_refresh" || wire["github_token"] != "ghs_explicit_scoped" {
		t.Fatalf("explicit acceptance must deliver the scoped token via token_refresh, got %v", wire)
	}
}

// TestCredentialAccept_NeverAcceptedNeverGetsCredential proves the negative: a
// task assigned in explicit mode that is never accepted (the client declines or
// times out) never has its credential delivered — nothing is ever written to the
// wire, and the pending token stays undelivered.
func TestCredentialAccept_NeverAcceptedNeverGetsCredential(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	s.deps.GHAppAuth = newSucceedingAppAuth(t, "ghs_never_delivered")
	explicit := true
	s.deps.Config.Hub.ContributeRequireExplicitAccept = &explicit

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "eve", ContributorID: "c-eve", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	if msg := hub.selectTask(conn); msg == nil || msg.GitHubToken != "" {
		t.Fatalf("expected a credential-free task_assign, got %+v", msg)
	}

	// No task_accepted ever arrives. The credential stays pending and undelivered.
	conn.mu.Lock()
	pending := conn.pendingToken
	delivered := conn.credentialDelivered
	conn.mu.Unlock()
	if pending == "" {
		t.Fatalf("token should have been minted and held pending")
	}
	if delivered {
		t.Fatalf("a never-accepted task must never have its credential delivered")
	}
}

// TestCredentialAccept_ModeResolver pins the backward-compatible default of the
// operator toggle: nil (older on-disk config) resolves to auto-accept, matching a
// running fleet's prior behavior; an explicit true opts into the wait state.
func TestCredentialAccept_ModeResolver(t *testing.T) {
	var h config.HubConfig
	if h.IsContributeRequireExplicitAccept() {
		t.Fatalf("unset toggle must default to auto-accept (false)")
	}
	f := false
	h.ContributeRequireExplicitAccept = &f
	if h.IsContributeRequireExplicitAccept() {
		t.Fatalf("explicit false must resolve to auto-accept")
	}
	tr := true
	h.ContributeRequireExplicitAccept = &tr
	if !h.IsContributeRequireExplicitAccept() {
		t.Fatalf("explicit true must resolve to explicit-accept")
	}
}

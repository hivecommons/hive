package dashboard

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// contribute_select_lock_scope_test.go — hivecommons/hive#7775.
//
// selectTask used to hold the fleet-wide selectMu across two GitHub round-trips
// (the scoped-token mint and the push-permission lookup). Every other
// contributor's `ready` then queued behind one contributor's GitHub latency, and
// because handleReady runs on the connection's read goroutine, a queued
// contributor stopped reading pongs, was hung up on by the heartbeat loop, and
// had an assignment committed to a dead socket — which the disconnect path then
// "released", booking a cooldown on an issue nobody worked. The lock now guards
// only the in-memory claim; the GitHub calls run unlocked, and a claim whose
// task_assign cannot ship is rolled back.

// newGatedAppAuth is newSucceedingAppAuth with a hold on the FIRST mint: the
// token server closes `entered` when the first installation-token request
// arrives and does not answer it until the test closes `release`. Every later
// request answers at once. This stands in for one contributor's slow GitHub
// round-trip while the rest of the fleet keeps asking for work.
func newGatedAppAuth(t *testing.T, token string) (auth *ghpkg.AppAuth, entered <-chan struct{}, release func()) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	keyFile := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(keyFile, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	enteredCh := make(chan struct{})
	releaseCh := make(chan struct{})
	var once, releaseOnce sync.Once
	release = func() { releaseOnce.Do(func() { close(releaseCh) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(enteredCh)
			select {
			case <-releaseCh:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		exp := time.Now().Add(wsTokenTTL).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, exp)
	}))
	t.Cleanup(srv.Close)
	// Registered AFTER srv.Close so it runs first (cleanups are LIFO): a failing
	// assertion must not leave the held request parked, because srv.Close waits
	// for active handlers and an unreleased gate would hang the whole package.
	t.Cleanup(release)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth, err = ghpkg.NewAppAuthWithCache(1, 2, keyFile,
		filepath.Join(t.TempDir(), "token.cache"), logger, srv.URL)
	if err != nil {
		t.Fatalf("NewAppAuthWithCache: %v", err)
	}
	return auth, enteredCh, release
}

// lockScopeConn builds a contributor connection and registers it on the hub the
// way the handshake does, so selectTask's live-connection scan — the
// double-assignment guard — can see the claim committed to it.
func lockScopeConn(hub *ContributeWSHub, username, id string) *ContributorConnection {
	c := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: username, ContributorID: id, TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections[id] = c
	hub.mu.Unlock()
	return c
}

// TestSelectTask_SlowMintDoesNotBlockOtherContributors is the headline: while
// contributor A's selection is parked inside its GitHub token mint, contributor
// B's selection must complete — and must be handed a DIFFERENT issue, because
// A's claim was committed before the lock was released.
func TestSelectTask_SlowMintDoesNotBlockOtherContributors(t *testing.T) {
	hub, s := covK2Hub(t)
	seedTwoIssues(s, 7001, 7002)
	auth, entered, release := newGatedAppAuth(t, "ghs_7775_gated")
	s.deps.GHAppAuth = auth

	connA := lockScopeConn(hub, "alice", "c-alice")
	connB := lockScopeConn(hub, "bob", "c-bob")

	aDone := make(chan *WSMessage, 1)
	go func() { aDone <- hub.selectTask(connA) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("contributor A's selection never reached the token mint")
	}
	// A is now parked inside mintScopedToken. Before #7775 it held selectMu
	// there, and this call would not return until `release` — which is exactly
	// the fleet-wide stall the issue describes.
	bDone := make(chan *WSMessage, 1)
	go func() { bDone <- hub.selectTask(connB) }()

	var bMsg *WSMessage
	select {
	case bMsg = <-bDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("#7775: contributor B's selection is blocked behind contributor A's GitHub token mint — " +
			"selectMu is still held across the GitHub round-trips")
	}
	if bMsg == nil || bMsg.Type != "task_assign" {
		t.Fatalf("contributor B expected a task_assign while A was mid-mint, got %+v", bMsg)
	}

	// The claim A committed before unlocking must have been visible to B's
	// selection: B cannot have been handed the same issue.
	connA.mu.Lock()
	aClaim := connA.currentTask
	connA.mu.Unlock()
	if aClaim == nil {
		t.Fatalf("contributor A's claim must be committed before its mint runs, so B's scan can see it")
	}
	if aClaim.Number == bMsg.Number {
		t.Fatalf("#7775: contributor B was handed #%d, the same issue contributor A holds a claim on", bMsg.Number)
	}

	release()
	var aMsg *WSMessage
	select {
	case aMsg = <-aDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("contributor A's selection did not finish after its mint was released")
	}
	if aMsg == nil || aMsg.Type != "task_assign" {
		t.Fatalf("contributor A expected a task_assign once its mint returned, got %+v", aMsg)
	}
	if aMsg.Number != aClaim.Number {
		t.Fatalf("contributor A's task_assign (#%d) does not match the claim it committed (#%d)", aMsg.Number, aClaim.Number)
	}
	connA.mu.Lock()
	aToken, aPrompt := connA.pendingToken, connA.currentPrompt
	connA.mu.Unlock()
	if aToken != "ghs_7775_gated" || aPrompt == "" {
		t.Fatalf("the credential and prompt minted after the unlock must be attached to the claim: token=%q prompt=%q", aToken, aPrompt)
	}
	if got := len(hub.leases); got != 2 {
		t.Fatalf("expected one lease per contributor, got %d", got)
	}
}

// TestSelectTask_MintFailureRollsBackClaim: the token mint now runs AFTER the
// claim is committed, so a failed mint must hand everything back — the
// connection's task, the lease, the rate-window slot — and the issue must be
// offerable again immediately. Nothing may record a task the contributor never
// received.
func TestSelectTask_MintFailureRollsBackClaim(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	s.deps.GHAppAuth = newFailingAppAuth(t)

	conn := lockScopeConn(hub, "carol", "c-carol")
	msg := hub.selectTask(conn)
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableTokenMintFailed {
		t.Fatalf("expected task_unavailable/%s, got %+v", taskUnavailableTokenMintFailed, msg)
	}

	conn.mu.Lock()
	task, prompt, token, assignedAt, renew := conn.currentTask, conn.currentPrompt, conn.pendingToken, conn.taskAssignedAt, conn.lastLeaseRenew
	conn.mu.Unlock()
	if task != nil {
		t.Fatalf("#7775: a claim whose mint failed was left on the connection: %+v", task)
	}
	if prompt != "" || token != "" || !assignedAt.IsZero() || !renew.IsZero() {
		t.Fatalf("#7775: rollback left assignment state behind: prompt=%q token=%q assignedAt=%v renew=%v", prompt, token, assignedAt, renew)
	}
	if got := len(hub.leases); got != 0 {
		t.Fatalf("#7775: a lease survived the rolled-back claim: %d lease(s)", got)
	}
	if hour, day := hub.rateWindowCounts(identityOf(conn), time.Now()); hour != 0 || day != 0 {
		t.Fatalf("#7775: the rolled-back claim still occupies a rate-window slot: hour=%d day=%d", hour, day)
	}
	if hub.isTaskInFailureCooldown("myorg/repo1", 101) {
		t.Fatalf("#7775: a cooldown was booked on an issue nobody was ever assigned")
	}

	// And the issue is offerable again — to anyone — the moment the mint works.
	s.deps.GHAppAuth = newSucceedingAppAuth(t, "ghs_7775_after_rollback")
	other := lockScopeConn(hub, "dave", "c-dave")
	again := hub.selectTask(other)
	if again == nil || again.Type != "task_assign" || again.Number != 101 {
		t.Fatalf("the issue must be offerable again after the rolled-back claim, got %+v", again)
	}
}

// TestHandleReady_SendFailureRollsBackClaim is the second half of the incident:
// the heartbeat loop had already closed the socket by the time the queued
// `ready` was served, so the task_assign write failed. The claim must be
// rolled back right there — before releaseOnDisconnect runs — so the
// disconnect path finds no task to "release" and books no cooldown on an issue
// the contributor never saw, and no rate slot or lease is spent on it.
func TestHandleReady_SendFailureRollsBackClaim(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	s.deps.GHAppAuth = newSucceedingAppAuth(t, "ghs_7775_dead_socket")

	server, client := wsPipe(t)
	conn := lockScopeConn(hub, "erin", "c-erin")
	conn.ws = server
	// The peer is gone and the hub's own side is closed — the shape the
	// heartbeat loop leaves behind after a missed pong.
	client.Close()
	server.Close()

	sess := &wsSession{h: hub, conn: server, contributor: conn}
	if stop := sess.handleReady(WSMessage{Type: "ready", Seq: 1}); !stop {
		t.Fatalf("a failed task_assign send must stop the session")
	}

	conn.mu.Lock()
	task := conn.currentTask
	conn.mu.Unlock()
	if task != nil {
		t.Fatalf("#7775: an assignment whose task_assign never left the hub was left on the dead connection: %+v", task)
	}
	if got := len(hub.leases); got != 0 {
		t.Fatalf("#7775: a lease survived the undeliverable assignment: %d lease(s)", got)
	}
	if hour, day := hub.rateWindowCounts(identityOf(conn), time.Now()); hour != 0 || day != 0 {
		t.Fatalf("#7775: the undeliverable assignment still occupies a rate-window slot: hour=%d day=%d", hour, day)
	}

	// releaseOnDisconnect now runs, as it does for every socket that closes. It
	// must find nothing to release: no cooldown on #101, and the issue offerable
	// to the next contributor at once.
	sess.releaseOnDisconnect()
	if hub.isTaskInFailureCooldown("myorg/repo1", 101) {
		t.Fatalf("#7775: the disconnect path booked a release cooldown on an issue the contributor never received")
	}
	next := lockScopeConn(hub, "frank", "c-frank")
	again := hub.selectTask(next)
	if again == nil || again.Type != "task_assign" || again.Number != 101 {
		t.Fatalf("the issue must be offerable again after the undeliverable assignment, got %+v", again)
	}
}

// TestSelectTask_ClaimReleasedDuringMintSendsNothing: if the socket drops while
// its selection is inside the GitHub round-trips, releaseOnDisconnect clears the
// claim under the connection lock. selectTask must notice on return and send
// nothing — not a task_assign for an item it no longer holds.
func TestSelectTask_ClaimReleasedDuringMintSendsNothing(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	auth, entered, release := newGatedAppAuth(t, "ghs_7775_released_mid_mint")
	s.deps.GHAppAuth = auth

	server, _ := wsPipe(t)
	conn := lockScopeConn(hub, "gina", "c-gina")
	conn.ws = server
	done := make(chan *WSMessage, 1)
	go func() { done <- hub.selectTask(conn) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("selection never reached the token mint")
	}

	// The socket dies mid-mint; the disconnect path releases the claim.
	sess := &wsSession{h: hub, conn: server, contributor: conn}
	sess.releaseOnDisconnect()
	release()

	select {
	case msg := <-done:
		if msg != nil {
			t.Fatalf("#7775: selectTask returned %+v for a claim the disconnect path had already released", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("selection did not finish after its mint was released")
	}
	conn.mu.Lock()
	token, prompt := conn.pendingToken, conn.currentPrompt
	conn.mu.Unlock()
	if token != "" || prompt != "" {
		t.Fatalf("#7775: a released claim was decorated after the fact: token=%q prompt=%q", token, prompt)
	}
}

// TestUnrecordAssignment removes exactly one matching stamp and leaves the
// identity's other assignments (and other identities) untouched.
func TestUnrecordAssignment(t *testing.T) {
	hub := &ContributeWSHub{logger: covBLogger()}
	now := time.Now()
	hub.recordAssignment("id-a", now.Add(-2*time.Minute))
	hub.recordAssignment("id-a", now.Add(-time.Minute))
	hub.recordAssignment("id-a", now.Add(-time.Minute)) // a duplicate stamp
	hub.recordAssignment("id-b", now.Add(-time.Minute))

	hub.unrecordAssignment("id-a", now.Add(-time.Minute))
	if hour, _ := hub.rateWindowCounts("id-a", now); hour != 2 {
		t.Fatalf("expected exactly one stamp removed (2 left), got %d", hour)
	}
	hub.unrecordAssignment("id-a", now.Add(-time.Hour)) // no such stamp: no-op
	if hour, _ := hub.rateWindowCounts("id-a", now); hour != 2 {
		t.Fatalf("a non-matching unrecord must be a no-op, got %d", hour)
	}
	if hour, _ := hub.rateWindowCounts("id-b", now); hour != 1 {
		t.Fatalf("other identities must be untouched, got %d", hour)
	}
	hub.unrecordAssignment("id-a", now.Add(-time.Minute))
	hub.unrecordAssignment("id-a", now.Add(-2*time.Minute))
	if hour, day := hub.rateWindowCounts("id-a", now); hour != 0 || day != 0 {
		t.Fatalf("expected an empty window, got hour=%d day=%d", hour, day)
	}
	if _, ok := hub.assignmentTimes["id-a"]; ok {
		t.Fatalf("an emptied identity must be dropped from the map")
	}
	hub.unrecordAssignment("", now) // must not panic
	hub.unrecordAssignment("id-zzz", time.Time{})
}

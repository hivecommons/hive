package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_lease_restart_test.go covers kubestellar/hive#5681: a hub restart made
// every in-flight contributor task unresumable.
//
// Leases lived only in process memory. A restart — which self-upgrade rolls (#5391)
// make routine — emptied the registry while the relays carried on working, so every
// reconnecting relay failed lookupLease, was answered "no active lease for this
// task", had its agent interrupted mid-turn, and was handed the identical issue back
// seconds later. Observed 2026-09-02: revoked at 14:24:40, the same issue reassigned
// to the same relay at 14:24:44, discarding two and a half minutes of a turn that was
// progressing normally.
//
// #4260's contribute_reconnect_resume_test.go pins the resume contract across a
// SOCKET drop and passes throughout, because that reconnect is to a live hub. These
// tests exercise the same contract across a PROCESS boundary, which had no coverage.

// waitContribDisconnect blocks until the hub's read-loop disconnect defer for
// username has fully run, using the defer's own last observable act — the "left"
// activity row — as the signal.
//
// It exists because this file's headline test is the one place ledger persistence
// stays ON while real websocket handlers run: httptest.Server.Close does not wait
// for hijacked connections, so a handler's disconnect defer (which books a release
// cooldown and writes failed-tasks.json under ws-state/) can still be running when
// the test returns — and t.TempDir's RemoveAll then races the write, failing the
// test with "ws-state: directory not empty". Waiting for the "left" row orders the
// defer's disk writes strictly before cleanup.
func waitContribDisconnect(t *testing.T, h *ContributeWSHub, username string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range h.RecentActivity() {
			if e.Username == username && e.Action == "left" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("timed out waiting for %s's disconnect defer to finish", username)
}

// restartedHub returns a second hub built over the same on-disk state as the first,
// which is what a hub restart is: a new process, an empty connection table, and
// whatever the previous process persisted. covK2Hub reuses HIVE_CONTRIBUTORS_DIR once
// it is set, so the second call boots against the first call's files.
func restartedHub(t *testing.T) *ContributeWSHub {
	t.Helper()
	hub, _ := covK2Hub(t)
	return hub
}

// --- 1. The headline contract, end to end --------------------------------------

// TestLeaseRestart_ResumeSurvivesHubRestart is the incident itself, driven through
// the real protocol: assign a task, replace the hub with a freshly constructed one
// over the same /data (a restart), and let the relay reconnect and re-assert the task
// it never stopped working. It must be resumed — not revoked, and above all not
// handed the same issue back as a brand-new assignment, which is the frame that types
// a fresh prompt into a pane whose CLI is still mid-turn.
func TestLeaseRestart_ResumeSurvivesHubRestart(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", filepath.Join(tmpDir, "contributors"))
	t.Setenv("HIVE_FEDERATION_REGISTRY_PATH", filepath.Join(tmpDir, "federation", "registry.json"))
	// Unlike setupWSTest, ledger persistence stays ON: the whole point is that the
	// lease reaches disk before the process ends.
	redirectContributeWSDisk(t, filepath.Join(tmpDir, "ws-state"))

	s1 := NewServer(0, slog.Default())
	s1.registerContributeRoutes()
	ts1 := httptest.NewServer(s1.mux)
	defer ts1.Close()
	s1.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_restart_5681")}
	s1.contributeHub.server = s1
	seedOneIssue(s1, 5617, "[v5] reviewer lane follow-ups")

	conn, reg := registerAndAuth(t, s1, ts1, "restart-resume-user")
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 5617 {
		t.Fatalf("expected task_assign for #5617, got type=%s number=%d", assign.Type, assign.Number)
	}
	if assign.TaskGen == 0 {
		t.Fatalf("task_assign carried no task_gen — the relay would have nothing to echo")
	}
	onlyLeaseIdentity(t, s1.contributeHub) // the assignment reached the registry

	// The hub restarts for an upgrade. The relay's socket closes (code 1012 since
	// #5390) and a brand-new process comes up over the same /data: new hub, empty
	// connection table, leases read back from disk.
	conn.Close()
	ts1.Close()
	// ts1.Close does not wait for the hijacked websocket handler; its disconnect
	// defer writes the release-cooldown ledger under ws-state/. Let it finish
	// before the "restarted" hub boots over the same files.
	waitContribDisconnect(t, s1.contributeHub, "restart-resume-user")

	s2 := NewServer(0, slog.Default())
	s2.registerContributeRoutes()
	ts2 := httptest.NewServer(s2.mux)
	defer ts2.Close()
	s2.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_restart_5681")}
	s2.contributeHub.server = s2
	seedOneIssue(s2, 5617, "[v5] reviewer lane follow-ups")

	// Errorf, not Fatalf: when this regresses, the protocol frames below are the
	// evidence worth seeing — the revoke, and the re-offer of the same issue.
	if len(s2.contributeHub.leases) == 0 {
		t.Errorf("#5681: the restarted hub restored no leases — every in-flight task " +
			"is unresumable and its agent will be interrupted mid-turn")
	}

	// The relay reconnects one backoff later and re-asserts the task it is still
	// working, carrying the task id and generation the PREVIOUS process issued.
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts2), nil)
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	defer conn2.Close()
	// Run after the deferred conn2.Close, before t.TempDir's RemoveAll: s2's
	// handler defer must finish its ws-state/ writes before cleanup deletes the
	// tree (the "directory not empty" TempDir flake).
	t.Cleanup(func() { waitContribDisconnect(t, s2.contributeHub, "restart-resume-user") })
	readMsg(t, conn2) // auth_challenge
	conn2.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn2) // auth_ok

	conn2.WriteJSON(WSMessage{
		Type: "task_progress", Seq: 2, TaskID: assign.TaskID, TaskGen: assign.TaskGen,
		Repo: assign.Repo, Number: assign.Number, Kind: "issue", Title: assign.Title,
		Status: "working",
	})

	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		conn2.SetReadDeadline(time.Now().Add(remaining))
		_, raw, rerr := conn2.ReadMessage()
		if rerr != nil {
			break
		}
		var m WSMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.Type {
		case "task_revoke":
			t.Fatalf("#5681: the relay reconnecting after a hub restart was told %q — "+
				"this is the revoke that interrupts a working agent", m.Reason)
		case "task_assign":
			t.Fatalf("#5681: the hub re-offered %s#%d to the very relay it just took it "+
				"from — ownership was never in question, only the record of it",
				m.Repo, m.Number)
		}
	}

	if !hubHoldsTask(s2.contributeHub, assign.TaskID) {
		t.Fatalf("#5681: the restarted hub neither revoked nor holds the task — the " +
			"relay is working an item the hub has no record of")
	}
}

// --- 2. What survives, and what deliberately does not ---------------------------

// TestLeaseRestart_UnexpiredLeaseIsReAdoptable is the unit-level core: a lease
// recorded by one process is matched by lookupLease in the next, on the exact
// {identity, task_id, repo, number, generation} tuple C4 requires.
func TestLeaseRestart_UnexpiredLeaseIsReAdoptable(t *testing.T) {
	hub1, _ := covK2Hub(t)
	const identity = "c-restart-me"
	hub1.recordLease(identity, "ct-live", "myorg/repo1", 5617, "contributor", 42, time.Now())

	hub2 := restartedHub(t)
	l := hub2.lookupLease(identity, "ct-live", "myorg/repo1", 5617, 42, time.Now())
	if l == nil {
		t.Fatalf("#5681: a lease recorded before the restart was not re-adoptable after it")
	}
	if l.repo != "myorg/repo1" || l.number != 5617 || l.gen != 42 {
		t.Fatalf("restored lease lost its match tuple: %+v", l)
	}

	// C4 is untouched: the restored record is still matched EXACTLY, so a claim that
	// differs in any field is still refused.
	for name, got := range map[string]*taskLease{
		"wrong task":       hub2.lookupLease(identity, "ct-other", "myorg/repo1", 5617, 42, time.Now()),
		"wrong generation": hub2.lookupLease(identity, "ct-live", "myorg/repo1", 5617, 43, time.Now()),
		"wrong repo":       hub2.lookupLease(identity, "ct-live", "myorg/other", 5617, 42, time.Now()),
		"wrong number":     hub2.lookupLease(identity, "ct-live", "myorg/repo1", 9999, 42, time.Now()),
		"other identity":   hub2.lookupLease("c-someone-else", "ct-live", "myorg/repo1", 5617, 42, time.Now()),
		"unversioned":      hub2.lookupLease(identity, "ct-live", "myorg/repo1", 5617, 0, time.Now()),
	} {
		if got != nil {
			t.Errorf("restart must not relax the C4 exact-match contract (%s was accepted)", name)
		}
	}
}

// TestLeaseRestart_ExpiredLeaseIsNotRestored: persistence must not resurrect a task
// that was already past its re-adoption window. A stale file is not authority.
func TestLeaseRestart_ExpiredLeaseIsNotRestored(t *testing.T) {
	hub1, _ := covK2Hub(t)
	const identity = "c-stale"
	hub1.recordLease(identity, "ct-stale", "myorg/repo1", 11, "contributor", 7, time.Now())
	// Rewind past the window and force a rewrite so the file itself holds the
	// expired record (saveLeasesLocked skips expired leases, so write it directly).
	hub1.leaseMu.Lock()
	hub1.leases[identity].expiresAt = time.Now().Add(-time.Minute)
	hub1.leaseMu.Unlock()
	writeRawLeaseFile(t, []persistedLease{{
		Identity: identity, TaskID: "ct-stale", Repo: "myorg/repo1", Number: 11,
		Tier: "contributor", Gen: 7, ExpiresAt: time.Now().Add(-time.Minute),
	}})

	hub2 := restartedHub(t)
	if hub2.lookupLease(identity, "ct-stale", "myorg/repo1", 11, 7, time.Now()) != nil {
		t.Fatalf("#5681: an already-expired lease was restored — a stale file must not " +
			"resurrect a task that is no longer re-adoptable")
	}
}

// TestLeaseRestart_RevokedLeaseStaysRevoked: every release path revokes the lease, and
// that revoke has to reach disk. A revoke lost to the next restart would resurrect a
// task the hub had already released.
func TestLeaseRestart_RevokedLeaseStaysRevoked(t *testing.T) {
	hub1, _ := covK2Hub(t)
	const identity = "c-released"
	hub1.recordLease(identity, "ct-done", "myorg/repo1", 21, "contributor", 8, time.Now())
	hub1.revokeLease(identity, "ct-done")

	hub2 := restartedHub(t)
	if hub2.lookupLease(identity, "ct-done", "myorg/repo1", 21, 8, time.Now()) != nil {
		t.Fatalf("#5681: a revoked lease came back after the restart — a released task " +
			"must never be re-adoptable")
	}
}

// TestLeaseRestart_RenewedWindowSurvives is the #4260 half across a process boundary.
// A task that has been progressing for longer than leaseTTL has a window anchored on
// its LAST report; persisting only the assignment-time window would bring it back
// already expired — exactly the bug #4260 fixed in memory.
func TestLeaseRestart_RenewedWindowSurvives(t *testing.T) {
	hub1, _ := covK2Hub(t)
	const identity = "c-longrunner"
	assigned := time.Now().Add(-(2 * leaseTTL))
	hub1.recordLease(identity, "ct-long", "myorg/repo1", 33, "contributor", 9, assigned)
	// Still working: a progress report a moment ago carried the window forward.
	hub1.renewLease(identity, "ct-long", time.Now())

	hub2 := restartedHub(t)
	if hub2.lookupLease(identity, "ct-long", "myorg/repo1", 33, 9, time.Now()) == nil {
		t.Fatalf("#5681/#4260: the restart restored the ASSIGNMENT window rather than the "+
			"renewed one, so a task progressing for %v came back unresumable", 2*leaseTTL)
	}
}

// --- 3. The restored lease must not be double-assigned --------------------------

// TestLeaseRestart_RestoredLeaseHoldsItsIssue closes the other half of the contract.
// The double-assignment guard is built from LIVE connections, which is empty right
// after a restart — so without this, the item whose holder is about to resume could
// be handed to somebody else in the meantime, turning "lose the task" into two relays
// on one issue.
func TestLeaseRestart_RestoredLeaseHoldsItsIssue(t *testing.T) {
	hub1, _ := covK2Hub(t)
	hub1.recordLease("c-holder", "ct-held", "myorg/repo1", 10, "contributor", 12, time.Now())

	hub2, s2 := covK2Hub(t)
	seedTwoIssues(s2, 10, 20)

	other := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "newcomer", ContributorID: "c-newcomer", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	msg := hub2.selectTask(other)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected the newcomer to be assigned the free issue, got %+v", msg)
	}
	if msg.Number == 10 {
		t.Fatalf("#5681: issue #10 was handed to a second contributor while its restored " +
			"lease holder was still reconnecting — a real double assignment")
	}

	// The HOLDER is never blocked by its own lease: asking for work is itself the
	// statement that it is not holding that task any more.
	holder := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "holder", ContributorID: "c-holder", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	if own := hub2.selectTask(holder); own == nil || own.Type != "task_assign" {
		t.Fatalf("a contributor's own restored lease must not lock it out of work: %+v", own)
	}
}

// TestLeaseRestart_HoldLapsesAfterGrace pins that this is a restart measure, NOT a
// change to what a lease means in steady state. A dropped socket keeps its lease so
// the relay can resume (#4260) while its item merely cools down (#2356); honouring
// leases as holds for the full TTL would silently replace that hedge with a
// 30-minute park on every disconnect.
func TestLeaseRestart_HoldLapsesAfterGrace(t *testing.T) {
	hub, _ := covK2Hub(t)
	hub.recordLease("c-holder", "ct-held", "myorg/repo1", 10, "contributor", 12, time.Now())
	hub.leaseMu.Lock()
	hub.leases["c-holder"].restored = true // as if loaded at boot
	hub.leaseMu.Unlock()

	if len(hub.leasedIssueKeys("c-other", time.Now())) != 1 {
		t.Fatalf("a restored lease must hold its item during the post-restart grace window")
	}
	past := hub.startedAt.Add(leaseHoldGraceAfterStart + time.Second)
	if got := hub.leasedIssueKeys("c-other", past); len(got) != 0 {
		t.Fatalf("after the grace window the live-connection guard is back in sole "+
			"charge; leases must contribute no holds, got %v", got)
	}

	// A lease minted by THIS process is never a hold: its holder has a live
	// connection, which the existing guard already covers.
	hub.recordLease("c-fresh", "ct-fresh", "myorg/repo1", 77, "contributor", 13, time.Now())
	for key := range hub.leasedIssueKeys("c-other", time.Now()) {
		if key == "myorg/repo1#77" {
			t.Fatalf("a lease minted in this process must not act as a restart hold")
		}
	}
}

// --- 4. The generation fence must not alias across the restart ------------------

// TestLeaseRestart_GenerationAdvancesPastRestoredLeases: taskGen is an in-memory
// counter that restarts at zero. Without raising it past every restored generation, a
// post-restart assignment would mint numbers that ALIAS the restored ones, and the
// #2568 Gate would accept a pre-restart straggler against a brand-new task.
func TestLeaseRestart_GenerationAdvancesPastRestoredLeases(t *testing.T) {
	hub1, _ := covK2Hub(t)
	hub1.recordLease("c-a", "ct-a", "myorg/repo1", 1, "contributor", 75, time.Now())

	hub2 := restartedHub(t)
	if got := hub2.nextTaskGen(); got <= 75 {
		t.Fatalf("#5681/#2568: the first generation minted after the restart was %d, which "+
			"aliases the restored lease's generation 75 — a pre-restart straggler would "+
			"be accepted against a new task", got)
	}
}

// --- 5. Persistence hygiene ------------------------------------------------------

// TestLeaseRestart_FileIsOwnerOnly: the registry is the C4 authorization record a
// resume is matched against, so it is owner-only on both sides — unlike the sibling
// contributor ledgers, which are reports.
func TestLeaseRestart_FileIsOwnerOnly(t *testing.T) {
	hub, _ := covK2Hub(t)
	hub.recordLease("c-perm", "ct-perm", "myorg/repo1", 3, "contributor", 4, time.Now())

	info, err := os.Stat(hub.taskLeasesPath())
	if err != nil {
		t.Fatalf("lease registry was not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("lease registry mode = %04o, want 0600", perm)
	}
}

// TestLeaseRestart_MalformedFileStartsEmpty: an unreadable registry must degrade to
// "no leases" — the pre-fix behavior — rather than panic or block startup.
func TestLeaseRestart_MalformedFileStartsEmpty(t *testing.T) {
	hub1, _ := covK2Hub(t)
	hub1.recordLease("c-x", "ct-x", "myorg/repo1", 1, "contributor", 2, time.Now())
	if err := os.WriteFile(hub1.taskLeasesPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	hub2 := restartedHub(t)
	if n := len(hub2.leases); n != 0 {
		t.Fatalf("a malformed registry must start empty, got %d leases", n)
	}
	if hub2.lookupLease("c-x", "ct-x", "myorg/repo1", 1, 2, time.Now()) != nil {
		t.Fatalf("a malformed registry must grant nothing")
	}
}

// TestLeaseRestart_ExpiredLeasesArePruned: a relay that never comes back leaves a
// lease nothing ever looks up. cleanupLoop's prune keeps it from holding its issue —
// and from sitting in the registry — until the process ends.
func TestLeaseRestart_ExpiredLeasesArePruned(t *testing.T) {
	hub, _ := covK2Hub(t)
	hub.recordLease("c-gone", "ct-gone", "myorg/repo1", 4, "contributor", 5, time.Now())
	hub.leaseMu.Lock()
	hub.leases["c-gone"].expiresAt = time.Now().Add(-time.Second)
	hub.leaseMu.Unlock()

	if n := hub.pruneExpiredLeases(time.Now()); n != 1 {
		t.Fatalf("pruned %d expired leases, want 1", n)
	}
	if n := len(hub.leases); n != 0 {
		t.Fatalf("registry still holds %d leases after the prune", n)
	}
	// Idempotent: nothing left to drop.
	if n := hub.pruneExpiredLeases(time.Now()); n != 0 {
		t.Fatalf("prune dropped %d on a clean registry, want 0", n)
	}
}

// --- helpers -------------------------------------------------------------------

// writeRawLeaseFile puts an arbitrary record set on disk, so a test can present the
// next boot with a file saveLeasesLocked would never have produced.
func writeRawLeaseFile(t *testing.T, records []persistedLease) {
	t.Helper()
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(taskLeasesFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taskLeasesFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedTwoIssues puts two admissible issues in the status payload so a test can prove
// which one selectTask picked.
func seedTwoIssues(s *Server, a, b int) {
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name: "repo1",
			Full: "myorg/repo1",
			ActionableIssues: []any{
				map[string]any{
					"number": float64(a), "title": "Issue A",
					"url": "https://github.com/myorg/repo1/issues/" + itoa(a), "author": "someone",
				},
				map[string]any{
					"number": float64(b), "title": "Issue B",
					"url": "https://github.com/myorg/repo1/issues/" + itoa(b), "author": "someone",
				},
			},
		}},
	}
	s.statusMu.Unlock()
}

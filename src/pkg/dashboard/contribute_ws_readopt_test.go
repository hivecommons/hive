package dashboard

import (
	"log/slog"
	"testing"
)

// Tests for taskReadoptedByLiveConnection (kubestellar/hive#5322): the guard
// that suppresses a stale socket's disconnect-release when the SAME contributor
// identity has already re-adopted the SAME task on a live connection. The match
// is deliberately narrow — same identity AND same task id AND same canonical
// repo/number — and these tests pin each rejection branch so the guard can
// never widen into suppressing a genuine abandonment.

func newReadoptHub(t *testing.T) *ContributeWSHub {
	t.Helper()
	hub := NewContributeWSHub(slog.Default(), nil)
	t.Cleanup(hub.Close)
	return hub
}

func readoptConn(contributorID, session string, task *WSTaskAssign) *ContributorConnection {
	return &ContributorConnection{
		profile:     &ContributorProfile{ContributorID: contributorID},
		session:     session,
		currentTask: task,
	}
}

func readoptTask(id, repo string, number int) *WSTaskAssign {
	return &WSTaskAssign{TaskID: id, Repo: repo, Number: number}
}

func TestTaskReadoptedNilHubTaskAndEmptyID(t *testing.T) {
	self := readoptConn("alice", "", nil)
	task := readoptTask("t1", "org/repo", 7)

	var nilHub *ContributeWSHub
	if nilHub.taskReadoptedByLiveConnection(self, task) {
		t.Error("nil hub must report false")
	}

	hub := newReadoptHub(t)
	if hub.taskReadoptedByLiveConnection(self, nil) {
		t.Error("nil task must report false")
	}
	if hub.taskReadoptedByLiveConnection(self, readoptTask("", "org/repo", 7)) {
		t.Error("empty TaskID must report false")
	}
}

func TestTaskReadoptedEmptyIdentity(t *testing.T) {
	hub := newReadoptHub(t)
	task := readoptTask("t1", "org/repo", 7)
	hub.connections["live"] = readoptConn("alice", "", task)

	// A self with no profile has no identity; the guard must not suppress.
	if hub.taskReadoptedByLiveConnection(nil, task) {
		t.Error("nil self (empty identity) must report false")
	}
	if hub.taskReadoptedByLiveConnection(&ContributorConnection{}, task) {
		t.Error("profile-less self (empty identity) must report false")
	}
}

func TestTaskReadoptedSkipsSelfAndNilConns(t *testing.T) {
	hub := newReadoptHub(t)
	task := readoptTask("t1", "org/repo", 7)
	self := readoptConn("alice", "", task)
	// Only holders are self (skipped by pointer identity) and a nil entry.
	hub.connections["self"] = self
	hub.connections["nil"] = nil

	if hub.taskReadoptedByLiveConnection(self, task) {
		t.Error("self holding the task is not a re-adoption; must report false")
	}
}

func TestTaskReadoptedRejectsMismatches(t *testing.T) {
	task := readoptTask("t1", "org/repo", 7)

	cases := []struct {
		name  string
		other *ContributorConnection
	}{
		{"different identity", readoptConn("bob", "", readoptTask("t1", "org/repo", 7))},
		{"no current task", readoptConn("alice", "", nil)},
		{"different task id", readoptConn("alice", "", readoptTask("t2", "org/repo", 7))},
		{"different number", readoptConn("alice", "", readoptTask("t1", "org/repo", 8))},
		{"different repo", readoptConn("alice", "", readoptTask("t1", "org/other", 7))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newReadoptHub(t)
			hub.connections["other"] = tc.other
			self := readoptConn("alice", "", nil)
			if hub.taskReadoptedByLiveConnection(self, task) {
				t.Errorf("%s must not count as re-adoption", tc.name)
			}
		})
	}
}

func TestTaskReadoptedMatchSuppresses(t *testing.T) {
	hub := newReadoptHub(t)
	task := readoptTask("t1", "org/repo", 7)
	hub.connections["live"] = readoptConn("alice", "", readoptTask("t1", "org/repo", 7))

	self := readoptConn("alice", "", nil)
	if !hub.taskReadoptedByLiveConnection(self, task) {
		t.Error("same identity + task id + repo + number on another live connection must report true")
	}
}

func TestTaskReadoptedSessionScopedIdentity(t *testing.T) {
	// Multi-session-per-account: identityOf() is ContributorID#session, so the
	// SAME account on a DIFFERENT session is a different identity and must not
	// suppress the release, while the same session does.
	task := readoptTask("t1", "org/repo", 7)

	hub := newReadoptHub(t)
	hub.connections["live"] = readoptConn("alice", "s2", readoptTask("t1", "org/repo", 7))
	if hub.taskReadoptedByLiveConnection(readoptConn("alice", "s1", nil), task) {
		t.Error("same account but different session is a different identity; must report false")
	}
	if !hub.taskReadoptedByLiveConnection(readoptConn("alice", "s2", nil), task) {
		t.Error("same account and same session must report true")
	}
}

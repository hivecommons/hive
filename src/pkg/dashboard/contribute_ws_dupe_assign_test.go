package dashboard

import (
	"sync"
	"testing"
	"time"
)

// singleIssueStatus seeds a status with EXACTLY ONE admissible issue in a repo
// whose Full (owner/repo) differs from its bare Name. The single-issue candidate
// set is the deterministic setup for the double-assignment race: if two selections
// both pick it, the queue handed the same work to two contributors (#2644).
func singleIssueStatus(s *Server) {
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "fsdk-containers",
				Full: "projectbluefin/fsdk-containers",
				ActionableIssues: []any{
					map[string]any{
						"number": float64(48),
						"title":  "the only admissible issue",
						"url":    "https://github.com/projectbluefin/fsdk-containers/issues/48",
						"author": "someone",
					},
				},
			},
		},
	}
	s.statusMu.Unlock()
}

// TestDupeAssign_ConcurrentSelectTaskSingleIssue is the #2644 concurrency
// regression. Two contributors' connections request work at the same instant
// against a candidate set of exactly ONE issue. selectTask must be atomic
// offer->claim: it chooses AND records the issue as in-flight under a single
// lock, so a concurrent selectTask for the second contributor sees the issue is
// already held and cannot pick it. Exactly one connection gets the issue; the
// other gets nothing (no_matching_work) — never the SAME issue twice.
func TestDupeAssign_ConcurrentSelectTaskSingleIssue(t *testing.T) {
	hub, s := covK2Hub(t)
	singleIssueStatus(s)

	connA := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "ct-a", ContributorID: "c-a", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	connB := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "ct-b", ContributorID: "c-b", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	// Both connections must be REGISTERED on the hub: selectTask's in-flight scan
	// reads currentTask across h.connections, so a claim by one is only visible to
	// the other's scan if both are registered. Committing currentTask on the
	// connection alone is not enough — it must be reachable from the hub map.
	hub.mu.Lock()
	hub.connections["conn-a"] = connA
	hub.connections["conn-b"] = connB
	hub.mu.Unlock()

	// Fire both selections concurrently and gate them on a single barrier so they
	// contend on selectMu as closely as the runtime allows. Repeat a few rounds so
	// a non-atomic offer->claim would be caught by at least one interleaving.
	const rounds = 200
	for round := 0; round < rounds; round++ {
		// Reset both connections to idle at the top of each round.
		for _, c := range []*ContributorConnection{connA, connB} {
			c.mu.Lock()
			c.currentTask = nil
			c.mu.Unlock()
		}

		var start sync.WaitGroup
		start.Add(1)
		var done sync.WaitGroup
		results := make([]*WSMessage, 2)
		for i, c := range []*ContributorConnection{connA, connB} {
			done.Add(1)
			go func(idx int, conn *ContributorConnection) {
				defer done.Done()
				start.Wait() // release both goroutines together
				results[idx] = hub.selectTask(conn)
			}(i, c)
		}
		start.Done()
		done.Wait()

		assigned := 0
		var assignedTo string
		for idx, r := range results {
			if r != nil && r.Type == "task_assign" {
				assigned++
				assignedTo = []string{"ct-a", "ct-b"}[idx]
				if r.Number != 48 {
					t.Fatalf("round %d: unexpected issue number %d", round, r.Number)
				}
			}
		}
		if assigned != 1 {
			t.Fatalf("round %d: the single issue #48 was handed out %d times (want exactly 1) — the queue double-assigned it (#2644); last winner=%s",
				round, assigned, assignedTo)
		}

		// The winner must be recorded as in-flight so the NEXT scan also excludes it.
		active := hub.activeIssueKeys()
		if !active["projectbluefin/fsdk-containers#48"] {
			t.Fatalf("round %d: winner did not register the issue as active — the in-flight guard is not authoritative", round)
		}
	}
}

// TestDupeAssign_CanonicalRepoKeyNormalizesResumedRepo pins the #2644 ROOT CAUSE:
// the task_progress RESUME path re-populated currentTask.Repo from the raw
// client-supplied msg.Repo. When a relay reports the repo in any spelling other
// than the hub's repo.Full, every server key built off currentTask.Repo
// ("repo#number" — the activeIssues guard AND the cooldowns) missed, so the
// in-flight issue was NOT excluded and the SAME issue could be handed to a second
// contributor. canonicalRepoKey maps any client spelling back to repo.Full.
func TestDupeAssign_CanonicalRepoKeyNormalizesResumedRepo(t *testing.T) {
	hub, s := covK2Hub(t)
	singleIssueStatus(s)

	cases := []struct {
		name  string
		input string
	}{
		{"bare name", "fsdk-containers"},
		{"already full", "projectbluefin/fsdk-containers"},
		{"case-variant full", "ProjectBluefin/FSDK-Containers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hub.canonicalRepoKey(tc.input)
			if got != "projectbluefin/fsdk-containers" {
				t.Fatalf("canonicalRepoKey(%q) = %q, want the canonical repo.Full %q",
					tc.input, got, "projectbluefin/fsdk-containers")
			}
		})
	}
}

// TestDupeAssign_ResumedTaskExcludedFromSelection is the end-to-end #2644 guard:
// a contributor holds issue #48 whose currentTask.Repo was set on the RESUME path
// from a bare, non-Full client spelling ("fsdk-containers"). Because that repo is
// now canonicalised to repo.Full, a SECOND contributor's selectTask must see the
// issue as in-flight and refuse it (no other issue exists), rather than handing
// out #48 a second time. Before the fix the bare key never matched the
// repo.Full-keyed candidate lookup, so #48 was re-offered — the exact bug.
func TestDupeAssign_ResumedTaskExcludedFromSelection(t *testing.T) {
	hub, s := covK2Hub(t)
	singleIssueStatus(s)

	// Holder resumes its task via the task_progress path with a BARE repo spelling,
	// exactly as the fixed handler does (canonicalRepoKey applied). This mirrors
	// contribute_ws.go's "task_progress" case.
	holder := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "holder", ContributorID: "c-h", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	holder.mu.Lock()
	holder.currentTask = &WSTaskAssign{
		TaskID: "ct-resumed-48",
		Kind:   "issue",
		Repo:   hub.canonicalRepoKey("fsdk-containers"), // bare spelling from the relay
		Number: 48,
	}
	holder.mu.Unlock()

	hub.mu.Lock()
	hub.connections["conn-holder"] = holder
	hub.mu.Unlock()

	// The active key the holder registers must be the canonical repo.Full form so
	// the candidate lookup (also repo.Full-keyed) excludes it.
	if !hub.activeIssueKeys()["projectbluefin/fsdk-containers#48"] {
		t.Fatalf("resumed task did not register under the canonical repo.Full key; the in-flight guard would miss it (#2644)")
	}

	// A second contributor asks for work. #48 is the only issue and it is in-flight,
	// so the selection must NOT hand it out again.
	other := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "other", ContributorID: "c-o", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	msg := hub.selectTask(other)
	if msg != nil && msg.Type == "task_assign" {
		t.Fatalf("dupe assign: #48 was handed to a second contributor while still held via a resumed task (#2644); got %s#%d",
			msg.Repo, msg.Number)
	}
	if msg == nil || msg.Type != "task_unavailable" {
		t.Fatalf("expected a task_unavailable no-work ack (the only issue is in-flight), got %+v", msg)
	}
}

// TestDupeAssign_StaleReconnectCannotResumeIssueHeldByAnotherConnection covers
// the #3093 seam: after a disconnect/lease expiry, an old relay can reconnect
// and re-assert its locally retained task via task_progress. If the issue was
// already granted to another connection while the first relay was away, the
// resume path must not create a second in-flight owner for the same repo#issue.
func TestDupeAssign_StaleReconnectCannotResumeIssueHeldByAnotherConnection(t *testing.T) {
	hub, s := covK2Hub(t)
	singleIssueStatus(s)

	holder := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "new-owner", ContributorID: "c-new", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	holder.mu.Lock()
	holder.currentTask = &WSTaskAssign{TaskID: "ct-new-48", Kind: "issue", Repo: "projectbluefin/fsdk-containers", Number: 48}
	holder.mu.Unlock()

	returning := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "stale-owner", ContributorID: "c-old", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	hub.mu.Lock()
	hub.connections["conn-new"] = holder
	hub.connections["conn-old"] = returning
	hub.mu.Unlock()

	if !hub.taskHeldByAnotherConnection(returning, "fsdk-containers", 48) {
		t.Fatalf("stale reconnect did not detect that projectbluefin/fsdk-containers#48 is already held")
	}

	returning.mu.Lock()
	if hub.taskHeldByAnotherConnection(returning, "fsdk-containers", 48) {
		// This is the branch the task_progress resume handler now takes: reject the
		// stale resume and leave currentTask nil.
	} else {
		returning.currentTask = &WSTaskAssign{TaskID: "ct-old-48", Kind: "issue", Repo: hub.canonicalRepoKey("fsdk-containers"), Number: 48}
	}
	got := returning.currentTask
	returning.mu.Unlock()
	if got != nil {
		t.Fatalf("stale reconnect resurrected duplicate ownership: %+v", got)
	}
}

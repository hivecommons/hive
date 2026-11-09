package dashboard

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// contribute_run_duration_resume_test.go — a run adopted on the task_progress
// resume path was booked with no duration_s.
//
// The resume path rebuilds currentTask on a NEW connection from the lease, and
// that connection never saw the assignment, so taskAssignedAt stayed zero and
// every terminal booking skipped the duration. Observed on 2026-11-09: a task
// held across the 16:46Z hub restart failed at 17:16:53Z with no duration, and
// a task whose socket flapped two minutes in shipped 14 minutes later with no
// duration either. The lease now carries the original assignment time
// (persisted, so it survives the restart), and the run log measures from it.

const (
	durResumeIdentity = "c-dur-resume"
	durResumeUser     = "dur-resume-user"
	durResumeTask     = "ct-myorg/repo1-475-1794235485"
	durResumeRepo     = "myorg/repo1"
	durResumeNumber   = 475
	durResumeGen      = uint64(1868)
	durResumeAge      = 14 * time.Minute
)

// resumeOnFreshConnection re-asserts the leased task from a brand-new
// connection, exactly as a reconnecting relay does, and returns the session
// that now holds it.
func resumeOnFreshConnection(t *testing.T, hub *ContributeWSHub) *wsSession {
	t.Helper()
	conn := &ContributorConnection{
		profile: &ContributorProfile{
			ContributorID:  durResumeIdentity,
			GitHubUsername: durResumeUser,
			TrustTier:      "contributor",
		},
		cliBackend: "pi",
		model:      "lemonade/test-model",
		role:       "contributor",
	}
	s := &wsSession{h: hub, contributor: conn}
	s.handleTaskProgress(WSMessage{
		Type: "task_progress", TaskID: durResumeTask, TaskGen: durResumeGen,
		Repo: durResumeRepo, Number: durResumeNumber, Kind: "issue", Status: "working",
	})
	conn.mu.Lock()
	held := conn.currentTask != nil && conn.currentTask.TaskID == durResumeTask
	assignedAt := conn.taskAssignedAt
	conn.mu.Unlock()
	if !held {
		t.Fatalf("setup: the resume was not adopted from the lease")
	}
	// The resume deliberately leaves taskAssignedAt alone: its other readers
	// (the fast-failure streak among them) keep their resume behaviour.
	if !assignedAt.IsZero() {
		t.Fatalf("taskAssignedAt = %v after a resume, want zero (unchanged behaviour)", assignedAt)
	}
	return s
}

// runRecordFor returns the run-log line booked for taskID, raw and decoded.
func runRecordFor(t *testing.T, hub *ContributeWSHub, taskID string) (string, TaskRunRecord) {
	t.Helper()
	f, err := os.Open(hub.taskRunLogPath())
	if err != nil {
		t.Fatalf("open run log: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var rec TaskRunRecord
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.TaskID == taskID {
			return sc.Text(), rec
		}
	}
	t.Fatalf("no run record for %s in %s", taskID, hub.taskRunLogPath())
	return "", TaskRunRecord{}
}

func finishResumedTask(s *wsSession, outcome string) {
	switch outcome {
	case outcomeCompleted:
		s.handleTaskComplete(WSMessage{Type: "task_complete", TaskID: durResumeTask, TaskGen: durResumeGen, Result: "completed"})
	default:
		s.handleTaskFailed(WSMessage{Type: "task_failed", TaskID: durResumeTask, TaskGen: durResumeGen,
			Reason: "no observed progress for 30min", FailureKind: TaskFailureKindEnvironment})
	}
}

// (a) A task assigned 14 minutes ago, held across a hub restart (the lease is
// read back from disk) or a socket flap (the lease is still in memory), then
// finished on the new connection, is booked with a duration measured from the
// ORIGINAL assignment. On the v6 tip the record carried no duration_s at all.
func TestResumedRunDuration_MeasuredFromOriginalAssignment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
		outcome string
	}{
		{"hub restart then completed", true, outcomeCompleted},
		{"hub restart then failed", true, outcomeFailed},
		{"socket flap then completed", false, outcomeCompleted},
		{"socket flap then failed", false, outcomeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
			hub, _ := covK2Hub(t)
			assignedAt := time.Now().Add(-durResumeAge)
			if err := hub.recordLease(durResumeIdentity, durResumeTask, durResumeRepo, durResumeNumber,
				"contributor", durResumeGen, assignedAt); err != nil {
				t.Fatalf("record lease: %v", err)
			}
			if tc.restart {
				hub = restartedHub(t)
				hub.leaseMu.Lock()
				l := hub.leaseForLocked(durResumeIdentity, durResumeTask)
				var restoredAt time.Time
				if l != nil {
					restoredAt = l.assignedAt
				}
				hub.leaseMu.Unlock()
				if l == nil || !l.restored {
					t.Fatalf("setup: the lease was not restored from disk")
				}
				if !restoredAt.Equal(assignedAt) {
					t.Fatalf("restored lease assignedAt = %v, want the original %v", restoredAt, assignedAt)
				}
			}

			s := resumeOnFreshConnection(t, hub)
			finishResumedTask(s, tc.outcome)

			line, rec := runRecordFor(t, hub, durResumeTask)
			if rec.Outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q: %s", rec.Outcome, tc.outcome, line)
			}
			if !strings.Contains(line, `"duration_s"`) {
				t.Fatalf("resumed run booked without duration_s: %s", line)
			}
			lo := (durResumeAge - time.Second).Seconds()
			hi := (durResumeAge + time.Minute).Seconds()
			if rec.DurationS < lo || rec.DurationS > hi {
				t.Fatalf("duration_s = %.1f, want about %.0f (measured from the original assignment, not the resume)",
					rec.DurationS, durResumeAge.Seconds())
			}
		})
	}
}

// (b) A lease written before leases recorded their assignment time restores
// with the start unknown. The resumed run must leave duration_s OFF the record
// — not 0, and not the time since the resume, which would pass a 14-minute run
// off as a few seconds.
func TestResumedRunDuration_UnknownStartOmitted(t *testing.T) {
	for _, outcome := range []string{outcomeCompleted, outcomeFailed} {
		t.Run(outcome, func(t *testing.T) {
			t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
			hub, _ := covK2Hub(t)
			if err := hub.recordLease(durResumeIdentity, durResumeTask, durResumeRepo, durResumeNumber,
				"contributor", durResumeGen, time.Now().Add(-durResumeAge)); err != nil {
				t.Fatalf("record lease: %v", err)
			}
			// Rewrite the registry as an older hub wrote it: no assigned_at.
			path := hub.taskLeasesPath()
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read lease registry: %v", err)
			}
			var records []map[string]any
			if err := json.Unmarshal(raw, &records); err != nil {
				t.Fatalf("decode lease registry: %v", err)
			}
			if len(records) != 1 || records[0]["assigned_at"] == nil {
				t.Fatalf("setup: expected one lease record carrying assigned_at, got %s", raw)
			}
			delete(records[0], "assigned_at")
			legacy, _ := json.Marshal(records)
			if err := os.WriteFile(path, legacy, 0o600); err != nil {
				t.Fatalf("write legacy lease registry: %v", err)
			}

			hub = restartedHub(t)
			s := resumeOnFreshConnection(t, hub)
			finishResumedTask(s, outcome)

			line, rec := runRecordFor(t, hub, durResumeTask)
			if rec.Outcome != outcome {
				t.Fatalf("outcome = %q, want %q: %s", rec.Outcome, outcome, line)
			}
			if strings.Contains(line, `"duration_s"`) {
				t.Fatalf("a run whose start is unknown must omit duration_s, got: %s", line)
			}
		})
	}
}

// A fresh assignment on a connection that earlier resumed a task measures from
// its own assignment, not from the resumed task's lease.
func TestRunStartLocked_FreshAssignmentClearsResumeAnchor(t *testing.T) {
	assigned := time.Now().Add(-time.Minute)
	c := &ContributorConnection{adoptedOnResume: true, resumedAssignedAt: time.Now().Add(-time.Hour)}
	c.taskAssignedAt = assigned
	c.clearResumeAnchorLocked()
	if got := c.runStartLocked(); !got.Equal(assigned) {
		t.Fatalf("runStartLocked = %v, want the fresh assignment %v", got, assigned)
	}
	c = &ContributorConnection{adoptedOnResume: true, taskAssignedAt: assigned}
	if got := c.runStartLocked(); !got.IsZero() {
		t.Fatalf("runStartLocked = %v for a resume with an unknown start, want zero", got)
	}
}

func TestRunDurationSeconds_RejectsUnknownNegativeAndAbsurd(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		start  time.Time
		wantOK bool
		want   float64
	}{
		{"unknown start", time.Time{}, false, 0},
		{"start in the future", now.Add(time.Minute), false, 0},
		{"older than the ceiling", now.Add(-maxRecordedRunDuration - time.Hour), false, 0},
		{"ordinary run", now.Add(-90 * time.Second), true, 90},
		{"long but plausible run", now.Add(-5 * time.Hour), true, (5 * time.Hour).Seconds()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := runDurationSeconds(tc.start, now)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("runDurationSeconds = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// The registry round-trips assigned_at, and a record without it loads with the
// start unknown rather than failing to load.
func TestLeasePersist_AssignedAtRoundTripsAndIsOptional(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	hub, _ := covK2Hub(t)
	at := time.Now().Add(-3 * time.Minute)
	if err := hub.recordLease("c-a", "t-a", "myorg/repo1", 1, "contributor", 5, at); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	raw, err := os.ReadFile(hub.taskLeasesPath())
	if err != nil {
		t.Fatalf("read lease registry: %v", err)
	}
	var records []persistedLease
	if err := json.Unmarshal(raw, &records); err != nil || len(records) != 1 {
		t.Fatalf("decode lease registry: %v (%s)", err, raw)
	}
	if records[0].AssignedAt == nil || !records[0].AssignedAt.Equal(at) {
		t.Fatalf("persisted assigned_at = %v, want %v", records[0].AssignedAt, at)
	}

	records[0].AssignedAt = nil
	legacy, _ := json.Marshal(records)
	if strings.Contains(string(legacy), "assigned_at") {
		t.Fatalf("an unknown assignment time must not be serialized: %s", legacy)
	}
	if err := os.WriteFile(hub.taskLeasesPath(), legacy, 0o600); err != nil {
		t.Fatalf("write legacy registry: %v", err)
	}
	hub2 := restartedHub(t)
	l := hub2.lookupLease("c-a", "t-a", "myorg/repo1", 1, 5, time.Now())
	if l == nil {
		t.Fatalf("a lease record without assigned_at must still load")
	}
	if !l.assignedAt.IsZero() {
		t.Fatalf("legacy lease assignedAt = %v, want zero (unknown)", l.assignedAt)
	}
}

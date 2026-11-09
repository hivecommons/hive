package dashboard

import (
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
)

const unassignedCompleteMsg = "task_complete for unassigned task ignored (assignment left intact)"

// unassignedCompleteLines returns the captured log lines that carry the
// "unassigned task ignored" message.
func unassignedCompleteLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, unassignedCompleteMsg) {
			lines = append(lines, line)
		}
	}
	return lines
}

// completeForeignTask sends task_complete for taskID to a connection that
// holds a different, hub-assigned task, and returns the connection, the task it
// held, and the captured log output.
func completeForeignTask(t *testing.T, taskID string) (*ContributorConnection, *WSTaskAssign, string) {
	t.Helper()
	hub, _ := covK2Hub(t)
	logs := &lockedBuffer{}
	hub.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	held := &WSTaskAssign{TaskID: "ct-acme/repo-7-1794245000", Kind: "issue", Repo: "acme/repo", Number: 7}
	conn := &ContributorConnection{
		profile:             &ContributorProfile{GitHubUsername: "Karibhou"},
		role:                "contributor",
		currentTask:         held,
		currentTaskGen:      5,
		pendingToken:        "held-credential",
		credentialDelivered: true,
	}
	// The relay sends no task_gen for its local task, so TaskGen is 0.
	(&wsSession{h: hub, contributor: conn}).handleTaskComplete(WSMessage{
		Type: "task_complete", TaskID: taskID, Result: "completed",
	})
	return conn, held, logs.String()
}

func assertAssignmentIntact(t *testing.T, conn *ContributorConnection, held *WSTaskAssign) {
	t.Helper()
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.currentTask != held || conn.currentTaskGen != 5 {
		t.Fatalf("assignment changed: currentTask=%+v gen=%d, want %+v gen=5", conn.currentTask, conn.currentTaskGen, held)
	}
	if conn.pendingToken != "held-credential" || !conn.credentialDelivered {
		t.Fatalf("credential state changed: pendingToken=%q delivered=%v", conn.pendingToken, conn.credentialDelivered)
	}
}

// TestTaskCompleteRelayLocalReviewNotWarn: the contributor relay sends
// task_complete for its own local PR-review task (a `pr-review-` id the hub
// never assigned) on purpose. The hub ignores it and leaves the real assignment
// alone, which is correct, but logged every one at WARN. It is now a DEBUG line.
func TestTaskCompleteRelayLocalReviewNotWarn(t *testing.T) {
	conn, held, out := completeForeignTask(t, "pr-review-1794245570604")

	assertAssignmentIntact(t, conn, held)
	lines := unassignedCompleteLines(out)
	if len(lines) != 1 {
		t.Fatalf("want exactly one unassigned-complete line, got %d:\n%s", len(lines), out)
	}
	if strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "level=DEBUG") {
		t.Fatalf("relay-local pr-review completion should log at DEBUG, not WARN:\n%s", lines[0])
	}
	if !strings.Contains(lines[0], "task=pr-review-1794245570604") {
		t.Fatalf("debug line should still name the task:\n%s", lines[0])
	}
}

// TestTaskCompleteUnassignedStillWarns: any other completion for a task this
// connection does not hold is still a WARN, with the assignment left intact.
func TestTaskCompleteUnassignedStillWarns(t *testing.T) {
	conn, held, out := completeForeignTask(t, "FAKE-NEVER-ASSIGNED")

	assertAssignmentIntact(t, conn, held)
	lines := unassignedCompleteLines(out)
	if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") {
		t.Fatalf("an unassigned non-pr-review completion should log one WARN line, got:\n%s", out)
	}
}

func TestIsRelayLocalTaskID(t *testing.T) {
	for id, want := range map[string]bool{
		"pr-review-1794245570604":   true,
		"pr-review-":                true,
		"":                          false,
		"pr-review":                 false,
		"ct-acme/repo-7-1794245000": false,
		"fix-karibhou-3":            false,
		"ct-acme/pr-review-7-1":     false,
		"PR-REVIEW-1794245570604":   false,
	} {
		if got := isRelayLocalTaskID(id); got != want {
			t.Errorf("isRelayLocalTaskID(%q) = %v, want %v", id, got, want)
		}
	}
}

// TestRelayLocalTaskIDPrefixMatchesRelay: the hub's prefix and the relay's
// LOCAL_TASK_ID_PREFIX are two spellings of one value in one tree. If the relay
// renames its prefix, the hub would go back to logging every review at WARN.
func TestRelayLocalTaskIDPrefixMatchesRelay(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../bin/contributor-relay.js")
	if err != nil {
		t.Fatalf("read bin/contributor-relay.js: %v (if the relay moved, re-point this test rather than deleting it)", err)
	}
	m := regexp.MustCompile(`(?m)^const LOCAL_TASK_ID_PREFIX = '([^']*)';`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("could not find LOCAL_TASK_ID_PREFIX in bin/contributor-relay.js; if it was renamed, re-point this test in the same PR")
	}
	if got := string(m[1]); got != relayLocalTaskIDPrefix {
		t.Errorf("bin/contributor-relay.js uses local task prefix %q but the hub expects %q; change both together", got, relayLocalTaskIDPrefix)
	}
}

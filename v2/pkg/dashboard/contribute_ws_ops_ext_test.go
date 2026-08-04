package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// --- #2546: the three formerly-silent selectTask paths now emit an explicit
// task_unavailable with a machine-readable reason ---------------------------------

// TestNoWork_ContributionSuspended (#2546): when the operator has suspended the
// whole contribute queue, selectTask returns an explicit contribution_suspended
// negative-ack instead of a bare nil (which sent NOTHING to the contributor).
func TestNoWork_ContributionSuspended(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s) // work IS available; suspension, not scarcity, is the reason.
	s.deps.Config.Hub.ContributeSuspended = true

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "newcomer"},
		lastPong: time.Now(),
	}
	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("suspended queue must send an explicit negative-ack, got nil (silent)")
	}
	if msg.Type != "task_unavailable" || msg.Reason != taskUnavailableContributionSuspended {
		t.Fatalf("expected task_unavailable/%s, got type=%q reason=%q",
			taskUnavailableContributionSuspended, msg.Type, msg.Reason)
	}
}

// TestNoWork_HubNotReady (#2546): with no status snapshot yet, selectTask returns
// an explicit hub_not_ready negative-ack rather than a silent nil.
func TestNoWork_HubNotReady(t *testing.T) {
	hub, s := covK2Hub(t)
	// Deliberately do NOT install a status snapshot: s.status stays nil.
	s.statusMu.Lock()
	s.status = nil
	s.statusMu.Unlock()

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "newcomer"},
		lastPong: time.Now(),
	}
	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("no-status must send an explicit negative-ack, got nil (silent)")
	}
	if msg.Type != "task_unavailable" || msg.Reason != taskUnavailableHubNotReady {
		t.Fatalf("expected task_unavailable/%s, got type=%q reason=%q",
			taskUnavailableHubNotReady, msg.Type, msg.Reason)
	}
}

// TestNoWork_NoMatchingWork (#2546): with a status snapshot but an empty candidate
// set (no actionable issues), selectTask returns an explicit no_matching_work
// negative-ack rather than a silent nil.
func TestNoWork_NoMatchingWork(t *testing.T) {
	hub, s := covK2Hub(t)
	// A status snapshot with a repo but NO actionable issues → empty candidate set.
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{Name: "repo1", Full: "myorg/repo1", ActionableIssues: []any{}}},
	}
	s.statusMu.Unlock()

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "carol", ContributorID: "c-carol", TrustTier: "newcomer"},
		lastPong: time.Now(),
	}
	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("empty candidate set must send an explicit negative-ack, got nil (silent)")
	}
	if msg.Type != "task_unavailable" || msg.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("expected task_unavailable/%s, got type=%q reason=%q",
			taskUnavailableNoMatchingWork, msg.Type, msg.Reason)
	}
}

// TestFleetSnapshot_IdleReasonSurfaced (#2546): a connection that was last told
// there is no matching work surfaces that reason on its FleetClanker so the ops
// tab can show "idle: no_matching_work".
func TestFleetSnapshot_IdleReasonSurfaced(t *testing.T) {
	hub, _ := covK2Hub(t)
	conn := &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: "dave", ContributorID: "c-dave", TrustTier: "newcomer"},
		lastPong:       time.Now(),
		lastIdleReason: taskUnavailableNoMatchingWork,
	}
	hub.mu.Lock()
	hub.connections["conn-dave"] = conn
	hub.mu.Unlock()

	snap := hub.FleetSnapshot()
	if len(snap.Clankers) != 1 {
		t.Fatalf("expected 1 clanker, got %d", len(snap.Clankers))
	}
	if snap.Clankers[0].IdleReason != taskUnavailableNoMatchingWork {
		t.Fatalf("idle_reason = %q, want %q", snap.Clankers[0].IdleReason, taskUnavailableNoMatchingWork)
	}
	// An idle clanker contributes no work item.
	if len(snap.Work) != 0 {
		t.Fatalf("idle clanker should produce no work items, got %d", len(snap.Work))
	}
}

// --- #2539: the ops tab previews the assignment prompt + task metadata, and the
// preview NEVER carries the minted github_token ----------------------------------

const testGitHubToken = "ghs_SUPERSECRETTOKENVALUE_must_never_leak"

// TestPromptPreview_SurfacesPromptAndMetadata_NoToken (#2539): FleetSnapshot
// surfaces the exact prompt and task metadata (repo/number/title/labels) for an
// active task, and the minted github_token — even when present on the live
// task_assign — never appears anywhere in the preview payload.
func TestPromptPreview_SurfacesPromptAndMetadata_NoToken(t *testing.T) {
	hub, _ := covK2Hub(t)

	repo, number, title := "myorg/repo1", 101, "Actionable issue"
	prompt := buildTaskPrompt(repo, number, title)

	// The token must NEVER appear in the prompt the server builds.
	if strings.Contains(prompt, testGitHubToken) || strings.Contains(prompt, "ghs_") {
		t.Fatalf("buildTaskPrompt leaked a token-shaped string: %q", prompt)
	}

	conn := &ContributorConnection{
		profile: &ContributorProfile{GitHubUsername: "erin", ContributorID: "c-erin", TrustTier: "contributor"},
		currentTask: &WSTaskAssign{
			TaskID: "ct-x", Kind: "issue", Repo: repo, Number: number, Title: title,
		},
		currentPrompt: prompt,
		currentLabels: []string{"good first issue", "bug"},
		lastPong:      time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-erin"] = conn
	hub.mu.Unlock()

	snap := hub.FleetSnapshot()

	// Clanker-level preview.
	if len(snap.Clankers) != 1 {
		t.Fatalf("expected 1 clanker, got %d", len(snap.Clankers))
	}
	fc := snap.Clankers[0]
	if fc.PromptPreview != prompt {
		t.Fatalf("clanker prompt_preview mismatch:\n got %q\nwant %q", fc.PromptPreview, prompt)
	}
	if fc.CurrentTask == nil || fc.CurrentTask.Repo != repo || fc.CurrentTask.Number != number || fc.CurrentTask.Title != title {
		t.Fatalf("clanker current_task metadata missing/wrong: %+v", fc.CurrentTask)
	}

	// Work-level preview: prompt text + repo/number/title/labels present.
	if len(snap.Work) != 1 {
		t.Fatalf("expected 1 work item, got %d", len(snap.Work))
	}
	w := snap.Work[0]
	if w.PromptPreview != prompt {
		t.Fatalf("work prompt_preview mismatch:\n got %q\nwant %q", w.PromptPreview, prompt)
	}
	if !strings.Contains(w.PromptPreview, title) {
		t.Fatalf("prompt preview should reference the issue title %q, got %q", title, w.PromptPreview)
	}
	if w.Repo != repo || w.Number != number || w.Title != title {
		t.Fatalf("work metadata missing/wrong: repo=%q number=%d title=%q", w.Repo, w.Number, w.Title)
	}
	if len(w.Labels) != 2 || w.Labels[0] != "good first issue" {
		t.Fatalf("work labels missing/wrong: %+v", w.Labels)
	}

	// The critical assertion: NEITHER preview surface carries a github_token.
	// Assert on the whole marshaled snapshot so no field (existing or future) can
	// smuggle the credential to the read-only ops view.
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	blob := string(raw)
	if strings.Contains(blob, testGitHubToken) || strings.Contains(blob, "github_token") {
		t.Fatalf("prompt-preview snapshot leaked a github_token:\n%s", blob)
	}
}

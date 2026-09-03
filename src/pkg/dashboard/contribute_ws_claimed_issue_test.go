package dashboard

import (
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// These tests are the #3768 regression suite: projectbluefin/dakota#353
// accumulated duplicate PRs because the contribute queue kept offering an
// issue that an earlier contributor's open PR already fixed. selectTask must
// consult the duplicate-PR claim ledger (via Dependencies.IssueClaimed) and
// skip ANY claimed issue — including claims from EXTERNAL (non-hive) PR
// authors, which the agent-side FilterClaimedIssues deliberately ignores.

// claimedIssueStatus seeds a status with two admissible issues in a repo whose
// Full (owner/repo) differs from its bare Name, mirroring the projectbluefin
// incident shape.
func claimedIssueStatus(s *Server) {
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "dakota",
				Full: "projectbluefin/dakota",
				ActionableIssues: []any{
					map[string]any{
						"number": float64(353),
						"title":  "the issue everyone kept fixing",
						"url":    "https://github.com/projectbluefin/dakota/issues/353",
						"author": "someone",
					},
					map[string]any{
						"number": float64(360),
						"title":  "an unclaimed issue",
						"url":    "https://github.com/projectbluefin/dakota/issues/360",
						"author": "someone",
					},
				},
			},
		},
	}
	s.statusMu.Unlock()
}

func claimedIssueConn() *ContributorConnection {
	return &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "ct-a", ContributorID: "c-a", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
}

// externalClaim is the exact incident signal: an open PR from a HUMAN
// contributor (not the hive) claiming to fix the issue.
func externalClaim(repo string, number int) ghpkg.IssueClaim {
	return ghpkg.IssueClaim{
		Repo: repo, Issue: number,
		PRNumber: 900, PRRepo: repo,
		PRURL:          "https://github.com/projectbluefin/dakota/pull/900",
		PRAuthor:       "castrojo",
		ObservedAt:     time.Now(),
		ExternalAuthor: true,
	}
}

// TestClaimedIssue_SkippedAndNextOffered replays #3768: issue 353 is claimed
// by an external contributor's open PR, so selectTask must pass over it and
// offer the next admissible issue instead of double-dispatching 353.
func TestClaimedIssue_SkippedAndNextOffered(t *testing.T) {
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)
	s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
		if repo == "projectbluefin/dakota" && number == 353 {
			return externalClaim(repo, number), true
		}
		return ghpkg.IssueClaim{}, false
	}

	msg := hub.selectTask(claimedIssueConn())
	if msg == nil {
		t.Fatal("expected a message from selectTask")
	}
	if msg.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %q (reason %q)", msg.Type, msg.Reason)
	}
	if msg.Number != 360 {
		t.Fatalf("claimed issue was offered anyway: got #%d, want #360", msg.Number)
	}
}

// TestClaimedIssue_ReadyQueueMatchesSelectTaskAdmission pins the contributor
// queue contract: repository-global admission decisions must agree between the
// read-only ReadyQueue projection and live selectTask assignment. An open PR
// claims #353, so both paths must withhold it while leaving #360 offerable.
func TestClaimedIssue_ReadyQueueMatchesSelectTaskAdmission(t *testing.T) {
	for _, claimRepo := range []string{"projectbluefin/dakota", "dakota"} {
		t.Run(claimRepo, func(t *testing.T) {
			hub, s := covK2Hub(t)
			claimedIssueStatus(s)
			s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
				if repo == claimRepo && number == 353 {
					return externalClaim(repo, number), true
				}
				return ghpkg.IssueClaim{}, false
			}

			queue := hub.ReadyQueue(readyQueueDefaultLimit)
			if len(queue) != 1 || queue[0].Number != 360 {
				t.Fatalf("ReadyQueue should initially offer only unclaimed #360, got %+v", queue)
			}

			conn := claimedIssueConn()
			hub.mu.Lock()
			hub.connections["conn-a"] = conn
			hub.mu.Unlock()

			msg := hub.selectTask(conn)
			if msg == nil || msg.Type != "task_assign" || msg.Number != 360 {
				t.Fatalf("selectTask should skip claimed #353 and assign #360, got %+v", msg)
			}

			queue = hub.ReadyQueue(readyQueueDefaultLimit)
			if len(queue) != 0 {
				t.Fatalf("ReadyQueue should be empty while #360 is active, got %+v", queue)
			}
		})
	}
}

// TestClaimedIssue_AllClaimedYieldsNoMatchingWork: when every admissible issue
// is claimed by an open PR, the contributor gets an explicit no_matching_work
// negative-ack — never a duplicate assignment.
func TestClaimedIssue_AllClaimedYieldsNoMatchingWork(t *testing.T) {
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)
	s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
		return externalClaim(repo, number), true
	}

	msg := hub.selectTask(claimedIssueConn())
	if msg == nil {
		t.Fatal("expected a message from selectTask")
	}
	if msg.Type != "task_unavailable" || msg.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("expected task_unavailable/no_matching_work, got %q/%q", msg.Type, msg.Reason)
	}
}

// TestClaimedIssue_BareRepoNameKeyMatches: the claim ledger keys claims by the
// repo spelling used in the hive config, which may be the BARE name rather
// than owner/repo. The lookup must try FrontendRepo.Name too, or a
// bare-configured repo would silently miss every claim (the same key-mismatch
// class as #2644).
func TestClaimedIssue_BareRepoNameKeyMatches(t *testing.T) {
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)
	s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
		if repo == "dakota" && number == 353 { // bare config-form key only
			return externalClaim(repo, number), true
		}
		return ghpkg.IssueClaim{}, false
	}

	msg := hub.selectTask(claimedIssueConn())
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", msg)
	}
	if msg.Number != 360 {
		t.Fatalf("bare-name claim key was not honoured: got #%d, want #360", msg.Number)
	}
}

// TestClaimedIssue_NoHookLeavesSelectionUnchanged is the positive control: with
// no IssueClaimed hook wired (test hubs, hives booted without GitHub
// credentials), selection behaves exactly as before and offers the first
// eligible issue.
func TestClaimedIssue_NoHookLeavesSelectionUnchanged(t *testing.T) {
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)
	s.deps.IssueClaimed = nil

	msg := hub.selectTask(claimedIssueConn())
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", msg)
	}
	if msg.Number != 353 {
		t.Fatalf("expected first eligible issue #353 with no claim data, got #%d", msg.Number)
	}
}

// referenceClaim is the #3980 signal: an open PR that is demonstrably working
// the issue but deliberately did NOT claim to close it ("Refs #N"), because it
// only partially addresses it. The agent-side FilterClaimedIssues ignores these;
// the contribute queue must not.
func referenceClaim(repo string, number int) ghpkg.IssueClaim {
	return ghpkg.IssueClaim{
		Repo: repo, Issue: number,
		PRNumber: 3898, PRRepo: repo,
		PRURL:      "https://github.com/projectbluefin/dakota/pull/3898",
		PRAuthor:   "Danathar",
		ObservedAt: time.Now(),
		Reference:  true,
	}
}

// TestClaimedIssue_ReferenceClaimSkipped replays #3980: the contributor's own
// open PR referenced the issue without a closing keyword, so nothing suppressed
// it and the queue re-offered the same work every cooldown window, forever.
// A reference claim must withhold the issue and offer the next one instead.
func TestClaimedIssue_ReferenceClaimSkipped(t *testing.T) {
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)
	s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
		if repo == "projectbluefin/dakota" && number == 353 {
			return referenceClaim(repo, number), true
		}
		return ghpkg.IssueClaim{}, false
	}

	msg := hub.selectTask(claimedIssueConn())
	if msg == nil {
		t.Fatal("expected a message from selectTask")
	}
	if msg.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %q (reason %q)", msg.Type, msg.Reason)
	}
	if msg.Number != 360 {
		t.Fatalf("reference-claimed issue was re-offered: got #%d, want #360", msg.Number)
	}
}

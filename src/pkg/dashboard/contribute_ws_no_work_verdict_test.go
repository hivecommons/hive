package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// #3987 suite: the fourth member of the duplicate/re-offer family
// (#3768 → #3792 → #3980). The residual #2547 shape: an issue's shippable
// parts land via merged `Part of #N` PRs, the remainder is deliberately
// maintainer-gated, and — merged PRs not being open PRs — the next claim
// rescan finds NO claim of any kind, so the issue re-enters the contribute
// offer pool forever. #3980's escalating no-PR cooldown only BOUNDS that
// loop; the no_work_needed completion verdict eliminates it, with
// invalidation-by-issue-activity as the guard against ever suppressing
// genuinely reopened work.

// noWorkIssue builds the ActionableIssues map shape carrying an updated_at,
// the field isSuppressedByNoWorkVerdict compares against the verdict's
// RecordedAt. A zero updatedAt omits the field (an older status producer).
func noWorkIssue(number int, title string, updatedAt time.Time) map[string]any {
	issue := map[string]any{
		"number": float64(number),
		"title":  title,
		"url":    "https://github.com/myorg/repo1/issues/" + itoa(number),
		"author": "someone",
	}
	if !updatedAt.IsZero() {
		issue["updated_at"] = updatedAt.UTC().Format(time.RFC3339)
	}
	return issue
}

func noWorkConn() *ContributorConnection {
	return &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "ct-nw", ContributorID: "c-nw", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
}

// sweepCompletion ages the issue's completion entry past its short no-PR
// cooldown and lets isTaskInCooldown's sweep collect it — the exact state a
// live hub is in between re-offers, and the reason the pre-#3987 hub had
// nothing left to suppress the issue with.
func sweepCompletion(t *testing.T, hub *ContributeWSHub, repo string, number int) {
	t.Helper()
	key := noPRKey(repo, number)
	hub.completedMu.Lock()
	hub.completedTasks[key] = time.Now().Add(-(completedNoPRCooldownHours + 1) * time.Hour)
	hub.completedMu.Unlock()
	if hub.isTaskInCooldown(repo, number) {
		t.Fatalf("setup: %s should have left its short cooldown", key)
	}
}

// TestNoWorkVerdict_SuppressesReofferAfterCooldownSweep replays the #2547 loop
// from #3980's incident table: a no_work_needed completion, the short cooldown
// entry expiring and being swept, no open PR claim of any kind — and the issue
// must STILL not be re-offered, while unrelated work is.
func TestNoWorkVerdict_SuppressesReofferAfterCooldownSweep(t *testing.T) {
	hub, s := covK2Hub(t)
	const repo = "myorg/repo1"

	// The verdict was recorded now; the issue's last GitHub activity predates it.
	hub.markTaskCompletedVerdict(repo, 61, "", completionVerdictNoWorkNeeded, "ct-nw", "maintainer_gated")
	sweepCompletion(t, hub, repo, 61)

	staleActivity := time.Now().Add(-24 * time.Hour)
	setStatusIssues(s,
		noWorkIssue(61, "maintainer-gated remainder", staleActivity),
		noWorkIssue(62, "unrelated admissible issue", staleActivity),
	)

	msg := hub.selectTask(noWorkConn())
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected task_assign for the unrelated issue, got %+v", msg)
	}
	if msg.Number != 62 {
		t.Fatalf("no_work_needed issue was re-offered: got #%d, want #62", msg.Number)
	}

	// With ONLY the verdict-suppressed issue present, the contributor gets an
	// explicit no_matching_work — never the same dead-end issue again.
	conn := noWorkConn()
	setStatusIssues(s, noWorkIssue(61, "maintainer-gated remainder", staleActivity))
	msg2 := hub.selectTask(conn)
	if msg2 == nil || msg2.Type != "task_unavailable" || msg2.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("expected task_unavailable/%s, got %+v", taskUnavailableNoMatchingWork, msg2)
	}
}

// TestNoWorkVerdict_IdleCompletionStillReoffered is the positive control and
// the old-relay compatibility pin: a completion WITHOUT the verdict field (the
// plain markTaskCompleted path every pre-#3987 relay drives) books only the
// escalating short cooldown, and once that elapses the issue IS offered again
// — byte-for-byte today's behavior.
func TestNoWorkVerdict_IdleCompletionStillReoffered(t *testing.T) {
	hub, s := covK2Hub(t)
	const repo = "myorg/repo1"

	hub.markTaskCompleted(repo, 63, "")
	sweepCompletion(t, hub, repo, 63)

	setStatusIssues(s, noWorkIssue(63, "idle completion, no verdict", time.Now().Add(-24*time.Hour)))
	msg := hub.selectTask(noWorkConn())
	if msg == nil || msg.Type != "task_assign" || msg.Number != 63 {
		t.Fatalf("idle completion must not earn the long verdict suppression: got %+v", msg)
	}
}

// TestNoWorkVerdict_IssueActivityVoidsVerdict is the invalidation positive
// control — the one failure mode this feature must not have is suppressing
// genuinely reopened work. Issue activity NEWER than the verdict (the
// maintainer answered, commits/comments landed) voids it: the issue is offered
// again and the ledger entry is gone.
func TestNoWorkVerdict_IssueActivityVoidsVerdict(t *testing.T) {
	hub, s := covK2Hub(t)
	const repo = "myorg/repo1"

	hub.markTaskCompletedVerdict(repo, 64, "", completionVerdictNoWorkNeeded, "ct-nw", "maintainer_gated")
	sweepCompletion(t, hub, repo, 64)

	// New activity: updated_at AFTER the verdict's RecordedAt.
	setStatusIssues(s, noWorkIssue(64, "maintainer answered", time.Now().Add(time.Minute)))
	msg := hub.selectTask(noWorkConn())
	if msg == nil || msg.Type != "task_assign" || msg.Number != 64 {
		t.Fatalf("issue activity must void the verdict and re-admit the issue: got %+v", msg)
	}

	hub.completedMu.Lock()
	_, still := hub.noWorkVerdicts[noPRKey(repo, 64)]
	hub.completedMu.Unlock()
	if still {
		t.Fatal("voided verdict must be removed from the ledger, not just bypassed")
	}
}

// TestNoWorkVerdict_UnknownUpdatedAtFailsOpen: when the status producer does
// not carry updated_at (older snapshot), we cannot prove the issue has been
// quiet since the verdict, so the check fails OPEN — the issue is offered and
// the record is kept for a later snapshot that does carry the field.
func TestNoWorkVerdict_UnknownUpdatedAtFailsOpen(t *testing.T) {
	hub, s := covK2Hub(t)
	const repo = "myorg/repo1"

	hub.markTaskCompletedVerdict(repo, 65, "", completionVerdictNoWorkNeeded, "ct-nw", "")
	sweepCompletion(t, hub, repo, 65)

	setStatusIssues(s, noWorkIssue(65, "no updated_at available", time.Time{}))
	msg := hub.selectTask(noWorkConn())
	if msg == nil || msg.Type != "task_assign" || msg.Number != 65 {
		t.Fatalf("unknown issue activity must fail open (offer), got %+v", msg)
	}

	hub.completedMu.Lock()
	_, still := hub.noWorkVerdicts[noPRKey(repo, 65)]
	hub.completedMu.Unlock()
	if !still {
		t.Fatal("fail-open on unknown activity must KEEP the verdict record")
	}
}

// TestNoWorkVerdict_ExpiryLiftsSuppression: a verdict older than its
// suppression window (the with-PR cooldown snapshotted at record time) no
// longer suppresses, and the expired entry is swept from the ledger.
func TestNoWorkVerdict_ExpiryLiftsSuppression(t *testing.T) {
	hub, s := covK2Hub(t)
	const repo = "myorg/repo1"
	key := noPRKey(repo, 66)

	hub.markTaskCompletedVerdict(repo, 66, "", completionVerdictNoWorkNeeded, "ct-nw", "already_covered")
	sweepCompletion(t, hub, repo, 66)

	hub.completedMu.Lock()
	rec := hub.noWorkVerdicts[key]
	rec.RecordedAt = time.Now().Add(-rec.suppressWindow() - time.Hour)
	hub.noWorkVerdicts[key] = rec
	hub.completedMu.Unlock()

	setStatusIssues(s, noWorkIssue(66, "verdict expired", time.Now().Add(-rec.suppressWindow()-2*time.Hour)))
	msg := hub.selectTask(noWorkConn())
	if msg == nil || msg.Type != "task_assign" || msg.Number != 66 {
		t.Fatalf("expired verdict must not suppress, got %+v", msg)
	}

	hub.completedMu.Lock()
	_, still := hub.noWorkVerdicts[key]
	hub.completedMu.Unlock()
	if still {
		t.Fatal("expired verdict must be swept from the ledger")
	}
}

// TestNoWorkVerdict_ClearedByShippedCompletion: a later completion that ships
// a verified PR voids any standing verdict — real work landed, so "nothing is
// shippable" is no longer true.
func TestNoWorkVerdict_ClearedByShippedCompletion(t *testing.T) {
	hub, _ := covK2Hub(t)
	const repo = "myorg/repo1"
	key := noPRKey(repo, 67)

	hub.markTaskCompletedVerdict(repo, 67, "", completionVerdictNoWorkNeeded, "ct-nw", "")
	hub.completedMu.Lock()
	_, have := hub.noWorkVerdicts[key]
	hub.completedMu.Unlock()
	if !have {
		t.Fatal("setup: verdict should be recorded")
	}

	hub.markTaskCompleted(repo, 67, "https://github.com/myorg/repo1/pull/9")
	hub.completedMu.Lock()
	_, still := hub.noWorkVerdicts[key]
	hub.completedMu.Unlock()
	if still {
		t.Fatal("a shipped completion must void the standing no-work verdict")
	}
}

// TestNormalizeCompletionVerdict pins the trust-order of the verdict field: a
// server-VERIFIED PR always wins; without one, only an explicit (case- and
// whitespace-insensitive) no_work_needed is honoured; everything else —
// including the absent field every pre-#3987 relay sends and a self-claimed
// "shipped" whose PR failed verification — normalizes to idle.
func TestNormalizeCompletionVerdict(t *testing.T) {
	cases := []struct {
		reported, verifiedPR, want string
	}{
		{"", "", completionVerdictIdle},
		{"no_work_needed", "", completionVerdictNoWorkNeeded},
		{"  No_Work_Needed  ", "", completionVerdictNoWorkNeeded},
		{"shipped", "", completionVerdictIdle},                                        // claimed shipped, nothing verified
		{"garbage", "", completionVerdictIdle},                                        // unknown value
		{"no_work_needed", "https://github.com/o/r/pull/1", completionVerdictShipped}, // verified PR wins
		{"", "https://github.com/o/r/pull/1", completionVerdictShipped},
	}
	for _, c := range cases {
		if got := normalizeCompletionVerdict(c.reported, c.verifiedPR); got != c.want {
			t.Errorf("normalizeCompletionVerdict(%q, %q) = %q, want %q", c.reported, c.verifiedPR, got, c.want)
		}
	}
}

// TestNoWorkVerdict_PersistsAcrossRestart: the verdict must survive a hub pod
// restart via the PVC-backed ledger — otherwise every deploy would re-admit
// every parked dead-end issue. An entry past its own suppression window is NOT
// resurrected.
func TestNoWorkVerdict_PersistsAcrossRestart(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	const repo = "myorg/repo1"

	hub1, _ := covK2Hub(t)
	hub1.markTaskCompletedVerdict(repo, 68, "", completionVerdictNoWorkNeeded, "ct-nw", "maintainer_gated")

	// Stale entry: written to disk aged past its window, must be dropped on load.
	hub1.completedMu.Lock()
	hub1.noWorkVerdicts[noPRKey(repo, 69)] = noWorkVerdictRecord{
		RecordedAt:    time.Now().Add(-completedTaskCooldownHours*time.Hour - time.Hour),
		SuppressHours: completedTaskCooldownHours,
	}
	hub1.completedMu.Unlock()
	// saveNoWorkVerdicts itself skips expired entries; write the live one.
	hub1.saveNoWorkVerdicts()

	// Simulated restart: a fresh hub loads the ledger from disk.
	hub2, _ := covK2Hub(t)
	hub2.completedMu.Lock()
	rec, live := hub2.noWorkVerdicts[noPRKey(repo, 68)]
	_, stale := hub2.noWorkVerdicts[noPRKey(repo, 69)]
	hub2.completedMu.Unlock()
	if !live {
		t.Fatal("verdict must survive a hub restart via the on-disk ledger")
	}
	if rec.Reporter != "ct-nw" || rec.Reason != "maintainer_gated" {
		t.Fatalf("audit fields must round-trip, got %+v", rec)
	}
	if stale {
		t.Fatal("an entry past its suppression window must not be resurrected on load")
	}
	if !hub2.isSuppressedByNoWorkVerdict(repo, 68, time.Now().Add(-24*time.Hour)) {
		t.Fatal("restored verdict must still suppress the offer")
	}
}

// TestNoWorkVerdict_OfferSurfacesOnly_StatusUntouched pins the trust doctrine:
// the contributor-reported verdict may ONLY suppress offers. The two offer
// surfaces (selectTask, ReadyQueue) must agree — the same contract the claim
// ledger admission pins — while the status payload's ActionableIssues, the set
// the hive AGENT pipeline reads, is left completely untouched.
func TestNoWorkVerdict_OfferSurfacesOnly_StatusUntouched(t *testing.T) {
	hub, s := covK2Hub(t)
	const repo = "myorg/repo1"

	hub.markTaskCompletedVerdict(repo, 70, "", completionVerdictNoWorkNeeded, "ct-nw", "maintainer_gated")
	sweepCompletion(t, hub, repo, 70)

	stale := time.Now().Add(-24 * time.Hour)
	setStatusIssues(s,
		noWorkIssue(70, "verdict-parked issue", stale),
		noWorkIssue(71, "free issue", stale),
	)

	queue := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(queue) != 1 || queue[0].Number != 71 {
		t.Fatalf("ReadyQueue must exclude the verdict-parked issue and keep #71, got %+v", queue)
	}

	msg := hub.selectTask(noWorkConn())
	if msg == nil || msg.Type != "task_assign" || msg.Number != 71 {
		t.Fatalf("selectTask must agree with ReadyQueue and offer only #71, got %+v", msg)
	}

	// The agent pipeline's source of truth is NOT filtered: both issues are
	// still present in the status payload. The verdict never mutates status,
	// never closes anything on GitHub, and has no reader outside the two offer
	// surfaces above.
	s.statusMu.RLock()
	issues := s.status.Repos[0].ActionableIssues
	s.statusMu.RUnlock()
	if len(issues) != 2 {
		t.Fatalf("status ActionableIssues must be untouched by the verdict, got %d entries", len(issues))
	}
}

// TestNoWorkVerdict_NeverGrantsTrustCredit drives the REAL WebSocket
// task_complete handler end to end: a completion carrying verdict
// no_work_needed (and no PR) records the verdict with the reporting
// contributor, but earns zero TasksWithPR, no promotion — the verdict is
// weak, self-reported evidence and must never feed the PR-gated trust path.
func TestNoWorkVerdict_NeverGrantsTrustCredit(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"verdict-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register response: %v", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn) // auth_ok

	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					noWorkIssue(80, "maintainer-gated issue", time.Now().Add(-24*time.Hour)),
				},
			},
		},
	}
	s.statusMu.Unlock()

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: assign.TaskID, Result: "no_work",
		Verdict: "no_work_needed", VerdictReason: "maintainer_gated",
	})
	time.Sleep(50 * time.Millisecond)

	p := findContributor(reg["contributor_id"])
	if p == nil {
		t.Fatal("contributor not found")
	}
	if p.TasksCompleted != 1 {
		t.Fatalf("completion itself must still be recorded, got TasksCompleted=%d", p.TasksCompleted)
	}
	if p.TasksWithPR != 0 {
		t.Fatalf("no_work_needed must NEVER count as a PR-shipping task, got TasksWithPR=%d", p.TasksWithPR)
	}
	if p.TrustTier != "newcomer" {
		t.Fatalf("no_work_needed must never promote, got tier %q", p.TrustTier)
	}

	hub := s.contributeHub
	hub.completedMu.Lock()
	rec, ok := hub.noWorkVerdicts["myorg/repo1#80"]
	hub.completedMu.Unlock()
	if !ok {
		t.Fatal("handler must record the no-work verdict for the completed issue")
	}
	if rec.Reporter != "verdict-user" || rec.Reason != "maintainer_gated" {
		t.Fatalf("verdict audit fields wrong: %+v", rec)
	}

	// End-to-end replay of the loop's tail: the issue must not be offered to
	// the next ready contributor even though its short cooldown entry is aged
	// out and swept.
	sweepCompletion(t, hub, "myorg/repo1", 80)
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	next := readMsg(t, conn)
	if next.Type != "task_unavailable" || next.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("verdict-parked issue must not be re-offered, got %+v", next)
	}
}

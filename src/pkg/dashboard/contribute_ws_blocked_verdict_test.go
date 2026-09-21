package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/internal/testutil"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#7924: the blocked verdict.
//
// projectbluefin/utah#100 ended in a correct no_work_needed whose reason was
// "nothing here can change until utah-packages' factory publishes an image
// with those packages" — a blocked-on-another-repo state. The hub booked it as
// an ordinary no-PR completion: the escalating 4h ladder, the issue back in
// the queue on the next rung to re-run the same ten minutes of research and
// re-find "still no factory build". A verdict that SAYS blocked is now held for
// the full with-PR cooldown from the first completion, its ledger row carries
// the marker, and the relay — with the task credential — applies the repo's
// `blocked` label so the existing admission gate holds it past the cooldown
// until a human clears it. The hub itself still labels nothing.

const blockedReason = "utah-packages has recipes on main but no factory build has published since they merged"

// TestBlockedVerdict_BooksFullCooldownNotTheLadder: the first blocked
// completion of an issue gets the with-PR cooldown outright, where the first
// no_work_needed gets the 4h base rung. The no-PR streak — the ladder's
// escalation state — is left untouched.
func TestBlockedVerdict_BooksFullCooldownNotTheLadder(t *testing.T) {
	hub, _ := covK2Hub(t)
	const repo = "myorg/repo1"

	hub.markTaskCompletedVerdict(repo, 100, "", completionVerdictBlocked, "ct-bl", blockedReason)
	if got, want := recordedCooldown(t, hub, noPRKey(repo, 100)), hub.configuredWithPRCooldown(); got != want {
		t.Fatalf("blocked verdict cooldown = %v, want the full with-PR cooldown %v", got, want)
	}
	hub.completedMu.Lock()
	rec, ok := hub.noWorkVerdicts[noPRKey(repo, 100)]
	_, streak := hub.noPRStreaks[noPRKey(repo, 100)]
	hub.completedMu.Unlock()
	if !ok || !rec.Blocked {
		t.Fatalf("ledger row must carry the blocked marker, got ok=%v rec=%+v", ok, rec)
	}
	if rec.Reporter != "ct-bl" || rec.Reason != blockedReason {
		t.Fatalf("audit fields must round-trip on a blocked row: %+v", rec)
	}
	if streak {
		t.Fatal("a blocked verdict is already at the ceiling; it must not advance the no-PR streak")
	}

	// Control: the sibling verdict on a fresh issue still starts on the ladder.
	hub.markTaskCompletedVerdict(repo, 101, "", completionVerdictNoWorkNeeded, "ct-bl", "maintainer_gated")
	if got, want := recordedCooldown(t, hub, noPRKey(repo, 101)), completedNoPRCooldownHours*time.Hour; got != want {
		t.Fatalf("no_work_needed cooldown = %v, want the 4h base rung %v (unchanged)", got, want)
	}
	hub.completedMu.Lock()
	rec = hub.noWorkVerdicts[noPRKey(repo, 101)]
	hub.completedMu.Unlock()
	if rec.Blocked {
		t.Fatal("a plain no_work_needed row must not carry the blocked marker")
	}
}

// TestBlockedVerdict_HeldPastTheLadderSweep replays utah#100's tail: the 4h
// rung a no_work_needed would have booked has elapsed and been swept, and the
// blocked issue must STILL be in cooldown — that is the whole difference.
func TestBlockedVerdict_HeldPastTheLadderSweep(t *testing.T) {
	hub, s := covK2Hub(t)
	const repo = "myorg/repo1"
	key := noPRKey(repo, 102)

	hub.markTaskCompletedVerdict(repo, 102, "", completionVerdictBlocked, "ct-bl", blockedReason)
	hub.completedMu.Lock()
	hub.completedTasks[key] = time.Now().Add(-(completedNoPRCooldownHours + 1) * time.Hour)
	hub.completedMu.Unlock()
	if !hub.isTaskInCooldown(repo, 102) {
		t.Fatal("a blocked issue must still be in cooldown after the 4h rung would have lapsed")
	}

	// Even with issue activity newer than the verdict — which VOIDS a
	// no_work_needed's ledger row — the cooldown is what holds it, so a
	// drive-by comment cannot re-trigger the research cycle.
	setStatusIssues(s,
		noWorkIssue(102, "blocked on the factory", time.Now().Add(time.Minute)),
		noWorkIssue(103, "unrelated admissible issue", time.Now().Add(-24*time.Hour)),
	)
	msg := hub.selectTask(noWorkConn())
	if msg == nil || msg.Type != "task_assign" || msg.Number != 103 {
		t.Fatalf("blocked issue was re-offered inside its full cooldown: got %+v, want #103", msg)
	}
}

// TestBlockedVerdict_IsAffirmativeEvidence: like no_work_needed, blocked is a
// conclusion the agent reached — never the evidence-less shape #6723/#7862
// bound to the flat cooldown and zero task credit.
func TestBlockedVerdict_IsAffirmativeEvidence(t *testing.T) {
	for _, signal := range []string{completionSignalVerdict, completionSignalChromeIdle, ""} {
		if isEvidenceLessCompletion("", completionVerdictBlocked, signal) {
			t.Errorf("blocked with signal %q must not be evidence-less", signal)
		}
	}
	if !isNoWorkFamilyVerdict(completionVerdictBlocked) || !isNoWorkFamilyVerdict(completionVerdictNoWorkNeeded) ||
		isNoWorkFamilyVerdict(completionVerdictIdle) || isNoWorkFamilyVerdict(completionVerdictShipped) {
		t.Fatal("the no-work family is exactly no_work_needed and blocked")
	}
}

// TestBlockedVerdict_PersistsAcrossRestart: the marker survives the PVC-backed
// ledger round trip, so a hub bounce still shows an operator what the issue is
// waiting on.
func TestBlockedVerdict_PersistsAcrossRestart(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	const repo = "myorg/repo1"

	hub1, _ := covK2Hub(t)
	hub1.markTaskCompletedVerdict(repo, 104, "", completionVerdictBlocked, "ct-bl", blockedReason)

	hub2, _ := covK2Hub(t)
	hub2.completedMu.Lock()
	rec, live := hub2.noWorkVerdicts[noPRKey(repo, 104)]
	hub2.completedMu.Unlock()
	if !live || !rec.Blocked || rec.Reason != blockedReason {
		t.Fatalf("blocked row must round-trip through the on-disk ledger, got live=%v rec=%+v", live, rec)
	}
}

// TestBlockedVerdict_WireShape drives the REAL task_complete handler with the
// shape the relay sends — verdict no_work_needed plus the verdict_blocked
// marker, so a hub older than #7924 still reads the no_work_needed it already
// books — and checks it is booked as blocked here: full cooldown, marked
// ledger row, a completed task but no PR credit and no promotion, and the
// blocked reason NOT fed to the #7871 settle path (it names what the issue is
// waiting on, not what settled it).
func TestBlockedVerdict_WireShape(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"blocked-user"}`
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
	authOK := readMsg(t, conn)
	advertised := false
	for _, c := range authOK.ServerCapabilities {
		if c == capBlockedVerdict {
			advertised = true
		}
	}
	if !advertised {
		t.Fatalf("auth_ok must advertise %q, got %v", capBlockedVerdict, authOK.ServerCapabilities)
	}

	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					noWorkIssue(105, "nautilus missing from image", time.Now().Add(-24*time.Hour)),
				},
			},
		},
	}
	s.statusMu.Unlock()

	// The settle path must never see a blocked reason: with a claim ledger
	// wired (the precondition settleIssueFromVerdict checks first), a verifier
	// that is called at all is the failure. The reason below cites a merged
	// PR by owner/repo#N precisely so the #7871 parser WOULD find something.
	hub := s.contributeHub
	if s.deps == nil {
		s.deps = &Dependencies{}
	}
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	var verifierCalls int32
	hub.settleVerifier = func(_ context.Context, _ string, _ int, ref ghpkg.SettlingRef, _ time.Time) (ghpkg.SettleVerification, error) {
		atomic.AddInt32(&verifierCalls, 1)
		return ghpkg.SettleVerification{Settled: true, Claim: ghpkg.IssueClaim{PRNumber: ref.Number, MergedPR: true}}, nil
	}

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: assign.TaskID, Result: "completed",
		CompletionSignal: completionSignalVerdict,
		Verdict:          "no_work_needed", VerdictBlocked: true,
		VerdictReason: "projectbluefin/utah-packages#41 merged the recipes but no factory image carries them yet",
	})
	// The completion is booked on the hub's goroutine after the read; wait on
	// the observable result rather than a fixed sleep (sleep ratchet).
	testutil.Eventually(t, 2*time.Second, func() bool {
		p := findContributor(reg["contributor_id"])
		return p != nil && p.TasksCompleted == 1
	}, "a blocked verdict is a real conclusion: TasksCompleted should reach 1")

	p := findContributor(reg["contributor_id"])
	if p == nil {
		t.Fatal("contributor not found")
	}
	if p.TasksWithPR != 0 || p.TrustTier != "newcomer" {
		t.Fatalf("blocked must never count as a PR or promote: TasksWithPR=%d tier=%q", p.TasksWithPR, p.TrustTier)
	}

	if n := atomic.LoadInt32(&verifierCalls); n != 0 {
		t.Fatalf("a blocked reason names what the issue is waiting on, not what settled it — it must not reach the settle verifier (called %d times)", n)
	}
	if len(fx.recorded) != 0 {
		t.Fatalf("no claim may be recorded from a blocked verdict, got %+v", fx.recorded)
	}

	key := noPRKey("myorg/repo1", 105)
	if got, want := recordedCooldown(t, hub, key), hub.configuredWithPRCooldown(); got != want {
		t.Fatalf("wire-shape blocked verdict booked %v, want the full with-PR cooldown %v", got, want)
	}
	hub.completedMu.Lock()
	rec, ok := hub.noWorkVerdicts[key]
	hub.completedMu.Unlock()
	if !ok || !rec.Blocked || rec.Reporter != "blocked-user" {
		t.Fatalf("ledger row must be the blocked marker for the reporting contributor, got ok=%v rec=%+v", ok, rec)
	}

	// The next ready contributor is not handed the same dead end.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	next := readMsg(t, conn)
	if next.Type != "task_unavailable" || next.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("blocked issue must not be re-offered, got %+v", next)
	}
}

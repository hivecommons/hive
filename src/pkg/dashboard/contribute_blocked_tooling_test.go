package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// hivecommons/hive#11775 / #11776: a contributor's agent could not push to its
// fork (task token without the `workflow` scope), printed a blocked verdict
// for it, and the relay labelled both issues `blocked`. The hub booked the
// full with-PR cooldown and the fleet-wide workflow gate parked the issues
// with no PR and nothing coming to release them. The hub now reclassifies a
// blocked verdict whose reason is the contributor's own tooling as idle and
// lifts the label.

const toolingBlockedReason = "the push to my fork is rejected because the token lacks workflow scope, and syncing the fork's v5 is blocked by the gh wrapper; the commit is ready in"

func TestBlockedVerdictToolingFailure_Classifier(t *testing.T) {
	tooling := []string{
		toolingBlockedReason,
		"push to my fork was rejected: refusing to allow a GitHub App to create or update workflow",
		"Fix is ready locally but the push to my fork is rejected: the token lacks the `workflow` scope",
		"git push failed with HTTP 403",
		"permission denied when pushing the branch",
		"cannot push: authentication failed for https://github.com/x/y",
		"the fork's v6 is behind upstream and the gh wrapper refuses to sync it",
		"GH_TOKEN does not have the repo scope needed to open the PR",
	}
	for _, reason := range tooling {
		if !isBlockedVerdictToolingFailure(reason) {
			t.Errorf("reason must classify as a contributor tooling failure: %q", reason)
		}
		if v, ok := reclassifyToolingBlockedVerdict(completionVerdictBlocked, reason); !ok || v != completionVerdictIdle {
			t.Errorf("blocked + tooling reason must demote to idle, got (%q, %v) for %q", v, ok, reason)
		}
	}
	external := []string{
		blockedReason,
		"waiting on projectbluefin/utah-packages to publish a factory image with the recipes",
		"the upstream library has not released the API this needs",
		"depends on hivecommons/hub#42 landing first",
		"",
	}
	for _, reason := range external {
		if isBlockedVerdictToolingFailure(reason) {
			t.Errorf("genuine external-dependency reason must NOT classify as tooling: %q", reason)
		}
		if v, ok := reclassifyToolingBlockedVerdict(completionVerdictBlocked, reason); ok || v != completionVerdictBlocked {
			t.Errorf("blocked + external reason must pass through, got (%q, %v) for %q", v, ok, reason)
		}
	}
	// Only a blocked verdict is subject to reclassification: a no_work_needed
	// or shipped completion with tooling words in its reason is left alone.
	for _, verdict := range []string{completionVerdictNoWorkNeeded, completionVerdictShipped, completionVerdictIdle, completionVerdictNeedsDecision} {
		if v, ok := reclassifyToolingBlockedVerdict(verdict, toolingBlockedReason); ok || v != verdict {
			t.Errorf("verdict %q must pass through unchanged, got (%q, %v)", verdict, v, ok)
		}
	}
}

func TestBlockedVerdictToolingFailure_ReleaseCommentIsMarkedAndBounded(t *testing.T) {
	long := strings.Repeat("§", noWorkReasonMaxLen*2)
	body := toolingBlockedReleaseComment("mrbobbytables", long)
	if !strings.HasPrefix(body, toolingBlockedReleaseMarker) {
		t.Fatalf("release comment must start with the marker, got %q", body[:80])
	}
	if !strings.Contains(body, "@mrbobbytables") || !strings.Contains(body, "`"+blockedWorkflowLabel+"`") {
		t.Fatalf("release comment must name the reporter and the label: %q", body)
	}
	if n := strings.Count(body, "§"); n == 0 || n > noWorkReasonMaxLen {
		t.Fatalf("release comment must carry the reason capped at %d bytes, got %d marks", noWorkReasonMaxLen, n)
	}
	anon := toolingBlockedReleaseComment("", "")
	if strings.Contains(anon, "@") || strings.Contains(anon, "Reported reason") {
		t.Fatalf("anonymous, reasonless comment must omit the handle and the reason tail: %q", anon)
	}
}

// toolingReleaseFixture records what the hub asked the forge to do.
type toolingReleaseFixture struct {
	mu       sync.Mutex
	calls    []string
	err      error
	released chan struct{}
}

func newToolingReleaseFixture() *toolingReleaseFixture {
	return &toolingReleaseFixture{released: make(chan struct{}, 8)}
}

func (f *toolingReleaseFixture) release(_ context.Context, repo string, number int, reporter, reason string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%s#%d by %s: %s", repo, number, reporter, reason))
	f.mu.Unlock()
	f.released <- struct{}{}
	return f.err
}

func TestBlockedVerdictToolingFailure_ReleaseSkipsNonIssueTasks(t *testing.T) {
	hub, _ := covK2Hub(t)
	fx := newToolingReleaseFixture()
	hub.toolingBlockedReleaser = fx.release

	hub.releaseToolingBlockedIssue(nil, "u", "r")
	hub.releaseToolingBlockedIssue(&WSTaskAssign{Kind: "pr", Repo: "o/r", Number: 5}, "u", "r")
	hub.releaseToolingBlockedIssue(&WSTaskAssign{Kind: "issue", Repo: "", Number: 5}, "u", "r")
	hub.releaseToolingBlockedIssue(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 0}, "u", "r")
	hub.releaseToolingBlockedIssue(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 5, ExternalID: "LIN-1"}, "u", "r")
	if len(fx.calls) != 0 {
		t.Fatalf("only GitHub-backed issue tasks may be released, got %v", fx.calls)
	}

	hub.releaseToolingBlockedIssue(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 5}, "u", "r")
	if len(fx.calls) != 1 || fx.calls[0] != "o/r#5 by u: r" {
		t.Fatalf("issue task must be released through the seam, got %v", fx.calls)
	}

	// An API error is logged, not fatal: the verdict is already booked idle.
	fx.err = errors.New("boom")
	hub.releaseToolingBlockedIssue(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 6}, "u", "r")
	if len(fx.calls) != 2 {
		t.Fatalf("release must still be attempted when the forge errors, got %v", fx.calls)
	}
}

// TestBlockedVerdictToolingFailure_WireShape drives the real task_complete
// handler with the exact completion the relay sent for #11776 and checks the
// hub booked an idle completion (no blocked ledger row, short cooldown),
// released the label through the seam, and that the issue is offered again
// once its short cooldown lapses.
func TestBlockedVerdictToolingFailure_WireShape(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"mrbobbytables"}`
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
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "copilot"})
	readMsg(t, conn) // auth_ok

	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name: "hive",
			Full: "hivecommons/hive",
			ActionableIssues: []any{
				noWorkIssue(11776, "[v6] Re-running kubectl apply -k wipes the keys in hive-secrets", time.Now().Add(-24*time.Hour)),
			},
		}},
	}
	s.statusMu.Unlock()

	hub := s.contributeHub
	fx := newToolingReleaseFixture()
	hub.toolingBlockedReleaser = fx.release

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 11776 {
		t.Fatalf("expected task_assign for #11776, got %+v", assign)
	}
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: assign.TaskID, Result: "completed",
		CompletionSignal: completionSignalVerdict,
		Verdict:          "no_work_needed", VerdictBlocked: true,
		VerdictReason: toolingBlockedReason,
	})
	// The booking is synchronous in the read loop; the reply to a later
	// `ready` is the happens-before edge (see TestBlockedVerdict_WireShape).
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	next := readMsg(t, conn)
	if next.Type != "task_unavailable" {
		t.Fatalf("the issue is inside its short cooldown right after completion, got %+v", next)
	}

	select {
	case <-fx.released:
	case <-time.After(5 * time.Second):
		t.Fatal("the hub must release the blocked label through the seam")
	}
	fx.mu.Lock()
	calls := append([]string(nil), fx.calls...)
	fx.mu.Unlock()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "hivecommons/hive#11776 by mrbobbytables: ") {
		t.Fatalf("release must target the completed issue for the reporting contributor, got %v", calls)
	}

	key := noPRKey("hivecommons/hive", 11776)
	hub.completedMu.Lock()
	_, blockedRow := hub.noWorkVerdicts[key]
	hub.completedMu.Unlock()
	if blockedRow {
		t.Fatal("a tooling-failure verdict must not leave a blocked/no-work ledger row")
	}
	if got, full := recordedCooldown(t, hub, key), hub.configuredWithPRCooldown(); got >= full {
		t.Fatalf("tooling-failure verdict booked %v, want a short re-offer cooldown below the full %v", got, full)
	}

	// Once that short cooldown lapses the issue is offered again — the whole
	// point: another contributor with working tooling can take it.
	hub.completedMu.Lock()
	hub.completedTasks[key] = time.Now().Add(-(hub.completedTaskCooldown[key] + time.Minute))
	hub.completedMu.Unlock()
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 4})
	again := readMsg(t, conn)
	if again.Type != "task_assign" || again.Number != 11776 {
		t.Fatalf("issue must be re-offered after the short cooldown, got %+v", again)
	}
}

// TestBlockedVerdictToolingFailure_GenuineBlockedUntouched: the control — a
// real external-dependency reason over the same wire still books blocked and
// never reaches the release seam.
func TestBlockedVerdictToolingFailure_GenuineBlockedUntouched(t *testing.T) {
	hub, _ := covK2Hub(t)
	fx := newToolingReleaseFixture()
	hub.toolingBlockedReleaser = fx.release

	verdict := normalizeCompletionVerdict("no_work_needed", true, false, "")
	if v, ok := reclassifyToolingBlockedVerdict(verdict, blockedReason); ok || v != completionVerdictBlocked {
		t.Fatalf("genuine blocked must survive reclassification, got (%q, %v)", v, ok)
	}
	if len(fx.calls) != 0 {
		t.Fatalf("no release may happen for a genuine blocked verdict, got %v", fx.calls)
	}
}

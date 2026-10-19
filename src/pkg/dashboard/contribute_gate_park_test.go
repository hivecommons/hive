package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/decisionpolicy"
)

// hivecommons/hive#13059: five contributor parks on one target repo
// in one day were agents obeying the target repo's AGENTS.md "offer a spec
// and wait for acceptance" rule, or its "stop on a missing registered
// checkout" rule, on issues the hive had already accepted and dispatched.
// The hub reclassifies such a needs-decision verdict as idle once per issue,
// lifts the decision label, and leaves a marked comment; a second gate park
// on the same issue stays parked.

const gateParkReason = "Implementation is paused at the repository's explicit AGENTS.md confirmation gate: its Spec-Worthy Discussion Recognition rule requires offering a spec and waiting for acceptance before implementing. `go run . repo list` also stops with `repo_location_missing` for the registered docs location /home/dev/workspace/acme/widgets-docs/.widgets."

const genuineDecisionReason = "whether the public-facing CLI flag should be renamed is a breaking change the maintainer must decide"

func TestGateParkVerdict_Classifier(t *testing.T) {
	hub, _ := covK2Hub(t)
	gates := []string{
		gateParkReason,
		"waiting on spec-workflow decision per AGENTS.md",
		"should I capture this as a spec or proceed?",
		"CLAUDE.md says to confirm scope before implementing",
		"registered sibling checkout missing under the workspace root",
	}
	for _, reason := range gates {
		if !isNeedsDecisionRepoLocalGate(reason) {
			t.Errorf("reason must classify as a repo-local gate: %q", reason)
		}
		if v, ok := hub.reclassifyGateParkVerdict("k", completionVerdictNeedsDecision, reason); !ok || v != completionVerdictIdle {
			t.Errorf("needs_decision + gate reason must demote to idle, got (%q, %v) for %q", v, ok, reason)
		}
	}
	real := []string{genuineDecisionReason, "Waiting on maintainer design guidance before altering workflow templates.", "rotate the deploy key?", ""}
	for _, reason := range real {
		if isNeedsDecisionRepoLocalGate(reason) {
			t.Errorf("genuine decision must NOT classify as a gate: %q", reason)
		}
		if v, ok := hub.reclassifyGateParkVerdict("k", completionVerdictNeedsDecision, reason); ok || v != completionVerdictNeedsDecision {
			t.Errorf("needs_decision + real reason must pass through, got (%q, %v) for %q", v, ok, reason)
		}
	}
	for _, verdict := range []string{completionVerdictNoWorkNeeded, completionVerdictShipped, completionVerdictIdle, completionVerdictBlocked} {
		if v, ok := hub.reclassifyGateParkVerdict("k", verdict, gateParkReason); ok || v != verdict {
			t.Errorf("verdict %q must pass through unchanged, got (%q, %v)", verdict, v, ok)
		}
	}
	// A nil hub still classifies (no re-kick memory to consult).
	var nilHub *ContributeWSHub
	if v, ok := nilHub.reclassifyGateParkVerdict("k", completionVerdictNeedsDecision, gateParkReason); !ok || v != completionVerdictIdle {
		t.Errorf("nil hub: got (%q, %v)", v, ok)
	}
}

func TestGateParkVerdict_OneRekickPerIssue(t *testing.T) {
	hub, _ := covK2Hub(t)
	fx := newToolingReleaseFixture()
	hub.gateParkReleaser = fx.release
	task := &WSTaskAssign{Kind: "issue", Repo: "acme/widgets", Number: 77}
	key := task.identityKey()

	if v, ok := hub.reclassifyGateParkVerdict(key, completionVerdictNeedsDecision, gateParkReason); !ok || v != completionVerdictIdle {
		t.Fatalf("first gate park must demote, got (%q, %v)", v, ok)
	}
	hub.releaseGateParkedIssue(task, "dev", gateParkReason)
	if len(fx.calls) != 1 || !strings.HasPrefix(fx.calls[0], "acme/widgets#77 by dev: ") {
		t.Fatalf("release must go through the seam once, got %v", fx.calls)
	}
	if v, ok := hub.reclassifyGateParkVerdict(key, completionVerdictNeedsDecision, gateParkReason); ok || v != completionVerdictNeedsDecision {
		t.Fatalf("second gate park on the same issue must stay parked, got (%q, %v)", v, ok)
	}
	// Another issue is unaffected.
	if v, ok := hub.reclassifyGateParkVerdict("other", completionVerdictNeedsDecision, gateParkReason); !ok || v != completionVerdictIdle {
		t.Fatalf("a different issue gets its own re-kick, got (%q, %v)", v, ok)
	}
	// An already-released marker found on GitHub (hub restarted) is logged,
	// not fatal, and a forge error likewise.
	fx.err = errGateParkAlreadyReleased
	hub.releaseGateParkedIssue(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 1}, "dev", gateParkReason)
	fx.err = errors.New("boom")
	hub.releaseGateParkedIssue(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 2}, "dev", gateParkReason)
	if len(fx.calls) != 3 {
		t.Fatalf("release must be attempted despite forge outcomes, got %v", fx.calls)
	}
	if !hub.gateParkRekicked(noPRKey("o/r", 2)) {
		t.Fatal("re-kick memory must be recorded even when the forge call failed")
	}
}

func TestGateParkVerdict_ReleaseSkipsNonIssueTasks(t *testing.T) {
	hub, _ := covK2Hub(t)
	fx := newToolingReleaseFixture()
	hub.gateParkReleaser = fx.release
	hub.releaseGateParkedIssue(nil, "u", "r")
	hub.releaseGateParkedIssue(&WSTaskAssign{Kind: "pr", Repo: "o/r", Number: 5}, "u", "r")
	hub.releaseGateParkedIssue(&WSTaskAssign{Kind: "issue", Repo: "", Number: 5}, "u", "r")
	hub.releaseGateParkedIssue(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 0}, "u", "r")
	hub.releaseGateParkedIssue(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 5, ExternalID: "LIN-1"}, "u", "r")
	if len(fx.calls) != 0 {
		t.Fatalf("only GitHub-backed issue tasks may be released, got %v", fx.calls)
	}
	var nilHub *ContributeWSHub
	nilHub.releaseGateParkedIssue(&WSTaskAssign{Kind: "issue", Repo: "o/r", Number: 5}, "u", "r")
	if nilHub.gateParkRekicked("x") {
		t.Fatal("nil hub has no re-kick memory")
	}
	var g *gateParkRekicks
	g.add("x")
	if g.has("x") {
		t.Fatal("nil memory must stay empty")
	}
}

func TestGateParkVerdict_ReleaseCommentIsMarkedAndBounded(t *testing.T) {
	long := strings.Repeat("§", noWorkReasonMaxLen*2)
	body := gateParkReleaseComment("dev", long, "stage/045-needs-decision")
	if !strings.HasPrefix(body, gateParkReleaseMarker) {
		t.Fatalf("release comment must start with the marker, got %q", body[:80])
	}
	for _, want := range []string{"@dev", "`stage/045-needs-decision`", "re-dispatched", "companion checkout", "label stays"} {
		if !strings.Contains(body, want) {
			t.Errorf("release comment lacks %q", want)
		}
	}
	if n := strings.Count(body, "§"); n == 0 || n > noWorkReasonMaxLen {
		t.Fatalf("reason must be capped at %d bytes, got %d marks", noWorkReasonMaxLen, n)
	}
	anon := gateParkReleaseComment("", "", "needs-decision")
	if strings.Contains(anon, "@") || strings.Contains(anon, "Reported reason") {
		t.Fatalf("anonymous, reasonless comment must omit the handle and the reason tail: %q", anon)
	}
}

func TestGateParkVerdict_ReleaseWithoutClientErrs(t *testing.T) {
	hub, _ := covK2Hub(t)
	hub.gateParkReleaser = nil
	if err := hub.releaseGateParkLabel(context.Background(), "o/r", 1, "u", "r"); err == nil {
		t.Fatal("no GitHub client must surface an error")
	}
}

// TestGateParkVerdict_WireShape drives the real task_complete handler with the
// completion the relay sent for widgets#77 and checks the hub booked an
// idle completion (no decision ledger row, short cooldown), released the
// label through the seam, re-offered the issue, and kept the SECOND gate
// park on the same issue as a real needs-decision.
func TestGateParkVerdict_WireShape(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"dev"}`
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
			Name: "widgets",
			Full: "acme/widgets",
			ActionableIssues: []any{
				noWorkIssue(77, "Smoke tests that check each agent loads what init and migrate install", time.Now().Add(-24*time.Hour)),
			},
		}},
	}
	s.statusMu.Unlock()

	hub := s.contributeHub
	fx := newToolingReleaseFixture()
	hub.gateParkReleaser = fx.release

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 77 {
		t.Fatalf("expected task_assign for #77, got %+v", assign)
	}
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: assign.TaskID, Result: "completed",
		CompletionSignal: completionSignalVerdict,
		Verdict:          "no_work_needed", VerdictNeedsDecision: true,
		VerdictReason: gateParkReason,
	})
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	if next := readMsg(t, conn); next.Type != "task_unavailable" {
		t.Fatalf("the issue is inside its short cooldown right after completion, got %+v", next)
	}
	select {
	case <-fx.released:
	case <-time.After(5 * time.Second):
		t.Fatal("the hub must release the decision label through the seam")
	}
	fx.mu.Lock()
	calls := append([]string(nil), fx.calls...)
	fx.mu.Unlock()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "acme/widgets#77 by dev: ") {
		t.Fatalf("release must target the completed issue for the reporting contributor, got %v", calls)
	}

	key := noPRKey("acme/widgets", 77)
	hub.completedMu.Lock()
	_, decisionRow := hub.noWorkVerdicts[key]
	hub.completedMu.Unlock()
	if decisionRow {
		t.Fatal("a gate park must not leave a needs-decision ledger row")
	}
	if got, full := recordedCooldown(t, hub, key), hub.configuredWithPRCooldown(); got >= full {
		t.Fatalf("gate park booked %v, want a short re-offer cooldown below the full %v", got, full)
	}

	// Re-offered once the short cooldown lapses.
	hub.completedMu.Lock()
	hub.completedTasks[key] = time.Now().Add(-(hub.completedTaskCooldown[key] + time.Minute))
	hub.completedMu.Unlock()
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 4})
	again := readMsg(t, conn)
	if again.Type != "task_assign" || again.Number != 77 {
		t.Fatalf("issue must be re-offered after the short cooldown, got %+v", again)
	}

	// The second gate park on the same issue is a real park: decision row,
	// full cooldown, no second release.
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: again.TaskID, Result: "completed",
		CompletionSignal: completionSignalVerdict,
		Verdict:          "no_work_needed", VerdictNeedsDecision: true,
		VerdictReason: gateParkReason,
	})
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 5})
	if next := readMsg(t, conn); next.Type != "task_unavailable" {
		t.Fatalf("parked issue must not be offered, got %+v", next)
	}
	hub.completedMu.Lock()
	rec, parked := hub.noWorkVerdicts[key]
	hub.completedMu.Unlock()
	if !parked || !rec.NeedsDecision {
		t.Fatalf("second gate park must book a needs-decision row, got %+v (present=%v)", rec, parked)
	}
	fx.mu.Lock()
	n := len(fx.calls)
	fx.mu.Unlock()
	if n != 1 {
		t.Fatalf("no second release may happen, got %d calls", n)
	}
}

// The contributor prompt carries the two rules that would have prevented the
// widgets parks, and the companion checkout plan when one is configured.
func TestTaskPromptRepoHints_GateAndCompanionText(t *testing.T) {
	hub, s := covK2Hub(t)
	s.deps.Config.Project.Org = "acme"
	s.deps.Config.Project.RepoPolicies = []config.RepoPolicy{{
		Repo:           "widgets",
		CompanionRepos: []string{"widgets-docs"},
	}}
	hints := hub.taskPromptRepoHints("acme/widgets")
	if len(hints.Companions) != 1 || hints.Companions[0] != "acme/widgets-docs" {
		t.Fatalf("companions = %v", hints.Companions)
	}
	if hints.Host == "" {
		t.Fatal("host must resolve to the configured forge base URL")
	}
	body := buildTaskPromptBodyWithHints("acme/widgets", "acme/widgets#77", "Smoke tests", "", "main", true, "", hints)
	for _, want := range []string{
		decisionpolicy.DispatchSatisfiesRepoGates,
		decisionpolicy.MissingCompanionCheckoutRule,
		"$HIVE_WORKSPACE_DIR/acme/widgets-docs",
		"git clone --depth 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	plain := buildTaskPromptBodyWithHints("acme/widgets", "acme/widgets#77", "Smoke tests", "", "main", true, "", taskPromptRepoHints{})
	if strings.Contains(plain, "git clone --depth 1") {
		t.Error("no companion clone instruction without configured companions")
	}
	if !strings.Contains(plain, decisionpolicy.DispatchSatisfiesRepoGates) {
		t.Error("gate rule must be present regardless of companions")
	}
	var nilHub *ContributeWSHub
	if h := nilHub.taskPromptRepoHints("x/y"); len(h.Companions) != 0 || h.Host != "" {
		t.Fatalf("nil hub hints = %+v", h)
	}
}

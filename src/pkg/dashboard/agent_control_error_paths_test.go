package dashboard

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// These tests cover branches of the owner-gated agent lifecycle handlers that
// no other suite reaches: handleResume's full success tail — including the
// #5706 pause-state ownership claim — and handleKick's scheduler fallback for
// an empty prompt, plus the prompt-length cap's contract and boundary. All are
// operator-facing mutation paths, so a silent regression (a 500, a false
// success, or a resume that lasts only until the next pod roll) misleads
// whoever is driving the dashboard.

// resumableServer builds an apiServer whose "scanner" carries a CONFIG-seeded
// pause: NewManager restores cfg.Paused as Paused=true with State left at
// StateStopped. That is the one resume shape with no relaunch leg
// (needsRelaunch is false), so the handler's success tail runs hermetically —
// no tmux, no mint.
func resumableServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	deps := testDeps(t)
	ac := deps.Config.Agents["scanner"]
	ac.Paused = true
	deps.Config.Agents["scanner"] = ac
	deps.AgentMgr = agent.NewManager(deps.Config.Agents, logger, agent.ProjectContext{})
	s := NewServer(0, logger)
	s.RegisterAPI(deps)
	return s, deps
}

// TestResumeConfigSeededPauseSucceedsAndClaimsOwnership pins handleResume's
// success tail. Resuming a paused, stopped agent must:
//   - answer 200 with changed:true and the running state label, and
//   - claim operator ownership of the agent's pause state (#5706) — without
//     the claim, the ACMM pack visibility sweep on the next restart re-pauses
//     any non-pack agent and the operator's resume silently evaporates.
func TestResumeConfigSeededPauseSucceedsAndClaimsOwnership(t *testing.T) {
	s, deps := resumableServer(t)

	rec := doPost(s, "/api/resume/scanner", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("resume of paused stopped agent = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := decodeKickJSON(t, rec.Body.String())
	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("resume reported ok=false: %v", body)
	}
	if status, _ := body["status"].(string); status != "resumed" {
		t.Errorf("status = %q, want %q", status, "resumed")
	}
	if changed, _ := body["changed"].(bool); !changed {
		t.Errorf("a genuine resume must report changed:true: %v", body)
	}

	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil || proc == nil {
		t.Fatalf("GetStatus after resume: %v", err)
	}
	if proc.Paused {
		t.Errorf("agent still paused after a 200 resume")
	}
	if !deps.Config.Agents["scanner"].PauseIsOperatorOwned() {
		t.Errorf("resume did not claim operator ownership of the pause state (#5706): %+v",
			deps.Config.Agents["scanner"])
	}
}

// TestResumeNotPausedAgentIsNoop pins the guard ahead of the tail: resuming an
// agent that is not paused must report changed:false and must not claim pause
// ownership — a routine no-op resume rewriting config ownership would make
// every agent immune to pack sweeps whether or not an operator ever acted.
func TestResumeNotPausedAgentIsNoop(t *testing.T) {
	s, deps := apiServer(t)

	rec := doPost(s, "/api/resume/scanner", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("noop resume = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := decodeKickJSON(t, rec.Body.String())
	if changed, _ := body["changed"].(bool); changed {
		t.Errorf("resume of a non-paused agent must report changed:false: %v", body)
	}
	if deps.Config.Agents["scanner"].PauseIsOperatorOwned() {
		t.Errorf("a noop resume must not claim pause-state ownership")
	}
}

// TestKickEmptyPromptFallsBackToSchedulerMessage covers the auto-message
// branch: an empty prompt with a scheduler attached must build the agent's
// message from the last actionable set instead of typing an empty line. The
// test agent has no tmux session, so delivery still fails — but with the
// manager's precondition error, proving the fallback ran without derailing the
// request.
func TestKickEmptyPromptFallsBackToSchedulerMessage(t *testing.T) {
	s, deps := apiServer(t)
	deps.Scheduler = scheduler.New(deps.Config, deps.Logger)

	rec := doPost(s, "/api/kick/scanner", map[string]string{})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("kick of non-running agent = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	msg, _ := decodeKickJSON(t, rec.Body.String())["error"].(string)
	if msg == "" {
		t.Fatalf("no error message in response: %s", rec.Body.String())
	}
	if strings.Contains(msg, "prompt too long") {
		t.Errorf("scheduler-built message tripped the length cap: %q", msg)
	}
}

// TestKickRejectsOverlongPromptWith400 pins the prompt-length cap's message
// contract. The kick prompt is typed verbatim into the agent's CLI session;
// the cap keeps a runaway client from wedging tmux. The rejection must name
// both the offending and the maximum length so the operator can fix the
// request without reading source.
func TestKickRejectsOverlongPromptWith400(t *testing.T) {
	s, _ := apiServer(t)

	long := strings.Repeat("x", 10001)
	rec := doPost(s, "/api/kick/scanner", map[string]string{"prompt": long})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overlong kick prompt = %d, want 400", rec.Code)
	}
	body := decodeKickJSON(t, rec.Body.String())
	if ok, _ := body["ok"].(bool); ok {
		t.Errorf("overlong prompt reported ok=true: %v", body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "prompt too long") {
		t.Errorf("error = %q, want it to say the prompt is too long", msg)
	}
	if !strings.Contains(msg, "10001") || !strings.Contains(msg, "10000") {
		t.Errorf("error = %q, want it to name the offending (10001) and maximum (10000) lengths", msg)
	}
}

// TestKickPromptAtCapIsNotRejectedForLength is the boundary: exactly
// maxKickPromptLen characters must pass the length gate. The test agent has no
// tmux session, so the kick still fails — but with the manager's precondition
// error, not the length rejection. If this test starts seeing "prompt too
// long", the cap became exclusive of its own limit.
func TestKickPromptAtCapIsNotRejectedForLength(t *testing.T) {
	s, _ := apiServer(t)

	atCap := strings.Repeat("x", 10000)
	rec := doPost(s, "/api/kick/scanner", map[string]string{"prompt": atCap})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("kick of non-running agent = %d, want 400", rec.Code)
	}
	msg, _ := decodeKickJSON(t, rec.Body.String())["error"].(string)
	if strings.Contains(msg, "prompt too long") {
		t.Errorf("a prompt of exactly maxKickPromptLen was rejected for length: %q", msg)
	}
}

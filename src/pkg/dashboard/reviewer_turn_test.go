package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/pkg/config"
)

// reviewerTurnTestEnv makes the Claude login and the run-as user hermetic:
// no operator token, the dashboard sign-in present when signedIn, and the
// run-as user lookup answering execErr.
func reviewerTurnTestEnv(t *testing.T, signedIn bool, execErr error) {
	t.Helper()
	t.Setenv(config.LiteLLMEndpointEnv, "")
	t.Setenv(claude.OperatorTokenEnvVar, "")
	t.Setenv(claude.OperatorTokenFileEnvVar, filepath.Join(t.TempDir(), "absent"))
	origRead, origExec := claudeReadToken, reviewerTurnExecUserSpec
	t.Cleanup(func() { claudeReadToken, reviewerTurnExecUserSpec = origRead, origExec })
	claudeReadToken = func(string) string {
		if signedIn {
			return "dashboard-sign-in"
		}
		return ""
	}
	reviewerTurnExecUserSpec = func() (string, error) { return "", execErr }
}

func delegateSettingsGET(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := doOwnerGet(s, decisionsDelegateTestPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	return decodeDelegateSettings(t, rec)
}

// With no reviewer endpoint and Claude signed in, the decider turn is ready
// on the Claude CLI path, and the settings say so. On the old code
// llm_reviewer_ready was false here (it required an endpoint) and there was
// no path field.
func TestDecisionsDelegateReadyOnClaudeCLIFallback(t *testing.T) {
	reviewerTurnTestEnv(t, true, nil)
	s := covApiServer(t)
	s.deps.Config.Governor.Trajectory.Endpoint = ""
	s.deps.Config.Governor.LiteLLM.Endpoint = ""

	got := delegateSettingsGET(t, s)
	if got["llm_reviewer_ready"] != true || got["llm_reviewer_path"] != config.ReviewerPathClaudeCLI || got["llm_reviewer_reason"] != "" {
		t.Fatalf("CLI fallback not reported ready: path=%v ready=%v reason=%v", got["llm_reviewer_path"], got["llm_reviewer_ready"], got["llm_reviewer_reason"])
	}

	// The admin MCP reads this same endpoint through settings_read
	// (section decisions_delegate); the Settings tab reads the governor
	// config's decisions_delegate block. All three agree.
	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor: %d %s", rec.Code, rec.Body.String())
	}
	var gov map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &gov); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gov["decisions_delegate"], any(got)) {
		t.Fatalf("governor config decisions_delegate differs from the delegate endpoint:\n%v\n%v", gov["decisions_delegate"], got)
	}
}

// Not signed in and no endpoint: not ready, path none, and the reason says
// what is missing.
func TestDecisionsDelegateNotReadyNamesReason(t *testing.T) {
	reviewerTurnTestEnv(t, false, nil)
	s := covApiServer(t)
	s.deps.Config.Governor.Trajectory.Endpoint = ""
	s.deps.Config.Governor.LiteLLM.Endpoint = ""

	got := delegateSettingsGET(t, s)
	reason, _ := got["llm_reviewer_reason"].(string)
	if got["llm_reviewer_ready"] != false || got["llm_reviewer_path"] != config.ReviewerPathNone || !strings.Contains(reason, "not signed in") {
		t.Fatalf("path=%v ready=%v reason=%q", got["llm_reviewer_path"], got["llm_reviewer_ready"], reason)
	}
}

// Under UID isolation without the turn's own user, the CLI path cannot run,
// so readiness is false and the reason names the missing user.
func TestDecisionsDelegateCLINotReadyWithoutRunAsUser(t *testing.T) {
	reviewerTurnTestEnv(t, true, errors.New("per-agent UID isolation is active but the CLI user identity \"hive-turn\" has no UID"))
	s := covApiServer(t)
	s.deps.Config.Governor.Trajectory.Endpoint = ""
	s.deps.Config.Governor.LiteLLM.Endpoint = ""

	got := delegateSettingsGET(t, s)
	reason, _ := got["llm_reviewer_reason"].(string)
	if got["llm_reviewer_ready"] != false || got["llm_reviewer_path"] != config.ReviewerPathClaudeCLI || !strings.Contains(reason, "reviewer turn user") {
		t.Fatalf("path=%v ready=%v reason=%q", got["llm_reviewer_path"], got["llm_reviewer_ready"], reason)
	}
}

// With an endpoint, the path is HTTP whatever the Claude login says: the
// behaviour before governor.reviewer_backend existed.
func TestDecisionsDelegateEndpointKeepsHTTP(t *testing.T) {
	reviewerTurnTestEnv(t, true, nil)
	s := covApiServer(t)
	s.deps.Config.Governor.Trajectory.Endpoint = "https://llm.example.com"
	s.deps.Config.Governor.Trajectory.Model = "reviewer-model"

	got := delegateSettingsGET(t, s)
	if got["llm_reviewer_ready"] != true || got["llm_reviewer_path"] != config.ReviewerPathHTTP || got["llm_model_effective"] != "reviewer-model" {
		t.Fatalf("path=%v ready=%v model=%v", got["llm_reviewer_path"], got["llm_reviewer_ready"], got["llm_model_effective"])
	}
}

// The exported run-as helpers are the Spektacular executor's, bound to the
// reviewer turn's identity.
func TestReviewerTurnRunAsHelpers(t *testing.T) {
	got := ReviewerTurnRunAs([]string{"claude", "-p"}, "hive-hive-turn", "/tmp/x/home")
	want := spekHubRunAsCommand([]string{"claude", "-p"}, "hive-hive-turn", "/tmp/x/home")
	if !reflect.DeepEqual(got, want) || got[0] != "su-exec" {
		t.Fatalf("runAs = %q", got)
	}
	env := ReviewerTurnExecEnv([]string{"PATH=/usr/bin"})
	if !strings.Contains(strings.Join(env, "\n"), "HIVE_PROXY_AGENT="+ReviewerTurnIdentity) {
		t.Fatalf("exec env lacks the proxy identity: %q", env)
	}
	orig := reviewerTurnExecUserSpec
	t.Cleanup(func() { reviewerTurnExecUserSpec = orig })
	reviewerTurnExecUserSpec = func() (string, error) { return "hive-hive-turn", nil }
	if spec, err := ReviewerTurnExecUser(); err != nil || spec != "hive-hive-turn" {
		t.Fatalf("spec=%q err=%v", spec, err)
	}
}

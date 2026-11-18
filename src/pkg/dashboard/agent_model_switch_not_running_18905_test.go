package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	notRunningTestAgent  = "supervisor"
	notRunningTestModel  = "opus"
	notRunningTestMethod = "gemini"
)

// notRunningServer returns a server whose config carries an extra agent that the
// runtime does not instantiate (stopped, or held back by the ACMM gate), with an
// agents overlay directory so the second config layer is observable.
func notRunningServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, deps := apiServer(t)
	dir := t.TempDir()
	deps.Config.Data.AgentsDir = dir
	deps.Config.Agents[notRunningTestAgent] = config.AgentConfig{Backend: "claude", Model: "sonnet", Enabled: true}
	return s, dir
}

func decodeBody18905(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

func overlayText(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read overlay dir: %v", err)
	}
	var sb strings.Builder
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read overlay %s: %v", e.Name(), err)
		}
		sb.Write(b)
	}
	return sb.String()
}

// A configured agent the runtime does not hold (stopped) accepts a model change
// by persisting it to both config layers, and says it applies on next start.
func TestHandleModelSet_StoppedConfiguredAgentSaves(t *testing.T) {
	s, dir := notRunningServer(t)
	rec := doPost(s, "/api/model/"+notRunningTestAgent+"/"+notRunningTestModel, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody18905(t, rec)
	if body["ok"] != true || body["applied"] != false {
		t.Fatalf("want ok=true applied=false, got %v", body)
	}
	if want := "saved; applies when " + notRunningTestAgent + " starts"; body["message"] != want {
		t.Fatalf("message = %v, want %q", body["message"], want)
	}
	got := s.deps.Config.Agents[notRunningTestAgent]
	if got.Model != notRunningTestModel || got.ModelOwner != config.FieldOwnerOperator {
		t.Fatalf("in-memory config not updated: %+v", got)
	}
	if !strings.Contains(overlayText(t, dir), notRunningTestModel) {
		t.Fatalf("agent overlay layer does not carry the saved model")
	}
}

func TestHandleSwitch_StoppedConfiguredAgentSaves(t *testing.T) {
	s, dir := notRunningServer(t)
	rec := doPost(s, "/api/switch/"+notRunningTestAgent+"/"+notRunningTestMethod, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody18905(t, rec)
	if body["applied"] != false || !strings.Contains(body["message"].(string), "applies when "+notRunningTestAgent+" starts") {
		t.Fatalf("unexpected body %v", body)
	}
	if got := s.deps.Config.Agents[notRunningTestAgent]; got.Backend != notRunningTestMethod {
		t.Fatalf("backend = %q, want %q", got.Backend, notRunningTestMethod)
	}
	if !strings.Contains(overlayText(t, dir), notRunningTestMethod) {
		t.Fatalf("agent overlay layer does not carry the saved backend")
	}
}

func TestHandleSwitch_StoppedConfiguredAgentRejectsBadBackend(t *testing.T) {
	s, _ := notRunningServer(t)
	rec := doPost(s, "/api/switch/"+notRunningTestAgent+"/not-a-backend", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := s.deps.Config.Agents[notRunningTestAgent]; got.Backend != "claude" {
		t.Fatalf("rejected backend must not be saved, got %q", got.Backend)
	}
}

// A paused agent the runtime does hold accepts the change and is not restarted
// into running: the response says it applies on start and the pause survives.
func TestHandleModelSet_PausedAgentSavesWithoutRestart(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.Pause("scanner", "test", "paused for 18905"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	rec := doPost(s, "/api/model/scanner/"+notRunningTestModel, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody18905(t, rec)
	if body["applied"] != false || body["message"] != "saved; applies when scanner starts" {
		t.Fatalf("unexpected body %v", body)
	}
	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil || proc == nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !proc.Paused {
		t.Fatalf("model change must not resume a paused agent")
	}
	if proc.ModelOverride != notRunningTestModel {
		t.Fatalf("ModelOverride = %q, want %q", proc.ModelOverride, notRunningTestModel)
	}
	if got := deps.Config.Agents["scanner"]; got.Model != notRunningTestModel {
		t.Fatalf("config model = %q, want %q", got.Model, notRunningTestModel)
	}
}

// A name that is neither running nor configured is still not found, and the
// error lists the valid names.
func TestHandleModelSet_UnknownAgentListsValidNames(t *testing.T) {
	s, _ := notRunningServer(t)
	for _, path := range []string{"/api/model/ghost/opus", "/api/switch/ghost/gemini"} {
		rec := doPost(s, path, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", path, rec.Code)
		}
		msg, _ := decodeBody18905(t, rec)["error"].(string)
		if !strings.Contains(msg, "agent ghost not found") ||
			!strings.Contains(msg, "scanner") || !strings.Contains(msg, notRunningTestAgent) {
			t.Fatalf("%s: error %q must say not found and list valid names", path, msg)
		}
	}
}

// An agent the runtime holds still gets its override set. The test runtime never
// launches agents, so scanner reads as stopped and answers "saved"; a launched
// agent answers "model_set" with no applied field. Either way the runtime
// override is set and the call succeeds.
func TestHandleModelSet_RegisteredAgentSetsOverride(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPost(s, "/api/model/scanner/"+notRunningTestModel, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeBody18905(t, rec)
	if body["ok"] != true {
		t.Fatalf("want ok=true, got %v", body)
	}
	if proc, _ := deps.AgentMgr.GetStatus("scanner"); proc == nil || proc.ModelOverride != notRunningTestModel {
		t.Fatalf("runtime ModelOverride not set")
	}
}

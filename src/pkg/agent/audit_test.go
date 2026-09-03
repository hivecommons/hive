package agent

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// recordedAudit is one captured audit event.
type recordedAudit struct {
	Actor  string
	Action string
	Agent  string
	Fields map[string]any
}

// fakeAuditSink captures events for assertions. Concurrency-safe because the
// manager records from the launch path and from background goroutines.
type fakeAuditSink struct {
	mu      sync.Mutex
	entries []recordedAudit
}

func (f *fakeAuditSink) Record(actor, action, agentName string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, recordedAudit{Actor: actor, Action: action, Agent: agentName, Fields: fields})
}

func (f *fakeAuditSink) find(action string) *recordedAudit {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.entries {
		if f.entries[i].Action == action {
			return &f.entries[i]
		}
	}
	return nil
}

func (f *fakeAuditSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

func testManagerWithSink(t *testing.T) (*Manager, *fakeAuditSink) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(map[string]config.AgentConfig{}, logger, ProjectContext{})
	sink := &fakeAuditSink{}
	m.SetAuditSink(sink)
	return m, sink
}

// TestAuditNilSinkIsNoOp is the load-bearing one for every other package: the
// manager must work with no dashboard attached, which is how all unit tests
// and any non-dashboard embedding construct it.
func TestAuditNilSinkIsNoOp(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(map[string]config.AgentConfig{}, logger, ProjectContext{})

	// No sink ever installed.
	m.audit(AuditAgentStarted, "nobody", auditFields("backend", "claude"))
	m.auditWithActor("alice", AuditAgentStopped, "nobody", nil)

	// And explicitly installing a nil interface must also not panic.
	m.SetAuditSink(nil)
	m.audit(AuditAgentStartFailed, "nobody", auditFields("error", "boom"))
}

// TestAuditLaunchFailureRecordsBackendModelAndError is the watsonx incident:
// an agent whose backend the image does not support must leave a durable,
// queryable record naming the backend, the model, and the error.
func TestAuditLaunchFailureRecordsBackendModelAndError(t *testing.T) {
	m, sink := testManagerWithSink(t)

	m.AddAgent("ci-maintainer", config.AgentConfig{Backend: "watsonx", Model: "granite-3"})
	m.mu.Lock()
	ap := m.agents["ci-maintainer"]
	ap.LastError = "unknown backend: watsonx"
	m.announceLaunchFailureInPane(ap, "backend watsonx did not launch")
	m.mu.Unlock()

	e := sink.find(AuditAgentStartFailed)
	if e == nil {
		t.Fatalf("no %s event recorded; got %d entries", AuditAgentStartFailed, sink.count())
	}
	if e.Agent != "ci-maintainer" {
		t.Errorf("agent = %q, want ci-maintainer", e.Agent)
	}
	if e.Fields["backend"] != "watsonx" {
		t.Errorf("backend = %v, want watsonx", e.Fields["backend"])
	}
	if e.Fields["model"] != "granite-3" {
		t.Errorf("model = %v, want granite-3", e.Fields["model"])
	}
	if e.Fields["outcome"] != "failure" {
		t.Errorf("outcome = %v, want failure", e.Fields["outcome"])
	}
	errText, _ := e.Fields["error"].(string)
	if !strings.Contains(errText, "unknown backend: watsonx") {
		t.Errorf("error = %q, want it to contain the launch error text", errText)
	}
}

// TestAuditLaunchFailureUsesOverrides proves the audit reports what the agent
// will ACTUALLY launch with, not just its static config.
func TestAuditLaunchFailureUsesOverrides(t *testing.T) {
	m, sink := testManagerWithSink(t)

	m.AddAgent("a1", config.AgentConfig{Backend: "claude", Model: "opus"})
	m.mu.Lock()
	ap := m.agents["a1"]
	ap.BackendOverride = "watsonx"
	ap.ModelOverride = "granite-4"
	ap.LastError = "unknown backend: watsonx"
	m.announceLaunchFailureInPane(ap, "nope")
	m.mu.Unlock()

	e := sink.find(AuditAgentStartFailed)
	if e == nil {
		t.Fatal("no launch failure event recorded")
	}
	if e.Fields["backend"] != "watsonx" || e.Fields["model"] != "granite-4" {
		t.Errorf("got backend=%v model=%v, want the overrides watsonx/granite-4",
			e.Fields["backend"], e.Fields["model"])
	}
}

func TestAuditAddAndRemoveRecordMethodAndModel(t *testing.T) {
	m, sink := testManagerWithSink(t)

	m.AddAgent("a1", config.AgentConfig{Backend: "copilot", Model: "gpt-5"})
	added := sink.find(AuditAgentAdded)
	if added == nil {
		t.Fatal("no agent_added event")
	}
	if added.Fields["backend"] != "copilot" || added.Fields["model"] != "gpt-5" {
		t.Errorf("added: backend=%v model=%v, want copilot/gpt-5", added.Fields["backend"], added.Fields["model"])
	}
	if added.Actor != auditActorSystem {
		t.Errorf("actor = %q, want %q", added.Actor, auditActorSystem)
	}

	m.RemoveAgent("a1")
	removed := sink.find(AuditAgentRemoved)
	if removed == nil {
		t.Fatal("no agent_removed event")
	}
	if removed.Fields["backend"] != "copilot" || removed.Fields["model"] != "gpt-5" {
		t.Errorf("removed: backend=%v model=%v, want copilot/gpt-5",
			removed.Fields["backend"], removed.Fields["model"])
	}
}

func TestAuditBackendChangeRecordsOldAndNew(t *testing.T) {
	m, sink := testManagerWithSink(t)
	m.AddAgent("a1", config.AgentConfig{Backend: "claude", Model: "opus"})

	if err := m.SetBackendOverride("a1", "copilot"); err != nil {
		t.Fatalf("SetBackendOverride: %v", err)
	}
	e := sink.find(AuditAgentBackendSet)
	if e == nil {
		t.Fatal("no agent_backend_changed event")
	}
	if e.Fields["previous_backend"] != "claude" {
		t.Errorf("previous_backend = %v, want claude", e.Fields["previous_backend"])
	}
	if e.Fields["backend"] != "copilot" {
		t.Errorf("backend = %v, want copilot", e.Fields["backend"])
	}
}

// TestAuditBackendChangeSkipsNoOp guards the volume decision: only real state
// transitions are audited, so config reloads re-applying the current value do
// not flood the ring buffer.
func TestAuditBackendChangeSkipsNoOp(t *testing.T) {
	m, sink := testManagerWithSink(t)
	m.AddAgent("a1", config.AgentConfig{Backend: "claude"})

	if err := m.SetBackendOverride("a1", "claude"); err != nil {
		t.Fatalf("SetBackendOverride: %v", err)
	}
	if e := sink.find(AuditAgentBackendSet); e != nil {
		t.Errorf("no-op backend set should record nothing, got %+v", e.Fields)
	}
}

func TestAuditModelChangeRecordsOldAndNew(t *testing.T) {
	m, sink := testManagerWithSink(t)
	m.AddAgent("a1", config.AgentConfig{Backend: "claude", Model: "sonnet"})

	if err := m.SetModelOverride("a1", "opus"); err != nil {
		t.Fatalf("SetModelOverride: %v", err)
	}
	e := sink.find(AuditAgentModelSet)
	if e == nil {
		t.Fatal("no agent_model_changed event")
	}
	if e.Fields["previous_model"] != "sonnet" {
		t.Errorf("previous_model = %v, want sonnet", e.Fields["previous_model"])
	}
	if e.Fields["model"] != "opus" {
		t.Errorf("model = %v, want opus", e.Fields["model"])
	}
	if e.Fields["backend"] != "claude" {
		t.Errorf("backend = %v, want claude", e.Fields["backend"])
	}
}

func TestAuditModelChangeSkipsNoOp(t *testing.T) {
	m, sink := testManagerWithSink(t)
	m.AddAgent("a1", config.AgentConfig{Backend: "claude", Model: "opus"})

	if err := m.SetModelOverride("a1", "opus"); err != nil {
		t.Fatalf("SetModelOverride: %v", err)
	}
	if e := sink.find(AuditAgentModelSet); e != nil {
		t.Errorf("no-op model set should record nothing, got %+v", e.Fields)
	}
}

func TestAuditPinModelRecordsChange(t *testing.T) {
	m, sink := testManagerWithSink(t)
	m.AddAgent("a1", config.AgentConfig{Backend: "claude", Model: "sonnet"})

	if err := m.PinModel("a1", "opus"); err != nil {
		t.Fatalf("PinModel: %v", err)
	}
	e := sink.find(AuditAgentModelSet)
	if e == nil {
		t.Fatal("no agent_model_changed event for pin")
	}
	if e.Fields["previous_model"] != "sonnet" || e.Fields["model"] != "opus" {
		t.Errorf("got previous=%v model=%v, want sonnet -> opus",
			e.Fields["previous_model"], e.Fields["model"])
	}
	if e.Fields["trigger"] != "pin" {
		t.Errorf("trigger = %v, want pin", e.Fields["trigger"])
	}
}

func TestAuditFieldsDropsEmptyStringsAndOddKey(t *testing.T) {
	f := auditFields("backend", "claude", "model", "", "count", 3, "dangling")
	if _, ok := f["model"]; ok {
		t.Error("empty-string value should be dropped so the UI has no noisy 'model='")
	}
	if f["backend"] != "claude" {
		t.Errorf("backend = %v, want claude", f["backend"])
	}
	if f["count"] != 3 {
		t.Errorf("count = %v, want 3", f["count"])
	}
	if _, ok := f["dangling"]; ok {
		t.Error("odd trailing key should be dropped")
	}
}

// TestFormatAuditDetailIsStableAndOrdered pins the detail string shape the
// Audit Log UI renders and filters over. Go map iteration is randomized, so
// without the sort the same event would render differently each time.
func TestFormatAuditDetailIsStableAndOrdered(t *testing.T) {
	fields := auditFields(
		"error", "unknown backend: watsonx",
		"model", "granite-3",
		"outcome", "failure",
		"backend", "watsonx",
	)
	got := FormatAuditDetail(fields)
	want := "outcome=failure, backend=watsonx, model=granite-3, error=unknown backend: watsonx"
	if got != want {
		t.Errorf("FormatAuditDetail =\n  %q\nwant\n  %q", got, want)
	}
	for i := 0; i < 10; i++ {
		if FormatAuditDetail(fields) != want {
			t.Fatal("FormatAuditDetail is not stable across calls")
		}
	}
	if FormatAuditDetail(nil) != "" {
		t.Error("empty fields should render as the empty string")
	}
}

package dashboard

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/flue"
	"github.com/hivecommons/hive/pkg/outputschema"
)

const (
	extTestIdentity = "relay-ext"
	extTestTask     = "task-8361"
	extTestKey      = "hivecommons/hive#8361"
	extTestTier     = "C4"
	extTestGen      = uint64(5)
	extTestRevision = "0123456789abcdef0123456789abcdef01234567"
)

func extTestAdmission() extwork.Admission {
	return extwork.Admission{
		WorkKey:          extTestKey,
		AssignmentID:     extTestTask,
		Generation:       extTestGen,
		Stage:            StageImplement,
		ContractRevision: "contract-1",
		Engine:           extExecEngineFlue,
		WorkflowVersion:  "flue-fixture/1.0.0",
		InputRevision:    extTestRevision,
		RequestDigest:    extwork.RequestDigest([]byte("bundle")),
		Authority:        extwork.AuthorityBinding{Identity: extTestIdentity, Tier: extTestTier, Capability: capExtExecFlue, Mode: extwork.ModeReportOnly},
	}
}

// extLeaseHub returns a persisting hub holding the lease the test admission
// names, minted through the production recordLeaseForKeyStage path.
func extLeaseHub(t *testing.T, now time.Time) *ContributeWSHub {
	t.Helper()
	h := &ContributeWSHub{logger: covBLogger(), persistTaskLedgers: true, taskLeasesFile: filepath.Join(t.TempDir(), "leases.json")}
	if err := h.recordLeaseForKeyStage(extTestIdentity, extTestTask, "hivecommons/hive", 8361, extTestKey, extTestTier, StageImplement, extTestGen, now); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCapabilityTokenMatchesAdapter(t *testing.T) {
	if capExtExecFlue != flue.Capability {
		t.Fatalf("dashboard token %q must equal flue.Capability %q", capExtExecFlue, flue.Capability)
	}
	if extExecEngineFlue != flue.Engine {
		t.Fatalf("dashboard engine %q must equal flue.Engine %q", extExecEngineFlue, flue.Engine)
	}
	found := false
	for _, c := range serverCapabilities() {
		if c == capExtExecFlue {
			found = true
		}
	}
	if !found {
		t.Fatal("hub must advertise the ext-exec/flue token")
	}
}

func TestLeaseAdmissionStorePersistAndLoad(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := extLeaseHub(t, now)
	dir := filepath.Join(t.TempDir(), "records")
	store := newLeaseAdmissionStore(h, dir, func() time.Time { return now })
	adm := extTestAdmission()

	bad := adm
	bad.WorkKey = ""
	if err := store.Persist(bad); !errors.Is(err, extwork.ErrInvalidAdmission) {
		t.Fatalf("invalid admission = %v", err)
	}
	for name, mutate := range map[string]func(*extwork.Admission){
		"other identity": func(a *extwork.Admission) { a.Authority.Identity = "someone-else" },
		"other task":     func(a *extwork.Admission) { a.AssignmentID = "task-other" },
		"other work key": func(a *extwork.Admission) { a.WorkKey = "hivecommons/hive#1" },
		"other gen":      func(a *extwork.Admission) { a.Generation = extTestGen + 1 },
		"other stage":    func(a *extwork.Admission) { a.Stage = StagePlan },
		"other tier":     func(a *extwork.Admission) { a.Authority.Tier = "C1" },
	} {
		m := adm
		mutate(&m)
		if err := store.Persist(m); !errors.Is(err, errLeaseAdmission) {
			t.Errorf("%s: Persist = %v, want lease mismatch", name, err)
		}
	}
	if _, ok, err := store.Load(extTestTask); ok || err != nil {
		t.Fatalf("Load before persist = %v %v", ok, err)
	}
	if err := store.Persist(adm); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	got, ok, err := store.Load(extTestTask)
	if err != nil || !ok || got.ExecutionKey() != adm.ExecutionKey() {
		t.Fatalf("Load = %+v %v %v", got, ok, err)
	}
	if err := store.SaveReceipt(extTestTask, []byte(`{"r":1}`)); err != nil {
		t.Fatal(err)
	}
	if raw, ok, err := store.LoadReceipt(extTestTask); err != nil || !ok || string(raw) != `{"r":1}` {
		t.Fatalf("LoadReceipt = %q %v %v", raw, ok, err)
	}
	// The lease expires: the record no longer grants anything.
	later := newLeaseAdmissionStore(h, dir, func() time.Time { return now.Add(leaseTTL + time.Minute) })
	if _, ok, err := later.Load(extTestTask); ok || err != nil {
		t.Fatalf("Load after lease expiry = %v %v", ok, err)
	}
	if err := later.Persist(adm); !errors.Is(err, errLeaseAdmission) {
		t.Fatalf("Persist after lease expiry = %v", err)
	}
	// The lease is revoked: same outcome.
	h.revokeLease(extTestIdentity, extTestTask)
	if _, ok, _ := store.Load(extTestTask); ok {
		t.Fatal("record granted authority after the lease was revoked")
	}
}

func TestLeaseAdmissionStoreRefusesWhenLeaseNotDurable(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := extLeaseHub(t, now)
	// Point the registry file below a regular file so the directory can never
	// be created and the lease cannot reach disk.
	blocker := filepath.Join(t.TempDir(), "leases.json")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.taskLeasesFile = filepath.Join(blocker, "nested")
	store := newLeaseAdmissionStore(h, filepath.Join(t.TempDir(), "records"), func() time.Time { return now })
	err := store.Persist(extTestAdmission())
	if err == nil || !strings.Contains(err.Error(), "lease not durable") {
		t.Fatalf("Persist with unwritable lease file = %v", err)
	}
	if _, ok, _ := store.Load(extTestTask); ok {
		t.Fatal("a record was written although the lease never reached disk")
	}
}

func TestAuditProgressSink(t *testing.T) {
	s := covApiServer(t)
	sink := auditProgressSink{sink: s.AgentAuditSink()}
	adm := extTestAdmission()
	sink.Record(extwork.ProgressEvent{Action: extwork.EventProgress, ExecutionKey: adm.ExecutionKey(), AssignmentID: adm.AssignmentID, State: extwork.StateWaiting, Fields: map[string]any{"stage": "instrument"}})
	var found bool
	for _, e := range s.audit.Recent(20) {
		if e.Action != extwork.EventProgress {
			continue
		}
		found = true
		if e.User != "system" || e.Agent != adm.AssignmentID {
			t.Errorf("entry actor/agent = %q/%q", e.User, e.Agent)
		}
		for _, want := range []string{string(adm.ExecutionKey()), "waiting", "instrument"} {
			if !strings.Contains(e.Detail, want) {
				t.Errorf("audit detail lacks %q: %s", want, e.Detail)
			}
		}
	}
	if !found {
		t.Fatal("progress event did not reach the audit log")
	}
	// A nil sink is a no-op, never a panic.
	auditProgressSink{}.Record(extwork.ProgressEvent{Action: "x"})
}

type fakeExtAdapter struct{ starts int }

func (f *fakeExtAdapter) Engine() string { return extExecEngineFlue }
func (f *fakeExtAdapter) Start(context.Context, extwork.StartRequest) (extwork.StartResult, error) {
	f.starts++
	return extwork.StartResult{RemoteRunID: "r", RemoteIncarnation: "i"}, nil
}
func (f *fakeExtAdapter) Observe(context.Context, extwork.ExecutionKey, string) (extwork.Observation, error) {
	return extwork.Observation{}, extwork.ErrNotFound
}
func (f *fakeExtAdapter) Cancel(context.Context, extwork.ExecutionKey, string) (extwork.CancelFacts, error) {
	return extwork.CancelFacts{}, nil
}
func (f *fakeExtAdapter) OpenArtifact(context.Context, extwork.ExecutionKey, string, string) (io.ReadCloser, error) {
	return nil, extwork.ErrNotFound
}

func TestExternalFlueBindingFailsClosed(t *testing.T) {
	s := covApiServer(t)
	prevDir := outputschema.AgentReportDir
	outputschema.AgentReportDir = t.TempDir()
	t.Cleanup(func() { outputschema.AgentReportDir = prevDir })
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	if _, err := s.externalFlueBinding(clock); !errors.Is(err, errExternalBindingOff) {
		t.Fatalf("default config = %v, want off", err)
	}
	s.deps.Config.Runs.External.Flue.Enabled = true
	if _, err := s.externalFlueBinding(clock); !errors.Is(err, errExternalHubNotRunning) {
		t.Fatalf("no hub = %v", err)
	}
	s.contributeHub = extLeaseHub(t, now)
	if _, err := s.externalFlueBinding(clock); !errors.Is(err, extwork.ErrEngineNotLinked) {
		t.Fatalf("unlinked engine = %v (the default registry must not hold flue in this build)", err)
	}
	// A linked engine in shadow mode builds a binding that never starts.
	fake := &fakeExtAdapter{}
	reg := extwork.NewRegistry()
	reg.Register(extExecEngineFlue, func(settings map[string]string) (extwork.Adapter, error) {
		if settings[extwork.SettingEndpoint] != "http://127.0.0.1:1" {
			t.Errorf("settings = %v", settings)
		}
		return fake, nil
	})
	prev := extworkRegistry
	extworkRegistry = reg
	t.Cleanup(func() { extworkRegistry = prev })
	s.deps.Config.Runs.External.Flue.Endpoint = "http://127.0.0.1:1"
	b, err := s.externalFlueBinding(clock)
	if err != nil || b.Mode() != extwork.ModeShadow {
		t.Fatalf("shadow binding = %v %v", b, err)
	}
	adm := extTestAdmission()
	adm.Authority.Mode = extwork.ModeShadow
	res, err := b.Dispatch(context.Background(), adm, []byte("bundle"))
	if err != nil || !res.Shadow || res.Started || fake.starts != 0 {
		t.Fatalf("shadow dispatch = %+v %v starts=%d", res, err, fake.starts)
	}
	s.deps.Config.Runs.External.Flue.Mode = config.FlueBindingModeReportOnly
	b, err = s.externalFlueBinding(clock)
	if err != nil || b.Mode() != extwork.ModeReportOnly {
		t.Fatalf("report-only binding = %v %v", b, err)
	}
	var nilServer *Server
	if _, err := nilServer.externalFlueBinding(clock); !errors.Is(err, errExternalBindingOff) {
		t.Fatalf("nil server = %v", err)
	}
}

func TestExtExecAdmissibleRefusesNeverDowngrades(t *testing.T) {
	s := covApiServer(t)
	h := &ContributeWSHub{logger: covBLogger(), server: s}
	if got := extExecEngineFromIssueMap(map[string]any{"title": "x"}); got != "" {
		t.Fatalf("plain issue engine = %q", got)
	}
	if got := extExecEngineFromIssueMap(map[string]any{"ext_exec": " flue "}); got != "flue" {
		t.Fatalf("ext_exec = %q", got)
	}
	if got := extExecEngineFromIssueMap(map[string]any{"external_engine": "flue"}); got != "flue" {
		t.Fatalf("external_engine = %q", got)
	}
	withCap := &ContributorConnection{capabilities: &ContributorCapabilities{RelayCapabilities: []string{capExtExecFlue}}}
	withoutCap := &ContributorConnection{capabilities: &ContributorCapabilities{RelayCapabilities: []string{capRunStage}}}

	if ok, reason := h.extExecAdmissible("spektacular", withCap); ok || !strings.Contains(reason, "unsupported") {
		t.Fatalf("other engine = %v %q", ok, reason)
	}
	if ok, reason := h.extExecAdmissible(extExecEngineFlue, withCap); ok || !strings.Contains(reason, "disabled") {
		t.Fatalf("disabled binding = %v %q", ok, reason)
	}
	s.deps.Config.Runs.External.Flue.Enabled = true
	if ok, reason := h.extExecAdmissible(extExecEngineFlue, withoutCap); ok || !strings.Contains(reason, capExtExecFlue) {
		t.Fatalf("relay without capability = %v %q", ok, reason)
	}
	if ok, _ := h.extExecAdmissible(extExecEngineFlue, nil); ok {
		t.Fatal("nil connection admitted")
	}
	if ok, _ := h.extExecAdmissible(extExecEngineFlue, &ContributorConnection{}); ok {
		t.Fatal("connection without declared capabilities admitted")
	}
	if ok, reason := h.extExecAdmissible(extExecEngineFlue, withCap); !ok {
		t.Fatalf("positive control refused: %q", reason)
	}
	var nilHub *ContributeWSHub
	if ok, _ := nilHub.extExecAdmissible(extExecEngineFlue, withCap); ok {
		t.Fatal("nil hub admitted")
	}
}

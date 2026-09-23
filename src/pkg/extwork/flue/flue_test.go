package flue_test

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/flue"
	"github.com/hivecommons/hive/pkg/extwork/flue/fixture"
)

const (
	childEnv        = "HIVE_FLUE_FIXTURE_CHILD"
	workflowDir     = "testdata/flue-fixture"
	workflowVersion = "flue-fixture/1.0.0"
	inputRevision   = "9f1c2d3e4a5b6c7d8e9f0a1b2c3d4e5f60718293"
)

// TestMain doubles as the fixture process: when the conformance tests re-exec
// this binary with childEnv set, everything after "--" is the fixture argv.
func TestMain(m *testing.M) {
	flag.Parse()
	if os.Getenv(childEnv) == "1" {
		// Serve until killed; the parent ends the process with a signal.
		os.Exit(fixture.Run(context.Background(), flag.Args(), os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func admissionFor(t *testing.T, workKey, task string, gen uint64, hints map[string]string) (extwork.Admission, []byte) {
	t.Helper()
	adm := extwork.Admission{
		WorkKey:          workKey,
		AssignmentID:     task,
		Generation:       gen,
		Stage:            "implement",
		ContractRevision: "contract-1",
		Engine:           flue.Engine,
		WorkflowVersion:  workflowVersion,
		InputRevision:    inputRevision,
		Authority:        extwork.AuthorityBinding{Identity: "relay-a", Tier: "T1", Capability: flue.Capability, Mode: extwork.ModeReportOnly},
	}
	payload, err := flue.BuildBundle(adm, "bounded summary for "+workKey, "hivecommons/flue-fixture", nil, hints)
	if err != nil {
		t.Fatal(err)
	}
	adm.RequestDigest = extwork.RequestDigest(payload)
	return adm, payload
}

func TestNewAndFactoryValidation(t *testing.T) {
	bad := []string{"", "ftp://x", "http://", "http://user:pw@host", "http://host/?q=1", "http://host/#f", "::"}
	for _, ep := range bad {
		if _, err := flue.New(flue.Config{Endpoint: ep, WorkflowVersion: "v"}); err == nil {
			t.Errorf("endpoint %q accepted", ep)
		}
	}
	if _, err := flue.New(flue.Config{Endpoint: "http://127.0.0.1:1", WorkflowVersion: " "}); err == nil {
		t.Error("empty version accepted")
	}
	a, err := flue.New(flue.Config{Endpoint: "http://127.0.0.1:1/base", WorkflowVersion: "v1"})
	if err != nil || a.Engine() != flue.Engine || a.WorkflowVersion() != "v1" {
		t.Fatalf("New = %+v %v", a, err)
	}
	if _, err := flue.Factory(map[string]string{}); err == nil {
		t.Error("Factory without settings succeeded")
	}
	if got, err := flue.Factory(map[string]string{flue.SettingEndpoint: "http://127.0.0.1:1", flue.SettingWorkflowVersion: "v1"}); err != nil || got.Engine() != flue.Engine {
		t.Fatalf("Factory = %v %v", got, err)
	}
	if _, err := flue.BuildBundle(extwork.Admission{}, "s", "r", map[string]string{"../x": "y"}, nil); !errors.Is(err, extwork.ErrReceiptPath) {
		t.Fatalf("bundle with escaping file path = %v", err)
	}
}

// fakeRuntime is a scripted HTTP stand-in for exercising the adapter's error
// mapping without the fixture process.
type fakeRuntime struct {
	info      string
	dispatch  int
	dispBody  string
	lookup    int
	lookBody  string
	abort     int
	abortBody string
	artifact  int
}

func (f *fakeRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/":
		_, _ = io.WriteString(w, f.info)
	case r.URL.Path == "/dispatch":
		w.WriteHeader(f.dispatch)
		_, _ = io.WriteString(w, f.dispBody)
	case r.URL.Path == "/submissions":
		w.WriteHeader(f.lookup)
		_, _ = io.WriteString(w, f.lookBody)
	case strings.HasSuffix(r.URL.Path, "/abort"):
		w.WriteHeader(f.abort)
		_, _ = io.WriteString(w, f.abortBody)
	case strings.Contains(r.URL.Path, "/artifacts/"):
		w.WriteHeader(f.artifact)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestAdapterErrorMapping(t *testing.T) {
	ctx := context.Background()
	rt := &fakeRuntime{info: `{"engine":"flue","version":"` + workflowVersion + `","incarnation":"inc-1"}`, dispatch: http.StatusOK, dispBody: `{"submission_id":"s1","uid":"inc-1","deduplicated":false}`, lookup: http.StatusOK, lookBody: `{"id":"s1","uid":"inc-1","state":"odd","stage":"x"}`, abort: http.StatusOK, abortBody: `{"requested":true,"acknowledged":false,"stopped":false,"detail":"queued"}`, artifact: http.StatusOK}
	srv := httptest.NewServer(rt)
	defer srv.Close()
	a, err := flue.New(flue.Config{Endpoint: srv.URL, WorkflowVersion: workflowVersion})
	if err != nil {
		t.Fatal(err)
	}
	adm, payload := admissionFor(t, "w1", "task-1", 1, nil)
	req := extwork.StartRequest{Admission: adm, Payload: payload}

	invalid := adm
	invalid.WorkKey = ""
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: invalid, Payload: payload}); !errors.Is(err, extwork.ErrInvalidAdmission) {
		t.Errorf("invalid admission = %v", err)
	}
	noCap := adm
	noCap.Authority.Capability = "run-stage"
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: noCap, Payload: payload}); !errors.Is(err, extwork.ErrRefused) {
		t.Errorf("missing capability = %v", err)
	}
	wrongEngine := adm
	wrongEngine.Engine = "spektacular"
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: wrongEngine, Payload: payload}); !errors.Is(err, extwork.ErrRefused) {
		t.Errorf("wrong engine = %v", err)
	}
	wrongVersion := adm
	wrongVersion.WorkflowVersion = "flue-fixture/9"
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: wrongVersion, Payload: payload}); !errors.Is(err, extwork.ErrRefused) {
		t.Errorf("wrong version = %v", err)
	}
	if _, err := a.Start(ctx, extwork.StartRequest{Admission: adm, Payload: []byte("other")}); !errors.Is(err, extwork.ErrPayloadDigest) {
		t.Errorf("payload digest = %v", err)
	}
	if inc, err := a.Incarnation(ctx); err != nil || inc != "inc-1" {
		t.Errorf("Incarnation = %q %v", inc, err)
	}
	res, err := a.Start(ctx, req)
	if err != nil || res.RemoteRunID != "s1" || res.RemoteIncarnation != "inc-1" {
		t.Fatalf("Start = %+v %v", res, err)
	}
	rt.dispatch, rt.dispBody = http.StatusConflict, `{"type":"submission_conflict"}`
	if _, err := a.Start(ctx, req); !errors.Is(err, extwork.ErrConflict) {
		t.Errorf("conflict = %v", err)
	}
	rt.dispatch, rt.dispBody = http.StatusForbidden, `{"type":"policy"}`
	if _, err := a.Start(ctx, req); !errors.Is(err, extwork.ErrRefused) {
		t.Errorf("forbidden = %v", err)
	}
	rt.dispatch, rt.dispBody = http.StatusOK, `{"submission_id":"","uid":""}`
	if _, err := a.Start(ctx, req); !errors.Is(err, extwork.ErrRefused) {
		t.Errorf("empty reply = %v", err)
	}
	rt.dispatch, rt.dispBody = http.StatusOK, `not json`
	if _, err := a.Start(ctx, req); !errors.Is(err, extwork.ErrTransport) {
		t.Errorf("undecodable reply = %v", err)
	}
	rt.info = `{"engine":"flue","version":"other","incarnation":"inc-1"}`
	if _, err := a.Start(ctx, req); !errors.Is(err, extwork.ErrRefused) {
		t.Errorf("runtime version drift = %v", err)
	}
	rt.info = `{"engine":"flue","version":"` + workflowVersion + `","incarnation":"inc-1"}`

	// Observe: unknown native state maps to unknown; incarnation is enforced.
	obs, err := a.Observe(ctx, adm.ExecutionKey(), "inc-1")
	if err != nil || obs.State != extwork.StateUnknown || !strings.Contains(obs.Detail, "odd") {
		t.Errorf("Observe odd state = %+v %v", obs, err)
	}
	if _, err := a.Observe(ctx, adm.ExecutionKey(), "inc-2"); !errors.Is(err, extwork.ErrIncarnationMismatch) {
		t.Errorf("incarnation mismatch = %v", err)
	}
	rt.lookup = http.StatusNotFound
	if _, err := a.Observe(ctx, adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrNotFound) {
		t.Errorf("not found = %v", err)
	}
	rt.lookup = http.StatusBadGateway
	if _, err := a.Observe(ctx, adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrTransport) {
		t.Errorf("bad gateway = %v", err)
	}
	if _, err := a.Cancel(ctx, adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrTransport) {
		t.Errorf("cancel lookup failure = %v", err)
	}
	if _, err := a.OpenArtifact(ctx, adm.ExecutionKey(), "", "receipt.json"); !errors.Is(err, extwork.ErrTransport) {
		t.Errorf("open lookup failure = %v", err)
	}
	rt.lookup = http.StatusOK

	// Cancel: facts pass through; an abort transport failure stays "requested" only.
	facts, err := a.Cancel(ctx, adm.ExecutionKey(), "")
	if err != nil || !facts.Requested || facts.Acknowledged || facts.Stopped || facts.Detail != "queued" {
		t.Errorf("Cancel = %+v %v", facts, err)
	}
	rt.abort = http.StatusInternalServerError
	facts, err = a.Cancel(ctx, adm.ExecutionKey(), "")
	if err == nil || !facts.Requested || facts.Stopped {
		t.Errorf("Cancel failure = %+v %v", facts, err)
	}

	// OpenArtifact: path rules first, then engine replies.
	if _, err := a.OpenArtifact(ctx, adm.ExecutionKey(), "", "../receipt.json"); !errors.Is(err, extwork.ErrReceiptPath) {
		t.Errorf("escaping path = %v", err)
	}
	rc, err := a.OpenArtifact(ctx, adm.ExecutionKey(), "", "out/receipt.json")
	if err != nil {
		t.Fatalf("OpenArtifact = %v", err)
	}
	_ = rc.Close()
	rt.artifact = http.StatusNotFound
	if _, err := a.OpenArtifact(ctx, adm.ExecutionKey(), "", "receipt.json"); !errors.Is(err, extwork.ErrNotFound) {
		t.Errorf("missing artifact = %v", err)
	}
	rt.artifact = http.StatusTeapot
	if _, err := a.OpenArtifact(ctx, adm.ExecutionKey(), "", "receipt.json"); !errors.Is(err, extwork.ErrTransport) {
		t.Errorf("odd artifact status = %v", err)
	}

	// Transport failure once the runtime is gone.
	srv.Close()
	if _, err := a.Start(ctx, req); !errors.Is(err, extwork.ErrTransport) {
		t.Errorf("dead runtime Start = %v", err)
	}
	if _, err := a.Observe(ctx, adm.ExecutionKey(), ""); !errors.Is(err, extwork.ErrTransport) {
		t.Errorf("dead runtime Observe = %v", err)
	}
	if _, err := a.OpenArtifact(ctx, adm.ExecutionKey(), "", "receipt.json"); !errors.Is(err, extwork.ErrTransport) {
		t.Errorf("dead runtime OpenArtifact = %v", err)
	}
}

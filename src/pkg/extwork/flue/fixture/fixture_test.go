package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

const workflowDir = "../testdata/flue-fixture"

func bundle(t *testing.T, workKey, hint string) []byte {
	t.Helper()
	p := map[string]any{
		"admission": map[string]any{"work_key": workKey, "assignment_id": "task-" + workKey, "generation": 3, "stage": "implement", "contract_revision": "c1"},
		"summary":   "fixture bundle",
		"repo":      "hivecommons/flue-fixture",
	}
	if hint != "" {
		p["hints"] = map[string]string{HintResultClass: hint}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type denyProxy struct {
	mu   sync.Mutex
	seen []string
}

func (d *denyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.seen = append(d.seen, r.Method+" "+r.Host)
	d.mu.Unlock()
	w.WriteHeader(http.StatusForbidden)
}

func (d *denyProxy) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

func TestLoadWorkflowErrors(t *testing.T) {
	if _, err := LoadWorkflow(t.TempDir()); err == nil {
		t.Fatal("missing workflow accepted")
	}
	dir := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, WorkflowFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("{")
	if _, err := LoadWorkflow(dir); err == nil {
		t.Fatal("unparseable workflow accepted")
	}
	write(`{"engine":"other","version":"1","input_revision":"x","stages":[{"name":"a"}]}`)
	if _, err := LoadWorkflow(dir); err == nil {
		t.Fatal("wrong engine accepted")
	}
	write(`{"engine":"flue","version":"1","input_revision":"x","stages":[{"name":"a","reads":["missing.txt"]}]}`)
	if _, err := LoadWorkflow(dir); err == nil || !strings.Contains(err.Error(), "missing file") {
		t.Fatalf("missing read accepted: %v", err)
	}
	write(`{"engine":"flue","version":"1","input_revision":"x","stages":[{"name":"a","artifacts":[{"path":"r","source":"nope"}]}]}`)
	if _, err := LoadWorkflow(dir); err == nil || !strings.Contains(err.Error(), "artifact source") {
		t.Fatalf("missing artifact source accepted: %v", err)
	}
	if _, err := Start(Options{WorkflowDir: dir}); err == nil {
		t.Fatal("Start with a broken workflow succeeded")
	}
	if _, err := Start(Options{WorkflowDir: workflowDir, EgressProxy: "::bad"}); err == nil {
		t.Fatal("bad egress proxy accepted")
	}
	if _, err := Start(Options{WorkflowDir: workflowDir, Listen: "256.256.256.256:1"}); err == nil {
		t.Fatal("bad listen address accepted")
	}
}

func TestThreeStageRunAndEffects(t *testing.T) {
	proxy := &denyProxy{}
	ps := httptest.NewServer(proxy)
	defer ps.Close()
	t.Setenv("GITHUB_TOKEN", "")

	stateDir := t.TempDir()
	s, err := Start(Options{WorkflowDir: workflowDir, StateDir: stateDir, EgressProxy: ps.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if !strings.HasPrefix(s.Addr(), "http://127.0.0.1:") || s.Incarnation() == "" {
		t.Fatalf("Addr/Incarnation = %q %q", s.Addr(), s.Incarnation())
	}

	if _, _, err := s.Dispatch("", bundle(t, "w1", "")); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, _, err := s.Dispatch("k", bytes.Repeat([]byte("x"), maxPayloadBytes+1)); err == nil {
		t.Fatal("oversized payload accepted")
	}
	if _, _, err := s.Dispatch("k", []byte("not json")); err == nil {
		t.Fatal("non-bundle payload accepted")
	}
	sub, dedup, err := s.Dispatch("k", bundle(t, "w1", ""))
	if err != nil || dedup || sub.State != StateAccepted || sub.UID != s.Incarnation() {
		t.Fatalf("Dispatch = %+v %v %v", sub, dedup, err)
	}
	again, dedup, err := s.Dispatch("k", bundle(t, "w1", ""))
	if err != nil || !dedup || again.ID != sub.ID {
		t.Fatalf("dedup Dispatch = %+v %v %v", again, dedup, err)
	}
	if _, _, err := s.Dispatch("k", bundle(t, "w2", "")); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed payload = %v", err)
	}
	if st := s.Stats(); st.Dispatches != 3 || st.Runs != 1 || st.GitHubTokenPresent {
		t.Fatalf("Stats = %+v", st)
	}

	for i, wantStage := range []string{"analyze", "instrument", "report"} {
		if err := s.Tick(); err != nil {
			t.Fatal(err)
		}
		if sub.Stage != wantStage {
			t.Fatalf("tick %d: stage %q, want %q", i+1, sub.Stage, wantStage)
		}
	}
	if sub.State != StateTerminal || sub.ResultClass != string(outputschema.ReceiptResultCompleted) || sub.Receipt == nil {
		t.Fatalf("terminal submission = %+v", sub)
	}
	report, err := outputschema.Validate(sub.Artifacts[ReceiptArtifact])
	if err != nil {
		t.Fatalf("receipt invalid: %v", err)
	}
	if len(report.Receipt.Artifacts) != 2 || report.Receipt.ExecutionKey != "k" || report.Receipt.RemoteRunID != sub.ID || report.Receipt.RemoteIncarnation != sub.UID {
		t.Fatalf("receipt = %+v", report.Receipt)
	}
	if _, ok := sub.Artifacts["patch.diff"]; !ok {
		t.Fatal("patch artifact missing")
	}
	st := s.Stats()
	if len(st.EffectAttempts) != 2 {
		t.Fatalf("effect attempts = %+v", st.EffectAttempts)
	}
	for _, a := range st.EffectAttempts {
		if a.Outcome != EffectRefused {
			t.Errorf("effect %s to %s was %s, want refused", a.Kind, a.Target, a.Outcome)
		}
	}
	if seen := proxy.requests(); len(seen) != 2 || !strings.HasPrefix(seen[0], "CONNECT pkg.pr.new") || !strings.HasPrefix(seen[1], "CONNECT api.github.com") {
		t.Fatalf("proxy saw %v", seen)
	}
	// A further tick leaves a terminal run alone.
	if err := s.Tick(); err != nil || sub.EndedTick != 3 {
		t.Fatalf("post-terminal tick: %v ended=%d", err, sub.EndedTick)
	}
	if _, _, _, detail, err := s.Abort(sub.ID); err != nil || detail != "already terminal" {
		t.Fatalf("abort terminal = %q %v", detail, err)
	}
	if err := s.SetWaiting(sub.ID, true); err == nil {
		t.Fatal("waiting on a terminal submission accepted")
	}

	// Restart from the state file: same incarnation, same submissions.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := Start(Options{WorkflowDir: workflowDir, StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumed.Close() }()
	if resumed.Incarnation() != s.Incarnation() || resumed.Stats().Runs != 1 || resumed.Stats().Tick != 4 {
		t.Fatalf("resume lost state: %+v", resumed.Stats())
	}
	// A pinned different incarnation is a recreated engine: fresh state.
	_ = resumed.Close()
	fresh, err := Start(Options{WorkflowDir: workflowDir, StateDir: stateDir, Incarnation: "recreated"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	if fresh.Incarnation() != "recreated" || fresh.Stats().Runs != 0 {
		t.Fatalf("recreated engine kept old runs: %+v", fresh.Stats())
	}
	// Corrupt state is an error, not a silent reset.
	if err := os.WriteFile(filepath.Join(stateDir, StateFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(Options{WorkflowDir: workflowDir, StateDir: stateDir}); err == nil {
		t.Fatal("corrupt state accepted")
	}
}

func TestNoRouteAndAbortAndWaiting(t *testing.T) {
	s, err := Start(Options{WorkflowDir: workflowDir, IgnoreAbort: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	// Force "no proxy" regardless of the host environment.
	s.proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	sub, _, err := s.Dispatch("k", bundle(t, "w1", "no_change"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.Abort("nope"); err == nil {
		t.Fatal("abort of unknown submission succeeded")
	}
	if err := s.SetWaiting("nope", true); err == nil {
		t.Fatal("waiting on unknown submission succeeded")
	}
	requested, acknowledged, stopped, detail, err := s.Abort(sub.ID)
	if err != nil || !requested || !acknowledged || stopped || detail != "workload ignored abort" {
		t.Fatalf("ignored abort = %v %v %v %q %v", requested, acknowledged, stopped, detail, err)
	}
	if err := s.SetWaiting(sub.ID, true); err != nil || sub.State != StateWaiting {
		t.Fatalf("SetWaiting: %v state=%s", err, sub.State)
	}
	if err := s.Tick(); err != nil || sub.StageIndex != -1 {
		t.Fatalf("waiting submission advanced: %v idx=%d", err, sub.StageIndex)
	}
	if err := s.SetWaiting(sub.ID, false); err != nil || sub.State != StateRunning {
		t.Fatalf("resume: %v state=%s", err, sub.State)
	}
	for range 3 {
		if err := s.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	if sub.State != StateTerminal || sub.ResultClass != string(outputschema.ReceiptResultNoChange) {
		t.Fatalf("no_change run = %+v", sub)
	}
	report, err := outputschema.Validate(sub.Artifacts[ReceiptArtifact])
	if err != nil || len(report.Receipt.Artifacts) != 0 {
		t.Fatalf("no_change receipt = %+v %v", report, err)
	}
	for _, a := range s.Stats().EffectAttempts {
		if a.Outcome != EffectNoRoute {
			t.Errorf("without a route effect %s was %s", a.Kind, a.Outcome)
		}
	}

	// A stopping abort ends the run as failed.
	stopper, err := Start(Options{WorkflowDir: workflowDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stopper.Close() }()
	sub2, _, err := stopper.Dispatch("k2", bundle(t, "w2", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, stopped, _, err := stopper.Abort(sub2.ID); err != nil || !stopped || sub2.State != StateTerminal || sub2.ResultClass != string(outputschema.ReceiptResultFailed) {
		t.Fatalf("stopping abort = %v %v %+v", stopped, err, sub2)
	}
	if stopper.Stats().AbortRequests != 1 {
		t.Fatalf("abort count = %d", stopper.Stats().AbortRequests)
	}
}

func TestHTTPSurface(t *testing.T) {
	s, err := Start(Options{WorkflowDir: workflowDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	s.proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	client := &http.Client{}
	do := func(method, path string, body []byte) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, s.Addr()+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	if code, raw := do(http.MethodGet, "/", nil); code != http.StatusOK || !strings.Contains(string(raw), `"engine":"flue"`) {
		t.Fatalf("info = %d %s", code, raw)
	}
	if code, _ := do(http.MethodGet, "/nothing", nil); code != http.StatusNotFound {
		t.Fatalf("unknown path = %d", code)
	}
	if code, _ := do(http.MethodPost, "/dispatch", []byte("{")); code != http.StatusBadRequest {
		t.Fatalf("bad dispatch body = %d", code)
	}
	if code, _ := do(http.MethodPost, "/dispatch", []byte(`{"idempotency_key":"","payload":""}`)); code != http.StatusBadRequest {
		t.Fatalf("empty key = %d", code)
	}
	dispatch := func(key, workKey string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]any{"idempotency_key": key, "payload": bundle(t, workKey, "")})
		code, raw := do(http.MethodPost, "/dispatch", body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return code, out
	}
	code, first := dispatch("key-1", "w1")
	if code != http.StatusOK || first["submission_id"] == "" || first["deduplicated"] != false {
		t.Fatalf("dispatch = %d %v", code, first)
	}
	if code, second := dispatch("key-1", "w1"); code != http.StatusOK || second["deduplicated"] != true {
		t.Fatalf("dedup dispatch = %d %v", code, second)
	}
	if code, conflict := dispatch("key-1", "w2"); code != http.StatusConflict || conflict["type"] != "submission_conflict" {
		t.Fatalf("conflict = %d %v", code, conflict)
	}
	id, _ := first["submission_id"].(string)
	if code, _ := do(http.MethodGet, "/submissions?key=key-1", nil); code != http.StatusOK {
		t.Fatalf("by key = %d", code)
	}
	if code, _ := do(http.MethodGet, "/submissions?key=other", nil); code != http.StatusNotFound {
		t.Fatalf("unknown key = %d", code)
	}
	if code, raw := do(http.MethodGet, "/submissions/"+id, nil); code != http.StatusOK || !strings.Contains(string(raw), `"state":"accepted"`) {
		t.Fatalf("by id = %d %s", code, raw)
	}
	if code, _ := do(http.MethodGet, "/submissions/nope", nil); code != http.StatusNotFound {
		t.Fatalf("unknown id = %d", code)
	}
	if code, _ := do(http.MethodPost, "/control/wait/"+id, nil); code != http.StatusOK {
		t.Fatalf("wait = %d", code)
	}
	if code, _ := do(http.MethodPost, "/control/wait/nope", nil); code != http.StatusNotFound {
		t.Fatalf("wait unknown = %d", code)
	}
	if code, _ := do(http.MethodPost, "/control/resume/"+id, nil); code != http.StatusOK {
		t.Fatalf("resume = %d", code)
	}
	for range 3 {
		if code, _ := do(http.MethodPost, "/control/tick", nil); code != http.StatusOK {
			t.Fatalf("tick = %d", code)
		}
	}
	if code, raw := do(http.MethodGet, "/submissions/"+id, nil); code != http.StatusOK || !strings.Contains(string(raw), `"state":"terminal"`) || !strings.Contains(string(raw), `"receipt":{`) {
		t.Fatalf("terminal view = %d %s", code, raw)
	}
	if code, raw := do(http.MethodGet, "/submissions/"+id+"/artifacts/receipt.json", nil); code != http.StatusOK || !strings.Contains(string(raw), "stage_receipt") {
		t.Fatalf("artifact = %d %s", code, raw)
	}
	if code, _ := do(http.MethodGet, "/submissions/"+id+"/artifacts/missing.txt", nil); code != http.StatusNotFound {
		t.Fatalf("missing artifact = %d", code)
	}
	if code, _ := do(http.MethodGet, "/submissions/nope/artifacts/receipt.json", nil); code != http.StatusNotFound {
		t.Fatalf("artifact of unknown = %d", code)
	}
	if code, raw := do(http.MethodPost, "/submissions/"+id+"/abort", nil); code != http.StatusOK || !strings.Contains(string(raw), `"requested":true`) {
		t.Fatalf("abort = %d %s", code, raw)
	}
	if code, _ := do(http.MethodPost, "/submissions/nope/abort", nil); code != http.StatusNotFound {
		t.Fatalf("abort unknown = %d", code)
	}
	if code, raw := do(http.MethodGet, "/control/stats", nil); code != http.StatusOK || !strings.Contains(string(raw), `"dispatches":3`) {
		t.Fatalf("stats = %d %s", code, raw)
	}
	// Tick failure surfaces as 500: remove a read file from a private copy.
	broken := copyWorkflow(t)
	bs, err := Start(Options{WorkflowDir: broken})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bs.Close() }()
	if _, _, err := bs.Dispatch("k", bundle(t, "w", "")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(broken, "source", "README.md")); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, bs.Addr()+"/control/tick", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("broken tick = %d", resp.StatusCode)
	}
}

func copyWorkflow(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(workflowDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(workflowDir, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestRunAndBuildReceiptErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"-bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("bad flag exit = %d", code)
	}
	if code := Run(context.Background(), []string{"-workflow", t.TempDir()}, &out, &errOut); code != 1 {
		t.Fatalf("broken workflow exit = %d", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := Run(ctx, []string{"-workflow", workflowDir, "-egress-proxy", "http://127.0.0.1:1", "-ignore-abort", "-incarnation", "pinned"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), AddrLinePrefix) {
		t.Fatalf("Run = %d %q %q", code, out.String(), errOut.String())
	}
}

// allowProxy is an egress route that lets plain-HTTP effects through, so a
// class (a) leak shows up as "delivered" in stats instead of hiding.
type allowProxy struct{}

func (allowProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func TestDeliveredEffectIsVisible(t *testing.T) {
	ps := httptest.NewServer(allowProxy{})
	defer ps.Close()
	dir := copyWorkflow(t)
	wf, err := LoadWorkflow(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range wf.Stages {
		for j := range wf.Stages[i].Effects {
			wf.Stages[i].Effects[j].Target = "http://example.invalid/leak"
		}
	}
	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, WorkflowFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Start(Options{WorkflowDir: dir, EgressProxy: ps.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, _, err := s.Dispatch("k", bundle(t, "w", "")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	attempts := s.Stats().EffectAttempts
	if len(attempts) != 2 {
		t.Fatalf("attempts = %+v", attempts)
	}
	for _, a := range attempts {
		if a.Outcome != EffectDelivered {
			t.Errorf("effect %s through a permissive route was %s, want delivered (a leak must be visible)", a.Kind, a.Outcome)
		}
	}
	// A malformed target is refused before any request is built.
	bad := s.attemptEffect("sub", Effect{Kind: EffectOutboundPost, Target: "http://[::1"})
	if bad.Outcome != EffectRefused {
		t.Fatalf("malformed target = %+v", bad)
	}
}

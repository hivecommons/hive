// Package fixture is a deterministic stand-in for an OMP workbench, used by
// the #8361 step 9 conformance tests. It speaks the relay-channel protocol of
// pkg/extwork/omp exactly as a workbench would: it dials the hub, declares
// its capabilities, answers offers, accepts a bounded bundle only after it
// accepted the offer, emits progress events, publishes a stage receipt, and
// can disconnect mid-stage or ignore a cancel on request.
//
// It runs as a second local process (Run) or in-process (Start). It needs no
// OMP binary, no model, no GitHub token, and no network beyond loopback.
// Stages advance only when ticked through its control endpoint, so every run
// is reproducible. The workflow it executes is read from a directory such as
// testdata/omp-fixture/.
package fixture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/omp"
	"github.com/hivecommons/hive/pkg/outputschema"
)

const (
	// EngineName is what the fixture advertises as its engine.
	EngineName = omp.Engine
	// WorkflowFile is the workflow definition inside a workflow directory.
	WorkflowFile = "workbench.json"
	// DefaultDeclineMarker in an offer summary makes the auto policy decline.
	DefaultDeclineMarker = "[decline]"
	// HintResultClass lets a bundle steer the terminal result class.
	HintResultClass = "result_class"
	// AddrLinePrefix is what Run prints before the control base URL.
	AddrLinePrefix = "OMP_FIXTURE_ADDR "

	dialTimeout   = 5 * time.Second
	helloTimeout  = 5 * time.Second
	closeGrace    = 2 * time.Second
	tickMinutes   = 1
	runIDPrefix   = "omp-run-"
	envGitHub     = "GITHUB_TOKEN"
	envDashboard  = "HIVE_DASHBOARD_TOKEN"
	controlPrefix = "/control/"
)

// receiptEpoch anchors the deterministic timestamps.
var receiptEpoch = time.Date(2026, time.September, 23, 0, 0, 0, 0, time.UTC)

// Workflow is the definition read from WorkflowFile.
type Workflow struct {
	Engine        string  `json:"engine"`
	Version       string  `json:"version"`
	InputRevision string  `json:"input_revision"`
	Stages        []Stage `json:"stages"`
}

// Stage is one workbench stage and the artifacts it emits.
type Stage struct {
	Name      string         `json:"name"`
	Artifacts []ArtifactSpec `json:"artifacts,omitempty"`
}

// ArtifactSpec maps an output file in the workflow directory to an artifact
// path in the receipt.
type ArtifactSpec struct {
	Path        string `json:"path"`
	Description string `json:"description"`
	Source      string `json:"source"`
}

// LoadWorkflow reads and validates a workflow directory.
func LoadWorkflow(dir string) (Workflow, error) {
	raw, err := os.ReadFile(filepath.Join(dir, WorkflowFile))
	if err != nil {
		return Workflow{}, err
	}
	var wf Workflow
	if err := json.Unmarshal(raw, &wf); err != nil {
		return Workflow{}, fmt.Errorf("%s: %w", WorkflowFile, err)
	}
	if wf.Engine != EngineName {
		return Workflow{}, fmt.Errorf("%s: engine %q is not %s", WorkflowFile, wf.Engine, EngineName)
	}
	if wf.Version == "" || wf.InputRevision == "" || len(wf.Stages) == 0 {
		return Workflow{}, fmt.Errorf("%s: version, input_revision, and at least one stage are required", WorkflowFile)
	}
	for _, st := range wf.Stages {
		for _, art := range st.Artifacts {
			if _, err := os.Stat(filepath.Join(dir, art.Source)); err != nil {
				return Workflow{}, fmt.Errorf("stage %s: missing artifact source %s", st.Name, art.Source)
			}
		}
	}
	return wf, nil
}

// Stats is what the fixture recorded, read through /control/stats.
type Stats struct {
	Connected                bool  `json:"connected"`
	Offers                   int   `json:"offers"`
	Accepts                  int   `json:"accepts"`
	Declines                 int   `json:"declines"`
	Starts                   int   `json:"starts"`
	Runs                     int   `json:"runs"`
	UnsolicitedStarts        int   `json:"unsolicited_starts"`
	PayloadBytesBeforeAccept int64 `json:"payload_bytes_before_accept"`
	Cancels                  int   `json:"cancels"`
	Receipts                 int   `json:"receipts"`
	Tick                     int   `json:"tick"`
	GitHubTokenPresent       bool  `json:"github_token_present"`
	DashboardTokenPresent    bool  `json:"dashboard_token_present"`
}

// Options configure a workbench.
type Options struct {
	HubURL        string
	Identity      string
	WorkflowDir   string
	Incarnation   string
	Capabilities  []string
	Interactive   bool
	IgnoreCancel  bool
	DeclineMarker string
	ControlListen string
}

type payload struct {
	Admission omp.BundleAdmission `json:"admission"`
	Summary   string              `json:"summary"`
	Repo      string              `json:"repo"`
	Hints     map[string]string   `json:"hints,omitempty"`
}

type wbRun struct {
	key         string
	id          string
	gen         uint64
	digest      string
	payload     payload
	stageIdx    int
	terminal    bool
	waiting     bool
	startedTick int
	endedTick   int
}

// Workbench is a running fixture.
type Workbench struct {
	opts  Options
	wf    Workflow
	link  *omp.WSLink
	ctrl  *http.Server
	ln    net.Listener
	ctx   context.Context
	stop  context.CancelFunc
	done  chan struct{}
	seq   int
	sendM sync.Mutex

	mu       sync.Mutex
	stats    Stats
	accepted map[string]bool
	pending  map[string]omp.Message
	runs     map[string]*wbRun
	nextRun  int
}

// Start dials the hub, attaches, and serves the control endpoint.
func Start(opts Options) (*Workbench, error) {
	if opts.HubURL == "" || opts.Identity == "" || opts.WorkflowDir == "" {
		return nil, errors.New("hub url, identity, and workflow dir are required")
	}
	wf, err := LoadWorkflow(opts.WorkflowDir)
	if err != nil {
		return nil, err
	}
	if opts.Capabilities == nil {
		opts.Capabilities = []string{omp.Capability}
	}
	if opts.DeclineMarker == "" {
		opts.DeclineMarker = DefaultDeclineMarker
	}
	if opts.ControlListen == "" {
		opts.ControlListen = omp.DefaultListen
	}
	if opts.Incarnation == "" {
		opts.Incarnation = randomHex()
	}
	ctx, stop := context.WithCancel(context.Background())
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, opts.HubURL, nil)
	cancel()
	if err != nil {
		stop()
		return nil, fmt.Errorf("dial hub: %w", err)
	}
	w := &Workbench{opts: opts, wf: wf, link: omp.NewWSLink(conn), ctx: ctx, stop: stop, done: make(chan struct{}), accepted: map[string]bool{}, pending: map[string]omp.Message{}, runs: map[string]*wbRun{}}
	w.stats.GitHubTokenPresent = os.Getenv(envGitHub) != ""
	w.stats.DashboardTokenPresent = os.Getenv(envDashboard) != ""
	hello := omp.Message{Type: omp.MsgHello, ContributorID: opts.Identity, Capabilities: &omp.Capabilities{RelayCapabilities: opts.Capabilities}, WorkbenchVersion: wf.Version, Incarnation: opts.Incarnation}
	if err := w.send(hello); err != nil {
		w.Close()
		return nil, err
	}
	helloCtx, cancelHello := context.WithTimeout(ctx, helloTimeout)
	reply, err := w.link.Recv(helloCtx)
	cancelHello()
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("hello: %w", err)
	}
	if reply.Type != omp.MsgHelloOK {
		w.Close()
		return nil, fmt.Errorf("%w: hub refused attachment: %s", extwork.ErrRefused, reply.Reason)
	}
	w.stats.Connected = true
	ln, err := net.Listen("tcp", opts.ControlListen)
	if err != nil {
		w.Close()
		return nil, err
	}
	w.ln = ln
	w.ctrl = &http.Server{Handler: w.controlHandler(), ReadHeaderTimeout: helloTimeout}
	go func() { _ = w.ctrl.Serve(ln) }()
	go w.readLoop()
	return w, nil
}

// ControlAddr is the base URL of the control endpoint.
func (w *Workbench) ControlAddr() string { return "http://" + w.ln.Addr().String() }

// Incarnation is the session identity declared on hello.
func (w *Workbench) Incarnation() string { return w.opts.Incarnation }

// Close disconnects from the hub and stops the control endpoint.
func (w *Workbench) Close() {
	w.stop()
	_ = w.link.Close()
	if w.ctrl != nil {
		ctx, cancel := context.WithTimeout(context.Background(), closeGrace)
		defer cancel()
		_ = w.ctrl.Shutdown(ctx)
	}
}

// Disconnect drops the hub link abruptly, mid-stage, keeping the control
// endpoint and every run in memory.
func (w *Workbench) Disconnect() {
	_ = w.link.Close()
	w.mu.Lock()
	w.stats.Connected = false
	w.mu.Unlock()
}

// Done is closed when the read loop ends.
func (w *Workbench) Done() <-chan struct{} { return w.done }

func randomHex() string {
	sum := sha256.Sum256([]byte(time.Now().String()))
	return hex.EncodeToString(sum[:4])
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (w *Workbench) send(msg omp.Message) error {
	w.sendM.Lock()
	defer w.sendM.Unlock()
	w.seq++
	msg.Seq = w.seq
	return w.link.Send(w.ctx, msg)
}

func (w *Workbench) readLoop() {
	defer close(w.done)
	for {
		msg, err := w.link.Recv(w.ctx)
		if err != nil {
			w.mu.Lock()
			w.stats.Connected = false
			w.mu.Unlock()
			return
		}
		w.handle(msg)
	}
}

func (w *Workbench) handle(msg omp.Message) {
	switch msg.Type {
	case omp.MsgOffer:
		w.mu.Lock()
		w.stats.Offers++
		if w.opts.Interactive {
			w.pending[msg.ExecutionKey] = msg
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()
		if strings.Contains(msg.Summary, w.opts.DeclineMarker) {
			w.answer(msg.ExecutionKey, false, "declined by workbench policy")
			return
		}
		w.answer(msg.ExecutionKey, true, "accepted by workbench policy")
	case omp.MsgStart:
		w.handleStart(msg)
	case omp.MsgCancel:
		w.handleCancel(msg)
	}
}

// answer records and sends the decision for an execution key.
func (w *Workbench) answer(key string, accept bool, reason string) {
	w.mu.Lock()
	delete(w.pending, key)
	if accept {
		w.accepted[key] = true
		w.stats.Accepts++
	} else {
		w.stats.Declines++
	}
	w.mu.Unlock()
	typ := omp.MsgDecline
	if accept {
		typ = omp.MsgAccept
	}
	_ = w.send(omp.Message{Type: typ, ExecutionKey: key, Reason: reason})
}

func (w *Workbench) handleStart(msg omp.Message) {
	w.mu.Lock()
	w.stats.Starts++
	if !w.accepted[msg.ExecutionKey] {
		// The invariant the hub must uphold: no context before acceptance.
		// A bundle that arrives anyway is counted, never executed.
		w.stats.UnsolicitedStarts++
		w.stats.PayloadBytesBeforeAccept += int64(len(msg.Payload))
		w.mu.Unlock()
		_ = w.send(omp.Message{Type: omp.MsgStartRefused, ExecutionKey: msg.ExecutionKey, Reason: omp.ReasonNotAccepted})
		return
	}
	digest := digestOf(msg.Payload)
	if r, ok := w.runs[msg.ExecutionKey]; ok {
		if r.digest != digest {
			w.mu.Unlock()
			_ = w.send(omp.Message{Type: omp.MsgStartRefused, ExecutionKey: msg.ExecutionKey, Reason: omp.ReasonConflict})
			return
		}
		w.mu.Unlock()
		_ = w.send(omp.Message{Type: omp.MsgStarted, ExecutionKey: msg.ExecutionKey, RemoteRunID: r.id, Deduplicated: true})
		return
	}
	var p payload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		w.mu.Unlock()
		_ = w.send(omp.Message{Type: omp.MsgStartRefused, ExecutionKey: msg.ExecutionKey, Reason: "bundle: " + err.Error()})
		return
	}
	w.nextRun++
	r := &wbRun{key: msg.ExecutionKey, id: fmt.Sprintf("%s%d", runIDPrefix, w.nextRun), gen: msg.TaskGen, digest: digest, payload: p, startedTick: w.stats.Tick}
	w.runs[msg.ExecutionKey] = r
	w.stats.Runs++
	stage := w.stageOfLocked(r)
	w.mu.Unlock()
	_ = w.send(omp.Message{Type: omp.MsgStarted, ExecutionKey: msg.ExecutionKey, RemoteRunID: r.id})
	_ = w.send(omp.Message{Type: omp.MsgProgress, ExecutionKey: msg.ExecutionKey, RemoteRunID: r.id, State: string(extwork.StateAccepted), Stage: stage, Detail: "queued on the workbench"})
}

// stageOfLocked names the workbench stage a run is in: the lease stage from
// the bundle before the first tick, then the workflow stage last entered.
// Every progress frame carries it so the hub's audit never records an empty
// stage. Callers hold w.mu.
func (w *Workbench) stageOfLocked(r *wbRun) string {
	if r.stageIdx == 0 {
		return r.payload.Admission.Stage
	}
	idx := r.stageIdx - 1
	if idx >= len(w.wf.Stages) {
		idx = len(w.wf.Stages) - 1
	}
	return w.wf.Stages[idx].Name
}

func (w *Workbench) handleCancel(msg omp.Message) {
	w.mu.Lock()
	w.stats.Cancels++
	r, ok := w.runs[msg.ExecutionKey]
	if !ok {
		w.mu.Unlock()
		_ = w.send(omp.Message{Type: omp.MsgCancelAck, ExecutionKey: msg.ExecutionKey, Detail: "no such run"})
		return
	}
	switch {
	case r.terminal:
		w.mu.Unlock()
		_ = w.send(omp.Message{Type: omp.MsgCancelAck, ExecutionKey: msg.ExecutionKey, Acknowledged: true, Detail: "already terminal"})
	case w.opts.IgnoreCancel:
		w.mu.Unlock()
		_ = w.send(omp.Message{Type: omp.MsgCancelAck, ExecutionKey: msg.ExecutionKey, Acknowledged: true, Detail: "workbench ignored cancel"})
	default:
		r.terminal = true
		r.endedTick = w.stats.Tick
		stage := w.stageOfLocked(r)
		w.mu.Unlock()
		_ = w.send(omp.Message{Type: omp.MsgCancelAck, ExecutionKey: msg.ExecutionKey, Acknowledged: true, Stopped: true, Detail: "stopped"})
		_ = w.send(omp.Message{Type: omp.MsgProgress, ExecutionKey: msg.ExecutionKey, RemoteRunID: r.id, State: string(extwork.StateTerminal), Stage: stage, Detail: "stopped on request"})
	}
}

// Tick advances every live run by one stage; a run past its last stage
// publishes its receipt and goes terminal.
func (w *Workbench) Tick() {
	w.mu.Lock()
	w.stats.Tick++
	var advance []*wbRun
	for _, r := range w.runs {
		if !r.terminal && !r.waiting {
			advance = append(advance, r)
		}
	}
	sort.Slice(advance, func(i, j int) bool { return advance[i].id < advance[j].id })
	w.mu.Unlock()
	for _, r := range advance {
		w.advance(r)
	}
}

func (w *Workbench) advance(r *wbRun) {
	w.mu.Lock()
	if r.stageIdx < len(w.wf.Stages) {
		name := w.wf.Stages[r.stageIdx].Name
		r.stageIdx++
		w.mu.Unlock()
		_ = w.send(omp.Message{Type: omp.MsgProgress, ExecutionKey: r.key, RemoteRunID: r.id, State: string(extwork.StateRunning), Stage: name})
		return
	}
	r.terminal = true
	r.endedTick = w.stats.Tick
	raw, err := w.buildReceiptLocked(r)
	w.stats.Receipts++
	stage := w.stageOfLocked(r)
	w.mu.Unlock()
	if err != nil {
		_ = w.send(omp.Message{Type: omp.MsgProgress, ExecutionKey: r.key, RemoteRunID: r.id, State: string(extwork.StateTerminal), Stage: stage, Detail: "receipt: " + err.Error()})
		return
	}
	_ = w.send(omp.Message{Type: omp.MsgReceipt, ExecutionKey: r.key, RemoteRunID: r.id, Stage: stage, Artifact: &omp.Artifact{Path: omp.ReceiptArtifact, Digest: digestOf(raw), Size: int64(len(raw)), Body: raw}})
	_ = w.send(omp.Message{Type: omp.MsgProgress, ExecutionKey: r.key, RemoteRunID: r.id, State: string(extwork.StateTerminal), Stage: stage})
}

// BuildReceipt renders the stage_receipt AgentReport for a finished run.
func BuildReceipt(wf Workflow, adm omp.BundleAdmission, repo, key, runID, incarnation string, class outputschema.StageReceiptResultClass, startedTick, endedTick int) ([]byte, error) {
	artifacts := []outputschema.Artifact{}
	if class == outputschema.ReceiptResultCompleted {
		for _, st := range wf.Stages {
			for _, art := range st.Artifacts {
				artifacts = append(artifacts, outputschema.Artifact{Repo: repo, Path: art.Path, Description: art.Description})
			}
		}
	}
	parts := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		parts = append(parts, strings.Join([]string{a.Repo, a.Path, a.Description}, "\x00"))
	}
	sort.Strings(parts)
	report := outputschema.AgentReport{
		Lane:       EngineName,
		Kind:       outputschema.KindStageReceipt,
		Findings:   []outputschema.Finding{},
		PRsOpened:  []outputschema.PROpened{},
		BeadsFiled: []outputschema.BeadFiled{},
		Summary:    "omp workbench " + string(class) + " for " + adm.WorkKey,
		Receipt: &outputschema.StageReceipt{
			SchemaVersion:     outputschema.StageReceiptSchemaVersion,
			WorkKey:           adm.WorkKey,
			AssignmentID:      adm.AssignmentID,
			Generation:        adm.Generation,
			Stage:             adm.Stage,
			ContractRevision:  adm.ContractRevision,
			ExecutionKey:      key,
			Engine:            &outputschema.StageReceiptEngine{Name: wf.Engine, Version: wf.Version},
			RemoteRunID:       runID,
			RemoteIncarnation: incarnation,
			InputRevision:     wf.InputRevision,
			OutputDigest:      effects.StableDigest(parts...),
			ResultClass:       class,
			StartedAt:         receiptEpoch.Add(time.Duration(startedTick*tickMinutes) * time.Minute).Format(time.RFC3339),
			EndedAt:           receiptEpoch.Add(time.Duration(endedTick*tickMinutes) * time.Minute).Format(time.RFC3339),
			Provenance:        &proof.Provenance{Query: "omp-workbench:run/" + runID},
			Artifacts:         artifacts,
		},
	}
	return json.Marshal(report)
}

func (w *Workbench) buildReceiptLocked(r *wbRun) ([]byte, error) {
	class := outputschema.ReceiptResultCompleted
	if hint := r.payload.Hints[HintResultClass]; hint != "" {
		class = outputschema.StageReceiptResultClass(hint)
	}
	return BuildReceipt(w.wf, r.payload.Admission, r.payload.Repo, r.key, r.id, w.opts.Incarnation, class, r.startedTick, r.endedTick)
}

// Stats returns a snapshot.
func (w *Workbench) Stats() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// Pending lists execution keys parked for an interactive answer, sorted.
func (w *Workbench) Pending() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	keys := make([]string, 0, len(w.pending))
	for k := range w.pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Answer resolves a parked offer. It reports false when nothing is parked
// for key.
func (w *Workbench) Answer(key string, accept bool, reason string) bool {
	w.mu.Lock()
	_, ok := w.pending[key]
	w.mu.Unlock()
	if !ok {
		return false
	}
	w.answer(key, accept, reason)
	return true
}

// SetWaiting parks or resumes a run by remote run id.
func (w *Workbench) SetWaiting(runID string, waiting bool) bool {
	w.mu.Lock()
	var r *wbRun
	for _, cand := range w.runs {
		if cand.id == runID {
			r = cand
		}
	}
	if r == nil || r.terminal {
		w.mu.Unlock()
		return false
	}
	r.waiting = waiting
	stage := w.stageOfLocked(r)
	w.mu.Unlock()
	state := extwork.StateRunning
	if waiting {
		state = extwork.StateWaiting
	}
	_ = w.send(omp.Message{Type: omp.MsgProgress, ExecutionKey: r.key, RemoteRunID: r.id, State: string(state), Stage: stage, Detail: "waiting toggled by control"})
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// controlHandler serves the loopback control surface tests drive.
func (w *Workbench) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(controlPrefix+"stats", func(rw http.ResponseWriter, _ *http.Request) { writeJSON(rw, http.StatusOK, w.Stats()) })
	mux.HandleFunc(controlPrefix+"pending", func(rw http.ResponseWriter, _ *http.Request) { writeJSON(rw, http.StatusOK, w.Pending()) })
	mux.HandleFunc(controlPrefix+"tick", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(rw, http.StatusMethodNotAllowed, map[string]string{"error": "POST"})
			return
		}
		w.Tick()
		writeJSON(rw, http.StatusOK, w.Stats())
	})
	mux.HandleFunc(controlPrefix+"disconnect", func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(rw, http.StatusMethodNotAllowed, map[string]string{"error": "POST"})
			return
		}
		w.Disconnect()
		writeJSON(rw, http.StatusOK, w.Stats())
	})
	mux.HandleFunc(controlPrefix+"answer/", func(rw http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, controlPrefix+"answer/")
		decision := r.URL.Query().Get("decision")
		if r.Method != http.MethodPost || (decision != "accept" && decision != "decline") {
			writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "POST with decision=accept|decline"})
			return
		}
		if !w.Answer(key, decision == "accept", r.URL.Query().Get("reason")) {
			writeJSON(rw, http.StatusNotFound, map[string]string{"error": "no pending offer for key"})
			return
		}
		writeJSON(rw, http.StatusOK, w.Stats())
	})
	waiting := func(on bool) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if r.Method != http.MethodPost || !w.SetWaiting(id, on) {
				writeJSON(rw, http.StatusNotFound, map[string]string{"error": "no live run " + id})
				return
			}
			writeJSON(rw, http.StatusOK, w.Stats())
		}
	}
	mux.HandleFunc(controlPrefix+"wait/", waiting(true))
	mux.HandleFunc(controlPrefix+"resume/", waiting(false))
	return mux
}

// Run starts the workbench from argv, prints its control address, and serves
// until ctx ends. It returns the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("omp-fixture", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts Options
	var noCapability bool
	fs.StringVar(&opts.HubURL, "hub", "", "hub websocket URL (required)")
	fs.StringVar(&opts.Identity, "identity", "", "contributor identity (required)")
	fs.StringVar(&opts.WorkflowDir, "workflow", "", "workflow directory (required)")
	fs.StringVar(&opts.Incarnation, "incarnation", "", "pin the workbench session incarnation")
	fs.StringVar(&opts.ControlListen, "listen", "", "control listen address (default 127.0.0.1:0)")
	fs.StringVar(&opts.DeclineMarker, "decline-marker", "", "summary substring the auto policy declines on")
	fs.BoolVar(&opts.Interactive, "interactive", false, "park offers until answered through the control endpoint")
	fs.BoolVar(&opts.IgnoreCancel, "ignore-cancel", false, "acknowledge cancel requests but keep running")
	fs.BoolVar(&noCapability, "no-capability", false, "connect without declaring ext-exec/omp (must be refused)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.HubURL == "" || opts.Identity == "" || opts.WorkflowDir == "" {
		fmt.Fprintln(stderr, "omp-fixture: -hub, -identity, and -workflow are required")
		return 2
	}
	if noCapability {
		opts.Capabilities = []string{"run-stage"}
	}
	w, err := Start(opts)
	if err != nil {
		fmt.Fprintln(stderr, "omp-fixture:", err)
		return 1
	}
	fmt.Fprintln(stdout, AddrLinePrefix+w.ControlAddr())
	select {
	case <-ctx.Done():
	case <-w.Done():
		// The hub dropped the link; keep serving control until told to stop
		// so a test can still read stats after a disconnect.
		<-ctx.Done()
	}
	w.Close()
	return 0
}

// Package fixture is a deterministic stand-in for a Flue runtime, used by the
// #8361 conformance tests and the runnable example under examples/flue/.
//
// It speaks the subset of Flue's native surface the adapter relies on, modelled
// on Flue commit c5a2a725fe1d93209ed294cca90af97060f6f2e2:
// enqueueDispatch's keyed admission (same idempotency key and payload
// deduplicate; same key and a different payload is a submission_conflict), a
// per-runtime incarnation uid, abort, and per-submission artifacts. It resumes
// from its own state file the way Flue resumes from its event log.
//
// It runs as a second local process (see Run and cmd/flue-fixture) or
// in-process (see Start). It
// needs no Node, no network, no model, and no GitHub token. Stages advance
// only when a test ticks them, so every run is reproducible. The workflow it
// executes is read from a directory such as testdata/flue-fixture/.
package fixture

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/outputschema"
)

const (
	// EngineName is what the fixture advertises as its engine.
	EngineName = "flue"
	// PinnedFlueCommit is the Flue commit whose native surface this fixture
	// imitates.
	PinnedFlueCommit = "c5a2a725fe1d93209ed294cca90af97060f6f2e2"
	// WorkflowFile is the workflow definition inside a workflow directory.
	WorkflowFile = "workflow.json"
	// ReceiptArtifact is the artifact path under which a finished submission
	// publishes its stage receipt.
	ReceiptArtifact = "receipt.json"
	// StateFile is the fixture's own event-log substitute inside StateDir.
	StateFile = "state.json"

	// States as the fixture reports them; the adapter maps them.
	StateAccepted = "accepted"
	StateRunning  = "running"
	StateWaiting  = "waiting"
	StateTerminal = "terminal"

	// Effect outcomes recorded in stats.
	EffectDelivered = "delivered"
	EffectRefused   = "refused"
	EffectNoRoute   = "no_route"

	// Effect kinds a workflow stage may attempt.
	EffectOutboundPost = "outbound_post"
	EffectGitHubWrite  = "github_write"

	// HintResultClass lets a payload steer the terminal result class
	// (completed, no_change, blocked, failed) so tests can exercise each.
	HintResultClass = "result_class"

	maxPayloadBytes   = 1 << 20
	effectTimeout     = 3 * time.Second
	incarnationBytes  = 8
	submissionIDBytes = 6
	tickMinutes       = 1
	statePerm         = 0o600
	stateDirPerm      = 0o755
)

// receiptEpoch anchors the deterministic timestamps: started_at and ended_at
// are the epoch plus the tick number in minutes.
var receiptEpoch = time.Date(2026, time.September, 22, 0, 0, 0, 0, time.UTC)

// Workflow is the definition read from WorkflowFile.
type Workflow struct {
	Engine        string  `json:"engine"`
	Version       string  `json:"version"`
	FlueCommit    string  `json:"flue_commit"`
	InputRevision string  `json:"input_revision"`
	Stages        []Stage `json:"stages"`
}

// Stage is one workflow stage: files it reads, effects it attempts, artifacts
// it emits.
type Stage struct {
	Name      string         `json:"name"`
	Reads     []string       `json:"reads,omitempty"`
	Effects   []Effect       `json:"effects,omitempty"`
	Artifacts []ArtifactSpec `json:"artifacts,omitempty"`
}

// Effect is a side effect a stage attempts outside the receipt.
type Effect struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

// ArtifactSpec maps an output file in the workflow directory to an artifact
// path in the receipt.
type ArtifactSpec struct {
	Path        string `json:"path"`
	Description string `json:"description"`
	Source      string `json:"source"`
}

// EffectAttempt is what the fixture recorded about one attempted effect.
type EffectAttempt struct {
	SubmissionID string `json:"submission_id"`
	Kind         string `json:"kind"`
	Target       string `json:"target"`
	Outcome      string `json:"outcome"`
	Detail       string `json:"detail,omitempty"`
}

// ArtifactRef locates an artifact of a submission.
type ArtifactRef struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Payload is the part of the dispatched bundle the fixture reads.
type Payload struct {
	Admission struct {
		WorkKey          string `json:"work_key"`
		AssignmentID     string `json:"assignment_id"`
		Generation       uint64 `json:"generation"`
		Stage            string `json:"stage"`
		ContractRevision string `json:"contract_revision"`
	} `json:"admission"`
	Summary string            `json:"summary"`
	Repo    string            `json:"repo"`
	Hints   map[string]string `json:"hints,omitempty"`
}

// Submission is one admitted keyed request.
type Submission struct {
	ID            string            `json:"id"`
	Key           string            `json:"key"`
	UID           string            `json:"uid"`
	PayloadDigest string            `json:"payload_digest"`
	Payload       Payload           `json:"payload"`
	State         string            `json:"state"`
	Stage         string            `json:"stage,omitempty"`
	StageIndex    int               `json:"stage_index"`
	ResultClass   string            `json:"result_class,omitempty"`
	Receipt       *ArtifactRef      `json:"receipt,omitempty"`
	Artifacts     map[string][]byte `json:"artifacts,omitempty"`
	StartedTick   int               `json:"started_tick"`
	EndedTick     int               `json:"ended_tick"`
	AbortRequests int               `json:"abort_requests"`
}

// Stats is the test-facing view of what the fixture did.
type Stats struct {
	Incarnation        string          `json:"incarnation"`
	Dispatches         int             `json:"dispatches"`
	Runs               int             `json:"runs"`
	AbortRequests      int             `json:"abort_requests"`
	Tick               int             `json:"tick"`
	EffectAttempts     []EffectAttempt `json:"effect_attempts"`
	GitHubTokenPresent bool            `json:"github_token_present"`
}

type persisted struct {
	Incarnation    string          `json:"incarnation"`
	Dispatches     int             `json:"dispatches"`
	AbortRequests  int             `json:"abort_requests"`
	Tick           int             `json:"tick"`
	Submissions    []*Submission   `json:"submissions"`
	EffectAttempts []EffectAttempt `json:"effect_attempts"`
}

// Options configure a fixture server.
type Options struct {
	// WorkflowDir holds WorkflowFile, source/, and outputs/.
	WorkflowDir string
	// StateDir, when set, persists state so a restart resumes.
	StateDir string
	// Incarnation pins the runtime uid; empty means resume the persisted one
	// or mint a random one.
	Incarnation string
	// IgnoreAbort makes the workload ignore abort requests: the request is
	// acknowledged but the run keeps going, which is the row 8 scenario.
	IgnoreAbort bool
	// Listen is the address to bind; default 127.0.0.1:0.
	Listen string
	// EgressProxy, when set, is the only route stage effects may take; empty
	// falls back to the process proxy environment, and no route at all means
	// the effect is recorded as no_route without opening a connection.
	EgressProxy string
}

// Server is a running fixture.
type Server struct {
	opts   Options
	wf     Workflow
	mu     sync.Mutex
	state  persisted
	ln     net.Listener
	srv    *http.Server
	proxy  func(*http.Request) (*url.URL, error)
	client *http.Client
}

// LoadWorkflow reads and checks a workflow directory.
func LoadWorkflow(dir string) (Workflow, error) {
	raw, err := os.ReadFile(filepath.Join(dir, WorkflowFile))
	if err != nil {
		return Workflow{}, fmt.Errorf("read workflow: %w", err)
	}
	var wf Workflow
	if err := json.Unmarshal(raw, &wf); err != nil {
		return Workflow{}, fmt.Errorf("parse workflow: %w", err)
	}
	if wf.Engine != EngineName || wf.Version == "" || wf.InputRevision == "" || len(wf.Stages) == 0 {
		return Workflow{}, errors.New("workflow must name engine flue, a version, an input revision, and at least one stage")
	}
	for _, st := range wf.Stages {
		for _, rel := range st.Reads {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
				return Workflow{}, fmt.Errorf("stage %s reads missing file %s", st.Name, rel)
			}
		}
		for _, art := range st.Artifacts {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(art.Source))); err != nil {
				return Workflow{}, fmt.Errorf("stage %s artifact source missing: %s", st.Name, art.Source)
			}
		}
	}
	return wf, nil
}

// Start loads the workflow, restores or mints state, and begins serving.
func Start(opts Options) (*Server, error) {
	wf, err := LoadWorkflow(opts.WorkflowDir)
	if err != nil {
		return nil, err
	}
	proxy := http.ProxyFromEnvironment
	if opts.EgressProxy != "" {
		proxyURL, err := url.Parse(opts.EgressProxy)
		if err != nil {
			return nil, fmt.Errorf("egress proxy: %w", err)
		}
		proxy = http.ProxyURL(proxyURL)
	}
	s := &Server{opts: opts, wf: wf, proxy: proxy, client: &http.Client{Timeout: effectTimeout, Transport: &http.Transport{Proxy: proxy}}}
	if err := s.restore(); err != nil {
		return nil, err
	}
	listen := opts.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	s.ln = ln
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: effectTimeout}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Addr is the base URL of the running fixture.
func (s *Server) Addr() string { return "http://" + s.ln.Addr().String() }

// Incarnation returns the runtime uid.
func (s *Server) Incarnation() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Incarnation
}

// Close stops serving.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), effectTimeout)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func (s *Server) statePath() string {
	if s.opts.StateDir == "" {
		return ""
	}
	return filepath.Join(s.opts.StateDir, StateFile)
}

func (s *Server) restore() error {
	s.state = persisted{Incarnation: s.opts.Incarnation}
	path := s.statePath()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err == nil {
			var loaded persisted
			if err := json.Unmarshal(raw, &loaded); err != nil {
				return fmt.Errorf("corrupt fixture state: %w", err)
			}
			if s.opts.Incarnation == "" || s.opts.Incarnation == loaded.Incarnation {
				s.state = loaded
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if s.state.Incarnation == "" {
		s.state.Incarnation = randomHex(incarnationBytes)
	}
	return s.persistLocked()
}

func (s *Server) persistLocked() error {
	path := s.statePath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), stateDirPerm); err != nil {
		return err
	}
	raw, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, statePerm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Server) findByKeyLocked(key string) *Submission {
	for _, sub := range s.state.Submissions {
		if sub.Key == key {
			return sub
		}
	}
	return nil
}

func (s *Server) findByIDLocked(id string) *Submission {
	for _, sub := range s.state.Submissions {
		if sub.ID == id {
			return sub
		}
	}
	return nil
}

// Dispatch admits a keyed payload. It is the enqueueDispatch analogue.
func (s *Server) Dispatch(key string, payload []byte) (*Submission, bool, error) {
	if key == "" {
		return nil, false, errors.New("idempotency key is required")
	}
	if len(payload) > maxPayloadBytes {
		return nil, false, errors.New("payload too large")
	}
	var p Payload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, false, fmt.Errorf("payload is not a bundle: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Dispatches++
	digest := digestOf(payload)
	if existing := s.findByKeyLocked(key); existing != nil {
		if existing.PayloadDigest != digest {
			_ = s.persistLocked()
			return nil, false, ErrConflict
		}
		_ = s.persistLocked()
		return existing, true, nil
	}
	sub := &Submission{ID: "sub-" + randomHex(submissionIDBytes), Key: key, UID: s.state.Incarnation, PayloadDigest: digest, Payload: p, State: StateAccepted, StageIndex: -1, StartedTick: s.state.Tick, Artifacts: map[string][]byte{}}
	s.state.Submissions = append(s.state.Submissions, sub)
	return sub, false, s.persistLocked()
}

// ErrConflict is the submission_conflict analogue.
var ErrConflict = errors.New("submission_conflict")

// Tick advances every runnable submission by one stage.
func (s *Server) Tick() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Tick++
	for _, sub := range s.state.Submissions {
		if sub.State == StateTerminal || sub.State == StateWaiting {
			continue
		}
		if err := s.advanceLocked(sub); err != nil {
			return err
		}
	}
	return s.persistLocked()
}

func (s *Server) advanceLocked(sub *Submission) error {
	sub.StageIndex++
	stage := s.wf.Stages[sub.StageIndex]
	sub.State = StateRunning
	sub.Stage = stage.Name
	for _, rel := range stage.Reads {
		if _, err := os.ReadFile(filepath.Join(s.opts.WorkflowDir, filepath.FromSlash(rel))); err != nil {
			return err
		}
	}
	for _, eff := range stage.Effects {
		s.state.EffectAttempts = append(s.state.EffectAttempts, s.attemptEffect(sub.ID, eff))
	}
	for _, art := range stage.Artifacts {
		raw, err := os.ReadFile(filepath.Join(s.opts.WorkflowDir, filepath.FromSlash(art.Source)))
		if err != nil {
			return err
		}
		sub.Artifacts[art.Path] = raw
	}
	if sub.StageIndex == len(s.wf.Stages)-1 {
		return s.finishLocked(sub)
	}
	return nil
}

func (s *Server) attemptEffect(subID string, eff Effect) EffectAttempt {
	attempt := EffectAttempt{SubmissionID: subID, Kind: eff.Kind, Target: eff.Target}
	req, err := http.NewRequest(http.MethodPost, eff.Target, strings.NewReader(`{"fixture":"flue","effect":"`+eff.Kind+`"}`))
	if err != nil {
		attempt.Outcome, attempt.Detail = EffectRefused, err.Error()
		return attempt
	}
	if eff.Kind == EffectGitHubWrite {
		if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	proxyURL, err := s.proxy(req)
	if err != nil || proxyURL == nil {
		// No egress route configured: the fixture never opens a direct
		// connection, so a test run without a proxy touches no network.
		attempt.Outcome, attempt.Detail = EffectNoRoute, "no egress proxy configured"
		return attempt
	}
	resp, err := s.client.Do(req)
	if err != nil {
		attempt.Outcome, attempt.Detail = EffectRefused, err.Error()
		return attempt
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		attempt.Outcome = EffectDelivered
	} else {
		attempt.Outcome, attempt.Detail = EffectRefused, resp.Status
	}
	return attempt
}

func (s *Server) finishLocked(sub *Submission) error {
	sub.State = StateTerminal
	sub.EndedTick = s.state.Tick
	class := outputschema.ReceiptResultCompleted
	if hint := sub.Payload.Hints[HintResultClass]; hint != "" {
		class = outputschema.StageReceiptResultClass(hint)
	}
	sub.ResultClass = string(class)
	artifacts := []outputschema.Artifact{}
	if class == outputschema.ReceiptResultCompleted {
		for _, st := range s.wf.Stages {
			for _, art := range st.Artifacts {
				artifacts = append(artifacts, outputschema.Artifact{Repo: sub.Payload.Repo, Path: art.Path, Description: art.Description})
			}
		}
	}
	raw, err := BuildReceipt(s.wf, sub, class, artifacts)
	if err != nil {
		return err
	}
	sub.Artifacts[ReceiptArtifact] = raw
	sub.Receipt = &ArtifactRef{Path: ReceiptArtifact, Digest: digestOf(raw), Size: int64(len(raw))}
	return nil
}

// BuildReceipt renders the stage_receipt AgentReport for a finished
// submission. Its output digest follows the outputschema rule so Hive's
// validator accepts it.
func BuildReceipt(wf Workflow, sub *Submission, class outputschema.StageReceiptResultClass, artifacts []outputschema.Artifact) ([]byte, error) {
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
		Summary:    "flue fixture " + string(class) + " for " + sub.Payload.Admission.WorkKey,
		Receipt: &outputschema.StageReceipt{
			SchemaVersion:     outputschema.StageReceiptSchemaVersion,
			WorkKey:           sub.Payload.Admission.WorkKey,
			AssignmentID:      sub.Payload.Admission.AssignmentID,
			Generation:        sub.Payload.Admission.Generation,
			Stage:             sub.Payload.Admission.Stage,
			ContractRevision:  sub.Payload.Admission.ContractRevision,
			ExecutionKey:      sub.Key,
			Engine:            &outputschema.StageReceiptEngine{Name: wf.Engine, Version: wf.Version},
			RemoteRunID:       sub.ID,
			RemoteIncarnation: sub.UID,
			InputRevision:     wf.InputRevision,
			OutputDigest:      effects.StableDigest(parts...),
			ResultClass:       class,
			StartedAt:         receiptEpoch.Add(time.Duration(sub.StartedTick*tickMinutes) * time.Minute).Format(time.RFC3339),
			EndedAt:           receiptEpoch.Add(time.Duration(sub.EndedTick*tickMinutes) * time.Minute).Format(time.RFC3339),
			Provenance:        &proof.Provenance{Query: "flue-fixture:submission/" + sub.ID},
			Artifacts:         artifacts,
		},
	}
	return json.Marshal(report)
}

// Abort requests cancellation. With IgnoreAbort the request is acknowledged
// and nothing stops; otherwise the run ends as failed.
func (s *Server) Abort(id string) (requested, acknowledged, stopped bool, detail string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.findByIDLocked(id)
	if sub == nil {
		return false, false, false, "", os.ErrNotExist
	}
	s.state.AbortRequests++
	sub.AbortRequests++
	requested, acknowledged = true, true
	switch {
	case sub.State == StateTerminal:
		detail = "already terminal"
	case s.opts.IgnoreAbort:
		detail = "workload ignored abort"
	default:
		sub.State = StateTerminal
		sub.EndedTick = s.state.Tick
		sub.ResultClass = string(outputschema.ReceiptResultFailed)
		stopped = true
		detail = "aborted"
	}
	return requested, acknowledged, stopped, detail, s.persistLocked()
}

// SetWaiting parks or resumes a submission, the row 13 human-wait scenario.
func (s *Server) SetWaiting(id string, waiting bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.findByIDLocked(id)
	if sub == nil {
		return os.ErrNotExist
	}
	if sub.State == StateTerminal {
		return errors.New("submission is terminal")
	}
	if waiting {
		sub.State = StateWaiting
	} else {
		sub.State = StateRunning
	}
	return s.persistLocked()
}

// Stats returns the counters and effect attempts.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Incarnation:        s.state.Incarnation,
		Dispatches:         s.state.Dispatches,
		Runs:               len(s.state.Submissions),
		AbortRequests:      s.state.AbortRequests,
		Tick:               s.state.Tick,
		EffectAttempts:     append([]EffectAttempt(nil), s.state.EffectAttempts...),
		GitHubTokenPresent: os.Getenv("GITHUB_TOKEN") != "",
	}
}

func (s *Server) snapshot(sub *Submission) Submission {
	out := *sub
	out.Artifacts = nil
	out.Payload = Payload{}
	return out
}

// Handler serves the native surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"engine": s.wf.Engine, "version": s.wf.Version, "incarnation": s.Incarnation(), "flue_commit": s.wf.FlueCommit})
	})
	mux.HandleFunc("POST /dispatch", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IdempotencyKey string `json:"idempotency_key"`
			Payload        []byte `json:"payload"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, maxPayloadBytes*2)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"type": "bad_request", "error": err.Error()})
			return
		}
		sub, dedup, err := s.Dispatch(body.IdempotencyKey, body.Payload)
		switch {
		case errors.Is(err, ErrConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"type": "submission_conflict"})
		case err != nil:
			writeJSON(w, http.StatusBadRequest, map[string]string{"type": "bad_request", "error": err.Error()})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"submission_id": sub.ID, "uid": sub.UID, "deduplicated": dedup})
		}
	})
	mux.HandleFunc("GET /submissions", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		sub := s.findByKeyLocked(r.URL.Query().Get("key"))
		var view Submission
		if sub != nil {
			view = s.snapshot(sub)
		}
		s.mu.Unlock()
		if sub == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"type": "not_found"})
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
	mux.HandleFunc("GET /submissions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		sub := s.findByIDLocked(r.PathValue("id"))
		var view Submission
		if sub != nil {
			view = s.snapshot(sub)
		}
		s.mu.Unlock()
		if sub == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"type": "not_found"})
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
	mux.HandleFunc("POST /submissions/{id}/abort", func(w http.ResponseWriter, r *http.Request) {
		requested, acknowledged, stopped, detail, err := s.Abort(r.PathValue("id"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"type": "not_found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"requested": requested, "acknowledged": acknowledged, "stopped": stopped, "detail": detail})
	})
	mux.HandleFunc("GET /submissions/{id}/artifacts/{path...}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		sub := s.findByIDLocked(r.PathValue("id"))
		var raw []byte
		ok := false
		if sub != nil {
			raw, ok = sub.Artifacts[r.PathValue("path")]
		}
		s.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"type": "not_found"})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("POST /control/tick", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Tick(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"tick": s.Stats().Tick})
	})
	mux.HandleFunc("POST /control/wait/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.controlWaiting(w, r.PathValue("id"), true)
	})
	mux.HandleFunc("POST /control/resume/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.controlWaiting(w, r.PathValue("id"), false)
	})
	mux.HandleFunc("GET /control/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Stats())
	})
	return mux
}

func (s *Server) controlWaiting(w http.ResponseWriter, id string, waiting bool) {
	if err := s.SetWaiting(id, waiting); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"waiting": waiting})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// AddrLinePrefix is what Run prints before the base URL so a parent process
// can find the port.
const AddrLinePrefix = "FLUE_FIXTURE_ADDR "

// Run parses flags, starts the fixture, prints its address, and serves until
// ctx ends. It returns a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flue-fixture", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts Options
	fs.StringVar(&opts.WorkflowDir, "workflow", "", "workflow directory (required)")
	fs.StringVar(&opts.StateDir, "state", "", "state directory; resumes across restarts when set")
	fs.StringVar(&opts.Incarnation, "incarnation", "", "pin the runtime incarnation uid")
	fs.BoolVar(&opts.IgnoreAbort, "ignore-abort", false, "acknowledge abort requests but keep running")
	fs.StringVar(&opts.Listen, "listen", "", "listen address (default 127.0.0.1:0)")
	fs.StringVar(&opts.EgressProxy, "egress-proxy", "", "sole egress route for stage effects (default: proxy environment)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if opts.WorkflowDir == "" {
		fmt.Fprintln(stderr, "flue-fixture: -workflow is required")
		return 2
	}
	s, err := Start(opts)
	if err != nil {
		fmt.Fprintln(stderr, "flue-fixture:", err)
		return 1
	}
	fmt.Fprintln(stdout, AddrLinePrefix+s.Addr())
	<-ctx.Done()
	_ = s.Close()
	return 0
}

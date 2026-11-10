package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/agentstep"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/checkout"
	"github.com/hivecommons/hive/crux/cmdlog"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/crux/signals"
)

// Artifact names inside a run's directory: the full scrubbed output.
const (
	StdoutArtifact = "stdout.txt"
	StderrArtifact = "stderr.txt"
	// launchNamePrefix names the container/Job after the run so a teardown
	// can find it.
	launchNamePrefix = "crux-run-"
	// ReconcileReason is recorded on runs that were in flight when the
	// serving process stopped.
	ReconcileReason = "crux serve restarted while the run was in flight; the exit is not knowable"
	// exitUnknown is the launcher convention for "exit not read".
	exitUnknown = -1
)

// Typed errors. The serve routes map each onto an api.Code*.
var (
	// ErrNotConfigured: the server has no backend; records are still readable.
	ErrNotConfigured = errors.New("runs: no contained-run backend is configured on this server")
	// ErrNotFound: unknown run ID.
	ErrNotFound = runstate.ErrNotFound
	// ErrNoSignals: the server keeps no turn signals (Service.Signals nil).
	ErrNoSignals = errors.New("runs: turn signals are not enabled on this server")
	// ErrNotSession: a step was posted to a run that is not an open
	// agent-profile session.
	ErrNotSession = errors.New("runs: not an open agent step session")
	// ErrStepBusy: the session is already running a step.
	ErrStepBusy = errors.New("runs: the session is already running a step")
)

// RequestError is a malformed request (400).
type RequestError struct{ Msg string }

func (e *RequestError) Error() string { return e.Msg }

// ForbiddenError is a workspace the configuration does not allow (403).
type ForbiddenError struct{ Msg string }

func (e *ForbiddenError) Error() string { return e.Msg }

// UnsupportedError names a requested feature or limit the selected backend
// cannot enforce (501). The run never starts.
type UnsupportedError struct{ What string }

func (e *UnsupportedError) Error() string { return e.What }

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// detail is the service's payload inside the runstate record.
type detail struct {
	Profile           string   `json:"profile"`
	Network           string   `json:"network"`
	Backend           string   `json:"backend"`
	Command           []string `json:"command"`
	Checkout          string   `json:"checkout,omitempty"`
	Workspace         string   `json:"workspace,omitempty"`
	EnvAllowlist      []string `json:"env_allowlist,omitempty"`
	ExitCode          *int     `json:"exit_code,omitempty"`
	TimedOut          bool     `json:"timed_out,omitempty"`
	Stdout            string   `json:"stdout,omitempty"`
	Stderr            string   `json:"stderr,omitempty"`
	StdoutTruncatedAt *int64   `json:"stdout_truncated_at,omitempty"`
	StderrTruncatedAt *int64   `json:"stderr_truncated_at,omitempty"`
	// Request is what the run was created from, less the idempotency key
	// and detach, so a restart (S7) re-validates and re-runs the same thing.
	Request *api.ContainedRunRequest `json:"request,omitempty"`
	// RestartedFrom and RestartedBy link a restart to the run it re-ran and
	// back; RestartReason is the caller's stated reason for the restart.
	RestartedFrom string `json:"restarted_from,omitempty"`
	RestartedBy   string `json:"restarted_by,omitempty"`
	RestartReason string `json:"restart_reason,omitempty"`
}

// plan is a validated request.
type plan struct {
	worktree  *api.ContainedRunWorktree
	checkout  string
	workspace string
	command   []string
	allowlist []string
	network   string
	timeout   time.Duration
	maxOutput int64
	profile   string
}

type active struct {
	cancel    context.CancelFunc
	done      chan struct{}
	cancelled bool
	// checkout is the S1 id the run holds open, if any (see InUse).
	checkout string
	// The rest is the agent-profile session's (S6): ctx is what its steps
	// run under, ready closes once the workspace is provisioned (readyErr
	// says if that failed), and busy is held while a step runs.
	ctx      context.Context
	ready    chan struct{}
	readyErr error
	busy     chan struct{}
}

// Caller is who asked for a run: the authenticated principal's name and
// whether it holds the operator credential. The operator may run against
// any checkout; a project only against its own.
type Caller struct {
	Name     string
	Operator bool
}

// CheckoutResolver looks up S1 checkouts by id; *checkout.Service
// satisfies it. The runs service never creates or deletes checkouts.
type CheckoutResolver interface {
	Get(id string) (checkout.Record, error)
}

// Service accepts, executes, cancels and reports contained runs.
type Service struct {
	Store   *runstate.Store
	Backend *Backend
	Config  Config
	// Checkouts resolves the request's checkout id to a pinned worktree
	// (S1). Nil means this server has no checkout service and checkout
	// ids are refused as unsupported.
	Checkouts CheckoutResolver
	// Provisioner materialises the ad-hoc worktree form; nil uses git.
	Provisioner Provisioner
	// Environ supplies the base environment the allowlist filters; nil uses
	// os.Environ.
	Environ func() []string
	// Scrub redacts output before it is persisted; nil uses logscrub.
	Scrub func(string) string
	// CmdLog opens the per-run command log (S4) the runner appends to;
	// nil keeps no log. Reading does not need it: Commands reads the file.
	CmdLog cmdlog.Opener
	// Signals is the turn-signal book (S5): the runner appends started and
	// done through it, AppendSignal everything else. Nil keeps no signals;
	// ReadSignals still reads what an earlier process wrote.
	Signals *signals.Book

	mu     sync.Mutex
	active map[string]*active
}

// New returns a Service over store. backend nil means not configured: the
// saved records stay readable and every create answers ErrNotConfigured.
func New(store *runstate.Store, backend *Backend, cfg Config) *Service {
	cfg.Defaults()
	return &Service{Store: store, Backend: backend, Config: cfg, active: map[string]*active{}}
}

// Configured reports whether runs can be executed here.
func (s *Service) Configured() bool { return s.Backend != nil }

// Reconcile marks every run left accepted, running or waiting by a previous
// process unfinished, and with it any step (S6) that run still records as
// running and any command (S4) its log still records as in flight. Call it
// once at serve start, before accepting requests.
func (s *Service) Reconcile() ([]runstate.Record, error) {
	recs, err := s.Store.Reconcile(ReconcileReason)
	// A run the previous process left in flight ended unobserved; its done
	// signal says so, so a follower never waits on it forever.
	for _, rec := range recs {
		if _, serr := (agentstep.Store{RunDir: s.Store.Path(rec.ID)}).Reconcile(ReconcileReason, s.Store.Now()); serr != nil && err == nil {
			err = serr
		}
		// Reopening its command log ends the command it left in flight
		// (cmdlog.Open), so a follower's next_seq is not pinned behind it
		// forever. Like every other cmdlog failure, an unopenable log is
		// skipped rather than holding up the restart.
		if s.CmdLog != nil {
			_, _ = s.CmdLog(s.Store.Path(rec.ID))
		}
		s.signalDone(rec.ID, api.SignalDoneData{Status: rec.Status}, rec.Reason)
	}
	return recs, err
}

// Create validates req, saves the accepted record owned by caller.Name and
// starts the run under launchCtx (which must outlive the request). created
// is false when req.IdempotencyKey matched an existing run, whose record
// is returned.
func (s *Service) Create(launchCtx context.Context, caller Caller, req api.ContainedRunRequest) (api.ContainedRun, bool, error) {
	if !s.Configured() {
		return api.ContainedRun{}, false, ErrNotConfigured
	}
	p, err := s.validate(caller, req)
	if err != nil {
		return api.ContainedRun{}, false, err
	}
	return s.accept(launchCtx, caller.Name, req.IdempotencyKey, p, s.detailFor(p, req))
}

// detailFor is the accepted record's payload: the plan and the request it
// came from, with the per-call fields (idempotency key, detach) dropped so
// a restart cannot inherit them.
func (s *Service) detailFor(p plan, req api.ContainedRunRequest) detail {
	req.IdempotencyKey, req.Detach = "", false
	return detail{Profile: p.profile, Network: p.network, Backend: s.Backend.Name, Command: p.command, Checkout: p.checkout, Workspace: p.workspace, EnvAllowlist: p.allowlist, Request: &req}
}

// accept saves d as owner's accepted record (or returns the record key
// already names) and starts the run under launchCtx.
func (s *Service) accept(launchCtx context.Context, owner, key string, p plan, d detail) (api.ContainedRun, bool, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return api.ContainedRun{}, false, err
	}
	rec, created, err := s.Store.Create(runstate.Record{
		Kind:           runstate.KindRun,
		Owner:          owner,
		IdempotencyKey: runstate.DeriveIdempotencyKey(owner, key),
		Detail:         raw,
	})
	if err != nil {
		return api.ContainedRun{}, false, err
	}
	if !created {
		return toWire(rec), false, nil
	}
	ctx, cancel := context.WithCancel(launchCtx)
	a := &active{cancel: cancel, done: make(chan struct{}), checkout: p.checkout}
	s.mu.Lock()
	s.active[rec.ID] = a
	s.mu.Unlock()
	if p.profile == api.RunProfileAgent {
		a.ctx, a.ready, a.busy = ctx, make(chan struct{}), make(chan struct{}, 1)
		go s.session(ctx, rec.ID, p, a)
	} else {
		go s.execute(ctx, rec.ID, p, a)
	}
	return toWire(rec), true, nil
}

// InUse reports whether a run in flight holds checkout id. It is the
// checkout.Config.InUse hook: a checkout with live runs cannot be deleted.
func (s *Service) InUse(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.active {
		if a.checkout == id {
			return true
		}
	}
	return false
}

// Get returns the saved record whatever the process state.
func (s *Service) Get(id string) (api.ContainedRun, error) {
	rec, err := s.Store.Get(id)
	if err != nil {
		return api.ContainedRun{}, err
	}
	return toWire(rec), nil
}

// Commands is the run's command log (S4) after sinceSeq, at most limit
// records. A run whose directory holds no log yet answers an empty page,
// not an error; only an unknown id is ErrNotFound.
func (s *Service) Commands(id string, sinceSeq int64, limit int) (api.CommandPage, error) {
	if _, err := s.Store.Get(id); err != nil {
		return api.CommandPage{}, err
	}
	page, err := cmdlog.Read(s.Store.Path(id), sinceSeq, limit)
	if errors.Is(err, cmdlog.ErrNotFound) {
		page, err = api.CommandPage{Commands: []api.CommandRecord{}, NextSeq: sinceSeq}, nil
	}
	page.Run = id
	return page, err
}

// ReadSignals is the run's turn signals (S5) after sinceSeq, at most limit
// of them. A run whose directory holds no signals yet answers an empty
// page, not an error; only an unknown id is ErrNotFound.
func (s *Service) ReadSignals(id string, sinceSeq int64, limit int) (api.RunSignalPage, error) {
	if _, err := s.Store.Get(id); err != nil {
		return api.RunSignalPage{}, err
	}
	page, err := signals.Read(s.Store.Path(id), sinceSeq, limit)
	if errors.Is(err, signals.ErrNotFound) {
		page, err = api.RunSignalPage{Signals: []api.RunSignal{}, NextSeq: sinceSeq}, nil
	}
	page.Run = id
	return page, err
}

// AppendSignal records in on run id: a caller's report of where the turn
// is, or a gate's. The book enforces the kind set and what each source may
// append. A run that reached a terminal status is signals.ErrClosed even
// before its done line is written (or when it finished on a server that
// kept no signals): only the runner reports on a run that has ended.
func (s *Service) AppendSignal(id string, in signals.Signal) (api.RunSignal, error) {
	if s.Signals == nil {
		return api.RunSignal{}, ErrNoSignals
	}
	rec, err := s.Store.Get(id)
	if err != nil {
		return api.RunSignal{}, err
	}
	if runstate.Terminal(rec.Status) {
		return api.RunSignal{}, fmt.Errorf("%w: run %s is %s", signals.ErrClosed, id, rec.Status)
	}
	return s.Signals.Append(id, s.Store.Path(id), in)
}

// Wait blocks until the run reaches a terminal status or ctx ends, then
// returns the saved record.
func (s *Service) Wait(ctx context.Context, id string) (api.ContainedRun, error) {
	s.mu.Lock()
	a := s.active[id]
	s.mu.Unlock()
	if a != nil {
		select {
		case <-a.done:
		case <-ctx.Done():
			return api.ContainedRun{}, ctx.Err()
		}
	}
	return s.Get(id)
}

// Cancel stops a run in flight and waits for its record to settle. A run
// that already finished is returned unchanged with AlreadyFinished set.
func (s *Service) Cancel(ctx context.Context, id string) (api.ContainedRun, error) {
	rec, err := s.Store.Get(id)
	if err != nil {
		return api.ContainedRun{}, err
	}
	if runstate.Terminal(rec.Status) {
		out := toWire(rec)
		out.AlreadyFinished = true
		return out, nil
	}
	s.mu.Lock()
	a := s.active[id]
	if a != nil {
		a.cancelled = true
		a.cancel()
	}
	s.mu.Unlock()
	if a == nil {
		// Accepted or running with no goroutine: not possible after
		// Reconcile ran at start; report the record as it is.
		return toWire(rec), nil
	}
	return s.Wait(ctx, id)
}

func (s *Service) validate(caller Caller, req api.ContainedRunRequest) (plan, error) {
	var p plan
	switch p.profile = req.Profile; p.profile {
	case "":
		p.profile = api.RunProfileExperiment
	case api.RunProfileExperiment:
	case api.RunProfileAgent:
		// A session runs no command of its own: its steps are posted to it.
		if len(req.Command) > 0 {
			return p, &RequestError{fmt.Sprintf("profile %q takes no command; post steps to the session instead", req.Profile)}
		}
	default:
		return p, &RequestError{fmt.Sprintf("profile must be %q or %q", api.RunProfileExperiment, api.RunProfileAgent)}
	}
	if p.profile != api.RunProfileAgent && (len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "") {
		return p, &RequestError{"command is required"}
	}
	p.command = req.Command
	switch p.network = req.Network; p.network {
	case "":
		// An agent needs its model, so a session defaults to the restricted
		// network and is refused when the backend has none; an experiment
		// defaults to none.
		p.network = api.RunNetworkNone
		if p.profile == api.RunProfileAgent {
			p.network = api.RunNetworkRestricted
		}
	case api.RunNetworkNone, api.RunNetworkRestricted:
	default:
		return p, &RequestError{fmt.Sprintf("network must be %q or %q", api.RunNetworkNone, api.RunNetworkRestricted)}
	}
	if what, ok := s.Backend.Supports(p.network); !ok {
		return p, &UnsupportedError{what}
	}
	for _, name := range req.EnvAllowlist {
		if sandbox.IsCredentialName(name) {
			return p, &RequestError{fmt.Sprintf("env_allowlist names credential %q; contained runs carry no repository credentials", name)}
		}
	}
	p.allowlist = req.EnvAllowlist
	p.timeout = time.Duration(s.Config.DefaultTimeoutMs) * time.Millisecond
	if req.TimeoutMs > 0 {
		if req.TimeoutMs > s.Config.MaxTimeoutMs {
			return p, &RequestError{fmt.Sprintf("timeout_ms %d exceeds this server's cap of %d", req.TimeoutMs, s.Config.MaxTimeoutMs)}
		}
		p.timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	p.maxOutput = s.Config.DefaultMaxOutputBytes
	if req.MaxOutputBytes > 0 {
		if req.MaxOutputBytes > s.Config.MaxOutputBytesCap {
			return p, &RequestError{fmt.Sprintf("max_output_bytes %d exceeds this server's cap of %d", req.MaxOutputBytes, s.Config.MaxOutputBytesCap)}
		}
		p.maxOutput = req.MaxOutputBytes
	}
	return p, s.validateWorkspace(caller, req, &p)
}

func (s *Service) validateWorkspace(caller Caller, req api.ContainedRunRequest, p *plan) error {
	sources := 0
	for _, set := range []bool{req.Checkout != "", req.Worktree != nil, req.WorkspacePath != ""} {
		if set {
			sources++
		}
	}
	switch {
	case sources == 0:
		return &RequestError{"one of checkout, worktree or workspace_path is required"}
	case sources > 1:
		return &RequestError{"checkout, worktree and workspace_path are mutually exclusive"}
	case req.Checkout != "":
		return s.resolveCheckout(caller, req.Checkout, p)
	case req.Worktree != nil:
		wt := *req.Worktree
		if strings.TrimSpace(wt.Repo) == "" {
			return &RequestError{"worktree.repo is required"}
		}
		if !fullSHA.MatchString(wt.Commit) {
			return &RequestError{"worktree.commit must be a full 40-hex commit SHA; a tag or branch name cannot pin the run"}
		}
		// The URL must already be canonical: git (libcurl for https, literally
		// for file) resolves "..", so a prefix check on the raw string would
		// otherwise admit "https://h/allowed/../other/repo".
		if err := checkout.ValidateRepoURL(wt.Repo); err != nil {
			return &RequestError{"worktree.repo: " + strings.TrimPrefix(err.Error(), checkout.ErrBadRequest.Error()+": ")}
		}
		if !repoAllowed(wt.Repo, s.Config.RepoAllowlist) {
			return &ForbiddenError{fmt.Sprintf("repository %q is not on this server's repo_allowlist", wt.Repo)}
		}
		p.worktree = &wt
		return nil
	}
	path := filepath.Clean(req.WorkspacePath)
	if !filepath.IsAbs(path) {
		return &RequestError{"workspace_path must be absolute"}
	}
	if !pathUnder(path, s.Config.WorkspaceRoots) {
		return &ForbiddenError{fmt.Sprintf("workspace_path %q is not under this server's workspace_roots", path)}
	}
	// The lexical check above cannot see symlinks: <root>/x pointing at /
	// passes it, and a previous run can plant such a link in its rw
	// workspace. Resolve the path, check the resolved form against the
	// resolved roots, and mount the resolved path so the backend never
	// follows a link the check did not see.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return &RequestError{fmt.Sprintf("workspace_path %q is not a directory", path)}
	}
	if !pathUnder(resolved, resolvedRoots(s.Config.WorkspaceRoots)) {
		return &ForbiddenError{fmt.Sprintf("workspace_path %q resolves outside this server's workspace_roots", path)}
	}
	if st, err := os.Stat(resolved); err != nil || !st.IsDir() {
		return &RequestError{fmt.Sprintf("workspace_path %q is not a directory", path)}
	}
	p.workspace = resolved
	return nil
}

// resolveCheckout points the plan at the S1 checkout's pinned worktree.
// The id must exist and belong to the caller (or the caller is the
// operator): the same rule checkout applies to deletion.
func (s *Service) resolveCheckout(caller Caller, id string, p *plan) error {
	if s.Checkouts == nil {
		return &UnsupportedError{"checkout ids need a checkout service and this server has none (start it with --checkout-allow); use worktree {repo, commit} or workspace_path"}
	}
	rec, err := s.Checkouts.Get(id)
	switch {
	case errors.Is(err, checkout.ErrNotFound):
		return &RequestError{fmt.Sprintf("checkout %q not found", id)}
	case err != nil:
		return err
	case !checkout.CanUse(rec, caller.Name, caller.Operator):
		return &ForbiddenError{fmt.Sprintf("checkout %s is owned by %q, not by %q", rec.ID, rec.Owner, caller.Name)}
	}
	p.checkout, p.workspace = rec.ID, rec.Path
	return nil
}

// repoAllowed reports whether repo falls under one of the allowlisted URL
// prefixes on a segment boundary (checkout.PrefixMatches), so an entry
// without a trailing slash does not admit "<entry>-evil/…".
func repoAllowed(repo string, prefixes []string) bool {
	for _, p := range prefixes {
		if p = strings.TrimSpace(p); p != "" && checkout.PrefixMatches(repo, p) {
			return true
		}
	}
	return false
}

// pathUnder reports whether path is one of roots or lies beneath one.
func pathUnder(path string, roots []string) bool {
	for _, root := range roots {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "" || root == "." {
			continue
		}
		if path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/") {
			return true
		}
	}
	return false
}

// resolvedRoots returns roots with symlinks resolved, so a root that is
// itself reached through a link (a symlinked /tmp, a bind-mount alias)
// still contains the resolved paths beneath it. A root that cannot be
// resolved (it does not exist) is kept as written: nothing resolves
// under a missing directory, so it admits nothing it should not.
func resolvedRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "" || root == "." {
			continue
		}
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		out = append(out, root)
	}
	return out
}

// execute runs one accepted run to a terminal status. Every exit path
// writes the record and then the run's done signal; nothing is left
// accepted or running by this process.
func (s *Service) execute(ctx context.Context, id string, p plan, a *active) {
	defer func() {
		s.mu.Lock()
		delete(s.active, id)
		s.mu.Unlock()
		close(a.done)
	}()
	finish := func(status, reason string, fill func(*detail)) {
		_, _ = s.Store.Update(id, func(rec *runstate.Record) error {
			var d detail
			_ = json.Unmarshal(rec.Detail, &d)
			if fill != nil {
				fill(&d)
			}
			raw, err := json.Marshal(d)
			if err != nil {
				return err
			}
			rec.Status, rec.Reason, rec.Detail = status, reason, raw
			return nil
		})
		// fill only assigns the outcome fields, so applying it to an empty
		// detail yields them whether or not the record write succeeded.
		var out detail
		if fill != nil {
			fill(&out)
		}
		s.signalDone(id, api.SignalDoneData{Status: status, ExitCode: out.ExitCode, TimedOut: out.TimedOut}, reason)
	}
	workspace := p.workspace
	if p.worktree != nil {
		prov := s.Provisioner
		if prov == nil {
			prov = GitProvisioner{}
		}
		path, err := prov.Provision(ctx, s.Store.Path(id), *p.worktree)
		if err != nil {
			if ctx.Err() != nil {
				finish(runstate.StatusCancelled, "cancelled while provisioning the worktree", nil)
			} else {
				finish(runstate.StatusFailed, "provisioning the worktree: "+s.scrub(err.Error()), nil)
			}
			return
		}
		workspace = path
	}
	if _, err := s.Store.Update(id, func(rec *runstate.Record) error {
		var d detail
		_ = json.Unmarshal(rec.Detail, &d)
		d.Workspace = workspace
		raw, err := json.Marshal(d)
		rec.Status, rec.Detail = runstate.StatusRunning, raw
		return err
	}); err != nil {
		finish(runstate.StatusFailed, "recording the start: "+err.Error(), nil)
		return
	}

	runCtx, cancelTimeout := context.WithTimeout(ctx, p.timeout)
	defer cancelTimeout()
	name := launchNamePrefix + id
	spec := sandbox.LaunchSpec{
		Image:        s.Config.Image,
		Name:         name,
		Workspace:    workspace,
		Command:      p.command,
		Env:          s.allowlistedEnv(p.allowlist),
		EnvAllowlist: p.allowlist,
		NetworkMode:  p.network,
	}
	log, seq := s.commandStarted(id, p.command)
	s.signal(id, api.SignalStarted, "sandbox launched", nil)
	res, err := s.Backend.Launcher.Run(runCtx, spec)
	// A launcher that returned without error finished the command; a
	// cancel or deadline that landed after that does not unmake the result.
	cancelled := err != nil && ctx.Err() != nil
	timedOut := err != nil && !cancelled && errors.Is(runCtx.Err(), context.DeadlineExceeded)
	status, reason, exit := classify(res, err, cancelled, timedOut)
	if log != nil {
		_ = log.Ended(seq, cmdlog.End{ExitCode: exit, Status: commandStatus(status, exit), Reason: reason, Stdout: res.Stdout, Stderr: res.Stderr})
	}
	if (cancelled || timedOut) && s.Backend.Teardown != nil {
		tctx, cancel := context.WithTimeout(context.Background(), TeardownTimeout)
		if terr := s.Backend.Teardown(tctx, name); terr != nil {
			reason += "; teardown: " + s.scrub(terr.Error())
		}
		cancel()
	}
	stdout, stdoutCut := s.persistOutput(id, StdoutArtifact, res.Stdout, p.maxOutput)
	stderr, stderrCut := s.persistOutput(id, StderrArtifact, res.Stderr, p.maxOutput)
	finish(status, reason, func(d *detail) {
		d.ExitCode, d.TimedOut = exit, timedOut
		d.Stdout, d.StdoutTruncatedAt = stdout, stdoutCut
		d.Stderr, d.StderrTruncatedAt = stderr, stderrCut
	})
}

// signal appends a runner signal to run id; a failure is dropped, because
// signals report on the run and must never stop it.
func (s *Service) signal(id, kind, message string, data json.RawMessage) {
	if s.Signals == nil {
		return
	}
	_, _ = s.Signals.Append(id, s.Store.Path(id), signals.Signal{Kind: kind, Source: api.SignalSourceRunner, Message: message, Data: data})
}

// signalDone appends the run's done signal: its terminal status, the exit
// code when one was read and whether it timed out, with the reason as the
// message.
func (s *Service) signalDone(id string, d api.SignalDoneData, reason string) {
	raw, err := json.Marshal(d)
	if err != nil {
		return
	}
	s.signal(id, api.SignalDone, reason, raw)
}

// commandStarted appends the dispatched command to the run's command log
// and returns the log and seq to end it with; a nil log means no log is
// kept (CmdLog unset or the open failed, which must not stop the run).
func (s *Service) commandStarted(id string, command []string) (cmdlog.Recorder, int64) {
	if s.CmdLog == nil {
		return nil, 0
	}
	log, err := s.CmdLog(s.Store.Path(id))
	if err != nil {
		return nil, 0
	}
	seq, err := log.Started(cmdlog.Start{Command: command, Cwd: sandbox.DefaultWorkspaceMount, Capture: api.CommandCaptureRunner})
	if err != nil {
		return nil, 0
	}
	return log, seq
}

// commandStatus maps the run's outcome onto the command log's closed set.
// The run calls a non-zero exit "completed" (the command ran to its own
// end); the log says "failed", because the question it answers is whether
// the command succeeded. Unfinished is reserved for an exit the runtime
// never learned.
func commandStatus(runStatus string, exit *int) string {
	switch runStatus {
	case runstate.StatusCancelled:
		return api.CommandCancelled
	case runstate.StatusUnfinished:
		return api.CommandUnfinished
	case runstate.StatusCompleted:
		if exit != nil && *exit == 0 {
			return api.CommandCompleted
		}
		return api.CommandFailed
	}
	// The run failed around the command (timed out, refused, teardown):
	// the runtime ended it, so it is failed, not unfinished.
	return api.CommandFailed
}

// classify turns the launcher's answer into a status. The exit code is
// reported only when the launcher read one: a zero Result beside an error
// means the launcher refused before anything ran, and the launchers'
// exitUnknown means the process ended without a readable exit. Neither is
// ever turned into a guessed code.
func classify(res sandbox.Result, err error, cancelled, timedOut bool) (status, reason string, exit *int) {
	launched := err == nil || res != (sandbox.Result{})
	if launched && res.ExitCode != exitUnknown {
		code := res.ExitCode
		exit = &code
	}
	switch {
	case cancelled:
		return runstate.StatusCancelled, "cancelled by the caller", exit
	case timedOut:
		return runstate.StatusFailed, "timed out", exit
	case err == nil:
		return runstate.StatusCompleted, "", exit
	case exit != nil:
		// The command ran and its exit was read; the error is the backend's
		// own description (a Job that reports Failed on a non-zero exit).
		return runstate.StatusCompleted, logscrub.ScrubString(err.Error()), exit
	case !launched:
		return runstate.StatusFailed, logscrub.ScrubString(err.Error()), nil
	}
	return runstate.StatusUnfinished, logscrub.ScrubString(err.Error()), nil
}

// persistOutput scrubs raw, writes the whole of it as an artifact and
// returns the response copy, cut at max with the offset when it was.
func (s *Service) persistOutput(id, name, raw string, max int64) (string, *int64) {
	text := s.scrub(raw)
	_ = os.WriteFile(filepath.Join(s.Store.Path(id), name), []byte(text), 0o660)
	if int64(len(text)) <= max {
		return text, nil
	}
	cut := max
	return text[:max], &cut
}

func (s *Service) scrub(v string) string {
	if s.Scrub != nil {
		return s.Scrub(v)
	}
	return logscrub.ScrubString(v)
}

// allowlistedEnv is the explicit environment the container receives: the
// allowlisted names present in the service's environment, credentials
// excluded by sandbox.SanitizedEnv. The podman launcher forwards only
// explicit entries into the container, so the allowlist alone is not
// enough.
func (s *Service) allowlistedEnv(allowlist []string) map[string]string {
	environ := os.Environ
	if s.Environ != nil {
		environ = s.Environ
	}
	out := map[string]string{}
	for _, entry := range sandbox.SanitizedEnv(environ(), allowlist, nil) {
		if k, v, ok := strings.Cut(entry, "="); ok {
			out[k] = v
		}
	}
	return out
}

func toWire(rec runstate.Record) api.ContainedRun {
	var d detail
	_ = json.Unmarshal(rec.Detail, &d)
	return api.ContainedRun{
		ID: rec.ID, Status: rec.Status, Owner: rec.Owner, Reason: rec.Reason,
		CreatedAt: rec.CreatedAt, StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt,
		Profile: d.Profile, Network: d.Network, Backend: d.Backend, Command: d.Command, Checkout: d.Checkout, Workdir: d.Workspace,
		ExitCode: d.ExitCode, TimedOut: d.TimedOut, Stdout: d.Stdout, Stderr: d.Stderr,
		StdoutTruncatedAt: d.StdoutTruncatedAt, StderrTruncatedAt: d.StderrTruncatedAt,
		RestartedFrom: d.RestartedFrom, RestartedBy: d.RestartedBy,
	}
}

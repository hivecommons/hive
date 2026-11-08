package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hivecommons/hive/crux/agentstep"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/runstate"
)

// session runs an agent-profile run (S6): it provisions the workspace,
// marks the run waiting and holds it open for steps until it is cancelled.
// The steps themselves run through RunStep under this goroutine's ctx.
func (s *Service) session(ctx context.Context, id string, p plan, a *active) {
	defer func() {
		s.mu.Lock()
		delete(s.active, id)
		s.mu.Unlock()
		close(a.done)
	}()
	finish := func(status, reason string) {
		_, _ = s.Store.Update(id, func(rec *runstate.Record) error {
			rec.Status, rec.Reason = status, reason
			return nil
		})
		s.signalDone(id, api.SignalDoneData{Status: status}, reason)
	}
	workspace := p.workspace
	if p.worktree != nil {
		prov := s.Provisioner
		if prov == nil {
			prov = GitProvisioner{}
		}
		path, err := prov.Provision(ctx, s.Store.Path(id), *p.worktree)
		if err != nil {
			a.readyErr = ErrNotSession
			close(a.ready)
			if ctx.Err() != nil {
				finish(runstate.StatusCancelled, "cancelled while provisioning the worktree")
			} else {
				finish(runstate.StatusFailed, "provisioning the worktree: "+s.scrub(err.Error()))
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
		rec.Status, rec.Detail = runstate.StatusWaiting, raw
		return err
	}); err != nil {
		a.readyErr = ErrNotSession
		close(a.ready)
		finish(runstate.StatusFailed, "recording the start: "+err.Error())
		return
	}
	s.signal(id, api.SignalStarted, "agent step session open", nil)
	close(a.ready)
	<-ctx.Done()
	// Wait for a step in flight to record its own cancellation first.
	a.busy <- struct{}{}
	steps, _ := agentstep.Store{RunDir: s.Store.Path(id)}.List()
	finish(runstate.StatusCancelled, fmt.Sprintf("session closed after %d step(s)", len(steps)))
}

// sessionFor is the open session id belongs to, or ErrNotSession.
func (s *Service) sessionFor(ctx context.Context, id string) (*active, error) {
	rec, err := s.Store.Get(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	a := s.active[id]
	s.mu.Unlock()
	if rec.Kind != runstate.KindRun || a == nil || a.ready == nil {
		return nil, ErrNotSession
	}
	select {
	case <-a.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if a.readyErr != nil || a.ctx.Err() != nil {
		return nil, ErrNotSession
	}
	return a, nil
}

// RunStep executes one agent step inside session id and returns its
// record. One step runs at a time: a second concurrent post is ErrStepBusy.
// The step runs under the session's context, so cancelling the run ends it.
func (s *Service) RunStep(ctx context.Context, id string, req api.StepRequest) (api.Step, error) {
	a, err := s.sessionFor(ctx, id)
	if err != nil {
		return api.Step{}, err
	}
	select {
	case a.busy <- struct{}{}:
	default:
		return api.Step{}, ErrStepBusy
	}
	defer func() { <-a.busy }()
	if a.ctx.Err() != nil {
		return api.Step{}, ErrNotSession
	}
	rec, err := s.Store.Get(id)
	if err != nil {
		return api.Step{}, err
	}
	var d detail
	_ = json.Unmarshal(rec.Detail, &d)
	setStatus := func(status string) {
		_, _ = s.Store.Update(id, func(rec *runstate.Record) error {
			if runstate.Terminal(rec.Status) {
				return nil
			}
			rec.Status = status
			return nil
		})
	}
	setStatus(runstate.StatusRunning)
	defer setStatus(runstate.StatusWaiting)
	host := agentstep.Host{
		Run: id, RunDir: s.Store.Path(id), Workspace: d.Workspace,
		Image: s.Config.Image, Network: d.Network,
		Env: s.allowlistedEnv(d.EnvAllowlist), EnvAllowlist: d.EnvAllowlist,
		Launcher: s.Backend.Launcher, Teardown: s.Backend.Teardown,
		Timeout:    time.Duration(s.Config.DefaultTimeoutMs) * time.Millisecond,
		MaxTimeout: time.Duration(s.Config.MaxTimeoutMs) * time.Millisecond,
		Signal:     func(kind, msg string, data json.RawMessage) { s.signal(id, kind, msg, data) },
		Scrub:      s.scrub,
	}
	if s.CmdLog != nil {
		if log, err := s.CmdLog(host.RunDir); err == nil {
			host.CmdLog = log
		}
	}
	// A caller that goes away does not end the step; the session's cancel
	// does.
	return agentstep.Run(a.ctx, host, req)
}

// Steps lists run id's steps, oldest first.
func (s *Service) Steps(id string) (api.StepList, error) {
	if _, err := s.Store.Get(id); err != nil {
		return api.StepList{}, err
	}
	steps, err := agentstep.Store{RunDir: s.Store.Path(id)}.List()
	if err != nil {
		return api.StepList{}, err
	}
	return api.StepList{Run: id, Steps: steps}, nil
}

// Step is step n of run id.
func (s *Service) Step(id string, n int) (api.Step, error) {
	if _, err := s.Store.Get(id); err != nil {
		return api.Step{}, err
	}
	return agentstep.Store{RunDir: s.Store.Path(id)}.Get(n)
}

package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/gates"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/signals"
)

// Human gates (hivecommons/crux#114 S3) are a work-services family: a
// project principal may open gates and read or wait on the ones it opened;
// only the operator answers, and the principal that opened a gate answers
// it only with the explicit, audited as_operator override.

const (
	// DefaultGateWait is the long-poll window of GET /v1/gates/{id}/wait
	// when the request names none.
	DefaultGateWait = 30 * time.Second
	// MaxGateWait bounds the window a request may ask for; a longer wait is
	// a loop of polls (crux gate wait does that).
	MaxGateWait = 5 * time.Minute
	// auditGateOverride is the audit action of an opener answering its own
	// gate under as_operator.
	auditGateOverride = "gate.operator_override"
)

// registerGates mounts the gate routes, marks them a work service and
// relays gate changes onto /v1/events.
func (s *Server) registerGates(mux *http.ServeMux) {
	mux.HandleFunc("POST "+api.PathGates, s.handleGateOpen)
	mux.HandleFunc("GET "+api.PathGates, s.handleGateList)
	mux.HandleFunc("GET "+api.PathGates+"/{id}", s.handleGateGet)
	mux.HandleFunc("POST "+api.PathGates+"/{id}/answer", s.handleGateAnswer)
	mux.HandleFunc("GET "+api.PathGates+"/{id}/wait", s.handleGateWait)
	s.workService(api.PathGates)
	if s.Gates != nil {
		s.Gates.SetObserver(s.publishGate)
	}
}

// gatesCaller is the request's principal as the gate service sees it. A
// context that did not pass authenticate is the operator, as for runs.
func (s *Server) gatesCaller(r *http.Request) gates.Caller {
	p, ok := CallerFrom(r.Context())
	if !ok {
		p = s.operator()
	}
	return gates.Caller{Name: p.Name, Principal: p.String(), Operator: p.IsOperator()}
}

// gateService answers 501 when the server has no gate service.
func (s *Server) gateService(w http.ResponseWriter) (*gates.Service, bool) {
	if s.Gates == nil {
		s.writeError(w, http.StatusNotImplemented, api.CodeNotSupported, "this server has no gate service")
		return nil, false
	}
	return s.Gates, true
}

func (s *Server) handleGateOpen(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.gateService(w)
	if !ok {
		return
	}
	var in api.GateRequest
	if !s.readJSON(w, r, &in) {
		return
	}
	timeout, err := gates.ParseTimeout(in.Timeout)
	if err != nil {
		s.writeGateError(w, err)
		return
	}
	in.Run = strings.TrimSpace(in.Run)
	if in.Run != "" && !s.gateRunUsable(w, r, in.Run) {
		return
	}
	rec, created, err := svc.Open(s.gatesCaller(r), gates.Request{
		Subject: in.Subject, Context: in.Context, Choices: in.Choices, Timeout: timeout,
		OpenedBy: in.OpenedBy, IdempotencyKey: in.IdempotencyKey, Run: in.Run,
	})
	if err != nil {
		s.writeGateError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, gateToAPI(rec))
}

// gateRunUsable checks the run a gate is opened for: the server must keep
// runs, the run must exist and the caller must own it (or be the
// operator), the same rule as reading the run. On failure the response
// has been written.
func (s *Server) gateRunUsable(w http.ResponseWriter, r *http.Request, id string) bool {
	if s.Runs == nil {
		s.writeError(w, http.StatusNotImplemented, api.CodeNotSupported, "this server has no contained-run service; open the gate without run")
		return false
	}
	run, err := s.Runs.Get(id)
	switch {
	case errors.Is(err, runs.ErrNotFound):
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, fmt.Sprintf("run %q not found", id))
		return false
	case err != nil:
		s.writeRunsError(w, err)
		return false
	}
	if caller := s.runsCaller(r); !caller.Operator && run.Owner != caller.Name {
		s.writeError(w, http.StatusForbidden, api.CodeForbidden, fmt.Sprintf("run %s is owned by %q, not by %q", run.ID, run.Owner, caller.Name))
		return false
	}
	return true
}

func (s *Server) handleGateList(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.gateService(w)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", gates.StatusWaiting, gates.StatusAnswered, gates.StatusExpired:
	default:
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, fmt.Sprintf("status %q is not waiting, answered or expired", status))
		return
	}
	caller := s.gatesCaller(r)
	recs, err := svc.List(gates.ListFilter{Status: status, Caller: &caller})
	if err != nil {
		s.writeGateError(w, err)
		return
	}
	out := api.GateList{Gates: make([]api.Gate, 0, len(recs))}
	for _, rec := range recs {
		out.Gates = append(out.Gates, gateToAPI(rec))
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGateGet(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.gateService(w)
	if !ok {
		return
	}
	if rec, ok := s.visibleGate(w, r, svc); ok {
		s.writeJSON(w, http.StatusOK, gateToAPI(rec))
	}
}

func (s *Server) handleGateAnswer(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.gateService(w)
	if !ok {
		return
	}
	var in api.GateAnswerRequest
	if !s.readJSON(w, r, &in) {
		return
	}
	rec, err := svc.Answer(s.gatesCaller(r), r.PathValue("id"), gates.Answer{Choice: in.Answer, Note: in.Note, Override: in.AsOperator})
	if err != nil {
		s.writeGateError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, gateToAPI(rec))
}

func (s *Server) handleGateWait(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.gateService(w)
	if !ok {
		return
	}
	window := DefaultGateWait
	if v := r.URL.Query().Get("timeout"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, fmt.Sprintf("timeout %q is not a positive duration", v))
			return
		}
		window = min(d, MaxGateWait)
	}
	if _, ok := s.visibleGate(w, r, svc); !ok {
		return
	}
	rec, err := svc.Wait(r.Context(), r.PathValue("id"), window)
	if err != nil {
		if r.Context().Err() != nil {
			return // the caller went away; nothing to answer
		}
		s.writeGateError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, gateToAPI(rec))
}

// visibleGate loads {id} and enforces that the caller opened it or is the
// operator; on failure the response has been written.
func (s *Server) visibleGate(w http.ResponseWriter, r *http.Request, svc *gates.Service) (gates.Record, bool) {
	rec, err := svc.Get(r.PathValue("id"))
	if err != nil {
		s.writeGateError(w, err)
		return gates.Record{}, false
	}
	if caller := s.gatesCaller(r); !gates.Visible(rec, caller) {
		s.writeError(w, http.StatusForbidden, api.CodeForbidden, fmt.Sprintf("gate %s was opened by %s, not by %s", rec.ID, rec.OpenedByPrincipal, caller.Principal))
		return gates.Record{}, false
	}
	return rec, true
}

// writeGateError maps the package's sentinels to typed codes.
func (s *Server) writeGateError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, api.CodeRuntime
	switch {
	case errors.Is(err, gates.ErrBadRequest):
		status, code = http.StatusBadRequest, api.CodeBadRequest
	case errors.Is(err, gates.ErrContextTooLarge):
		status, code = http.StatusRequestEntityTooLarge, api.CodeGateContextTooLarge
	case errors.Is(err, gates.ErrNotFound):
		status, code = http.StatusNotFound, api.CodeNotFound
	case errors.Is(err, gates.ErrInvalidChoice):
		status, code = http.StatusUnprocessableEntity, api.CodeGateInvalidChoice
	case errors.Is(err, gates.ErrSelfAnswer):
		status, code = http.StatusForbidden, api.CodeGateSelfAnswer
	case errors.Is(err, gates.ErrForbidden):
		status, code = http.StatusForbidden, api.CodeForbidden
	case errors.Is(err, gates.ErrClosed):
		status, code = http.StatusConflict, api.CodeGateClosed
	}
	s.writeError(w, status, code, err.Error())
}

// publishGate relays one durable gate change onto /v1/events, without the
// context payload, audits an operator override and, for a gate opened for
// a run, appends the matching turn signal to that run.
func (s *Server) publishGate(op string, rec gates.Record) {
	typ := api.EventGateOpened
	switch op {
	case gates.OpAnswered:
		typ = api.EventGateAnswered
	case gates.OpExpired:
		typ = api.EventGateExpired
	}
	ev := gateToAPI(rec)
	ev.Context = nil
	s.publishData(typ, "", ev)
	if op == gates.OpAnswered && rec.OperatorOverride {
		auditTee{s}.Record(rec.AnsweredByPrincipal, auditGateOverride, "", map[string]any{
			"gate": rec.ID, "answer": rec.Answer, "opened_by_principal": rec.OpenedByPrincipal,
		})
	}
	s.gateSignal(op, rec)
}

// gateSignal maps a gate change onto its run's turn signals: opened is
// needs-input (the subject is the message), answered is input with the
// answer, expired is blocked. The note and context stay with the gate. A
// failure — no signal book, a run already done — is logged, never
// returned: the gate change is already durable.
func (s *Server) gateSignal(op string, rec gates.Record) {
	if rec.Run == "" || s.Runs == nil || s.Runs.Signals == nil {
		return
	}
	sig := signals.Signal{Source: api.SignalSourceGate}
	data := api.SignalGateData{Gate: rec.ID}
	switch op {
	case gates.OpOpened:
		sig.Kind, sig.Message = api.SignalNeedsInput, rec.Subject
	case gates.OpAnswered:
		sig.Kind, sig.Message = api.SignalInput, "gate answered: "+rec.Answer
		data.Answer, data.AnsweredBy = rec.Answer, rec.AnsweredBy
	case gates.OpExpired:
		sig.Kind, sig.Message = api.SignalBlocked, "gate expired with no answer"
	default:
		return
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	sig.Data = raw
	if _, err := s.Runs.AppendSignal(rec.Run, sig); err != nil {
		s.Logger.Warn("crux serve: gate turn signal", "gate", rec.ID, "run", rec.Run, "err", err)
	}
}

func gateToAPI(rec gates.Record) api.Gate {
	return api.Gate{
		ID: rec.ID, Status: rec.Status, Subject: rec.Subject, Context: rec.Context, Choices: rec.Choices, Timeout: rec.Timeout,
		OpenedBy: rec.OpenedBy, OpenedByPrincipal: rec.OpenedByPrincipal, OpenedAt: rec.OpenedAt, ExpiresAt: rec.ExpiresAt, Run: rec.Run,
		Answer: rec.Answer, AnsweredBy: rec.AnsweredBy, AnsweredByPrincipal: rec.AnsweredByPrincipal, AnsweredAt: rec.AnsweredAt,
		Note: rec.Note, OperatorOverride: rec.OperatorOverride, ExpiredAt: rec.ExpiredAt,
	}
}

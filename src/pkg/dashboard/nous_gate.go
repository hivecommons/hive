package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Gate protocol seams (RFC #11585 part 3, #11696).
//
// The dashboard owns the request/decision shapes and the mirroring and
// notification rules; cmd/hive wires the on-disk format (pkg/nousstore)
// through NousState.GateStore so this package stays free of storage
// concerns, exactly like PersistPanel. With GateStore unset every handler
// keeps its pre-#11696 in-memory behaviour.

// NousGateRequest is a gate the experimentation agent is blocked on.
type NousGateRequest struct {
	ID          string
	Gate        string
	Question    string
	Scope       string
	Summary     string
	Artifact    json.RawMessage
	Reviews     json.RawMessage
	RequestedAt time.Time
}

// NousGateDecision is the owner's answer to a NousGateRequest.
type NousGateDecision struct {
	GateID    string
	Decision  string
	Reason    string
	DecidedBy string
	DecidedAt time.Time
}

// NousGateStore is the durable gate protocol store the dashboard mirrors.
type NousGateStore interface {
	// PendingGate returns the gate the agent is waiting on, if any. An
	// error means a file exists but is unusable; the caller logs it and
	// leaves the last good mirror in place.
	PendingGate() (NousGateRequest, bool, error)
	// RecordGateDecision persists d and clears the pending gate.
	RecordGateDecision(d NousGateDecision) error
	// LastNotifiedGate / MarkGateNotified dedupe the owner ping across
	// restarts: one notification per gate id, ever.
	LastNotifiedGate() string
	MarkGateNotified(id string) error
}

// Gate decisions the owner may record. Mirrors nousstore's constants
// without importing the package.
const (
	NousGateApprove = "approve"
	NousGateReject  = "reject"
	NousGateAbort   = "abort"
)

// Keys of the GatePending / GateResponse maps served by /api/nous/gate-*.
const (
	nousGateKeyID          = "id"
	nousGateKeyGate        = "gate"
	nousGateKeyQuestion    = "question"
	nousGateKeyScope       = "scope"
	nousGateKeySummary     = "summary"
	nousGateKeyArtifact    = "artifact"
	nousGateKeyReviews     = "reviews"
	nousGateKeyRequestedAt = "requested_at"
	nousGateKeyGateID      = "gate_id"
	nousGateKeyDecision    = "decision"
	nousGateKeyReason      = "reason"
	nousGateKeyDecidedBy   = "decided_by"
	nousGateKeyDecidedAt   = "decided_at"
)

// Owner notification copy. The question is truncated so a long design
// summary cannot blow past ntfy/Slack message limits.
const (
	nousGateNotifyTitle       = "Experiment Lab: gate decision needed"
	nousGateNotifyMaxQuestion = 400
	nousGateNotifyEllipsis    = "…"
)

// ValidNousGateDecision reports whether s is approve, reject or abort.
func ValidNousGateDecision(s string) bool {
	switch s {
	case NousGateApprove, NousGateReject, NousGateAbort:
		return true
	}
	return false
}

// nousGateNow returns the injected clock or time.Now.
func (ns *NousState) nousGateNow() time.Time {
	if ns.Now != nil {
		return ns.Now()
	}
	return time.Now()
}

// gateRequestMap renders a request as the GatePending map the API serves.
// Raw JSON passthroughs are decoded so the response is a plain object, not
// a base64 string.
func gateRequestMap(req NousGateRequest) map[string]interface{} {
	m := map[string]interface{}{
		nousGateKeyID:       req.ID,
		nousGateKeyQuestion: req.Question,
	}
	if req.Gate != "" {
		m[nousGateKeyGate] = req.Gate
	}
	if req.Scope != "" {
		m[nousGateKeyScope] = req.Scope
	}
	if req.Summary != "" {
		m[nousGateKeySummary] = req.Summary
	}
	if !req.RequestedAt.IsZero() {
		m[nousGateKeyRequestedAt] = req.RequestedAt.UTC().Format(time.RFC3339)
	}
	for key, raw := range map[string]json.RawMessage{nousGateKeyArtifact: req.Artifact, nousGateKeyReviews: req.Reviews} {
		if len(raw) == 0 {
			continue
		}
		var v interface{}
		if err := json.Unmarshal(raw, &v); err == nil {
			m[key] = v
		} else {
			m[key] = string(raw)
		}
	}
	return m
}

// gateDecisionMap renders a decision as the GateResponse map the API serves.
func gateDecisionMap(d NousGateDecision) map[string]interface{} {
	m := map[string]interface{}{
		nousGateKeyDecision: d.Decision,
		nousGateKeyReason:   d.Reason,
	}
	if d.GateID != "" {
		m[nousGateKeyGateID] = d.GateID
	}
	if d.DecidedBy != "" {
		m[nousGateKeyDecidedBy] = d.DecidedBy
	}
	if !d.DecidedAt.IsZero() {
		m[nousGateKeyDecidedAt] = d.DecidedAt.UTC().Format(time.RFC3339)
	}
	return m
}

// pendingGateID returns the id of the mirrored pending gate, or "".
func (ns *NousState) pendingGateID() string {
	id, _ := ns.GatePending[nousGateKeyID].(string)
	return id
}

// SyncGate re-reads the pending gate from GateStore, mirrors it into
// GatePending and pings the owner once for a gate id that has not been
// announced before. It is a no-op without a GateStore. Callers must NOT
// hold Mu. The returned error is the store's read error, for logging.
func (ns *NousState) SyncGate() error {
	if ns == nil || ns.GateStore == nil {
		return nil
	}
	req, ok, err := ns.GateStore.PendingGate()
	if err != nil {
		return err
	}
	ns.Mu.Lock()
	if !ok {
		if ns.GatePending != nil && len(ns.GatePending) > 0 {
			ns.GatePending = map[string]interface{}{}
		}
		ns.Mu.Unlock()
		return nil
	}
	ns.GatePending = gateRequestMap(req)
	ns.Mu.Unlock()
	ns.notifyGateOnce(req)
	return nil
}

// notifyGateOnce pings the owner for req unless that gate id was already
// announced (in memory or in the store's marker). The marker is written
// before the ping so a notifier that blocks cannot cause a double send.
func (ns *NousState) notifyGateOnce(req NousGateRequest) {
	if ns.GateNotify == nil || req.ID == "" {
		return
	}
	ns.gateNotifyMu.Lock()
	defer ns.gateNotifyMu.Unlock()
	if req.ID == ns.lastNotifiedGate || req.ID == ns.GateStore.LastNotifiedGate() {
		ns.lastNotifiedGate = req.ID
		return
	}
	ns.lastNotifiedGate = req.ID
	if err := ns.GateStore.MarkGateNotified(req.ID); err != nil && ns.gateLogger() != nil {
		ns.gateLogger().Warn("nous: failed to record gate notification marker", "gate", req.ID, "error", err)
	}
	ns.GateNotify(nousGateNotifyTitle, nousGateNotifyMessage(req))
}

// nousGateNotifyMessage is the body of the owner ping.
func nousGateNotifyMessage(req NousGateRequest) string {
	question := strings.TrimSpace(req.Question)
	if len(question) > nousGateNotifyMaxQuestion {
		question = question[:nousGateNotifyMaxQuestion] + nousGateNotifyEllipsis
	}
	var b strings.Builder
	if req.Gate != "" {
		b.WriteString(req.Gate)
		b.WriteString(" gate")
	} else {
		b.WriteString("Gate")
	}
	if req.Scope != "" {
		fmt.Fprintf(&b, " (%s scope)", req.Scope)
	}
	b.WriteString(" is waiting for an owner decision: ")
	b.WriteString(question)
	b.WriteString(" — answer with approve, reject or abort in the Experiment Lab.")
	return b.String()
}

// RecordGateDecision validates and records the owner's answer to the
// pending gate. With a GateStore the decision is persisted and the pending
// gate must exist (and match gateID when one is given); without one the
// pre-#11696 in-memory behaviour is kept. The returned decision is what
// was recorded. Callers must NOT hold Mu.
func (ns *NousState) RecordGateDecision(gateID, decision, reason, decidedBy string) (NousGateDecision, error) {
	if !ValidNousGateDecision(decision) {
		return NousGateDecision{}, &nousGateError{status: http.StatusBadRequest, msg: fmt.Sprintf("decision must be %s, %s or %s", NousGateApprove, NousGateReject, NousGateAbort)}
	}
	d := NousGateDecision{GateID: gateID, Decision: decision, Reason: reason, DecidedBy: decidedBy, DecidedAt: ns.nousGateNow()}
	if ns.GateStore == nil {
		ns.Mu.Lock()
		if ns.GatePending == nil {
			ns.GatePending = make(map[string]interface{})
		}
		if d.GateID == "" {
			d.GateID = ns.pendingGateID()
		}
		ns.GateResponse = gateDecisionMap(d)
		ns.Mu.Unlock()
		return d, nil
	}
	if err := ns.SyncGate(); err != nil {
		return NousGateDecision{}, &nousGateError{status: http.StatusInternalServerError, msg: "read pending gate: " + err.Error()}
	}
	ns.Mu.Lock()
	pendingID := ns.pendingGateID()
	ns.Mu.Unlock()
	if pendingID == "" {
		return NousGateDecision{}, &nousGateError{status: http.StatusConflict, msg: "no gate is pending"}
	}
	if d.GateID == "" {
		d.GateID = pendingID
	} else if d.GateID != pendingID {
		return NousGateDecision{}, &nousGateError{status: http.StatusConflict, msg: fmt.Sprintf("gate %s is no longer pending (current: %s)", d.GateID, pendingID)}
	}
	if err := ns.GateStore.RecordGateDecision(d); err != nil {
		return NousGateDecision{}, &nousGateError{status: http.StatusInternalServerError, msg: "record gate decision: " + err.Error()}
	}
	ns.Mu.Lock()
	ns.GatePending = map[string]interface{}{}
	ns.GateResponse = gateDecisionMap(d)
	ns.Mu.Unlock()
	return d, nil
}

// nousGateError carries the HTTP status a gate failure maps to.
type nousGateError struct {
	status int
	msg    string
}

func (e *nousGateError) Error() string { return e.msg }

// nousGateStatus returns the HTTP status for err (500 for unknown errors).
func nousGateStatus(err error) int {
	if ge, ok := err.(*nousGateError); ok {
		return ge.status
	}
	return http.StatusInternalServerError
}

func (ns *NousState) gateLogger() *slog.Logger { return ns.GateLogger }

// RunNousGateWatch re-syncs the pending gate on every tick until ctx ends.
// The tick channel is injected so tests drive it without sleeping; the
// production caller passes a time.Ticker's channel. Read errors are logged
// once per distinct message rather than on every tick.
func RunNousGateWatch(ctx context.Context, ns *NousState, ticks <-chan time.Time, logger *slog.Logger) {
	if ns == nil || ns.GateStore == nil {
		return
	}
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			err := ns.SyncGate()
			switch {
			case err == nil:
				lastErr = ""
			case err.Error() != lastErr:
				lastErr = err.Error()
				if logger != nil {
					logger.Warn("nous: pending gate unreadable", "error", err)
				}
			}
		}
	}
}

// StartNousGateWatch runs RunNousGateWatch on a ticker with the given
// interval in a goroutine; it returns immediately. A non-positive interval
// disables the watch (GET /api/nous/gate-pending still syncs on demand).
func StartNousGateWatch(ctx context.Context, ns *NousState, interval time.Duration, logger *slog.Logger) {
	if ns == nil || ns.GateStore == nil || interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		RunNousGateWatch(ctx, ns, ticker.C, logger)
	}()
}

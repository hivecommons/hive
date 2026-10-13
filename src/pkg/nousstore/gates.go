package nousstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Gate protocol (RFC #11585 part 3, #11696).
//
// Nous pauses at human gates (design review, findings review). The agent
// side cannot hold an owner credential, so the question is not posted to
// the owner-only HTTP API; it is published as a file under <root>/gates and
// the hive reads it:
//
//  1. The agent (bin/nous-hive-gate.py, or any engine honouring the
//     contract) writes GatePendingFile atomically.
//  2. The hive polls it, mirrors it into NousState.GatePending (served by
//     GET /api/nous/gate-pending) and pings the owner once per gate id.
//  3. The owner answers through PUT /api/nous/gate-decision. The hive
//     records the decision in GateDecisionFile, appends it to
//     GateHistoryFile and removes GatePendingFile.
//  4. The agent polls GateDecisionFile until the gate_id matches its own.
//
// Nothing here approves anything by itself: a missing or malformed decision
// is simply "still pending".
const (
	// GatePendingFile is the gate the agent is currently blocked on.
	GatePendingFile = "pending.json"

	// GateDecisionFile is the owner's most recent answer.
	GateDecisionFile = "decision.json"

	// GateHistoryFile is the append-only record of every decision.
	GateHistoryFile = "history.jsonl"

	// GateNotifiedFile holds the id of the last gate the owner was pinged
	// for, so a restart does not ping again for a gate that is still open.
	GateNotifiedFile = "notified"
)

// Decisions an owner may record for a gate. Anything else is rejected.
const (
	GateApprove = "approve"
	GateReject  = "reject"
	GateAbort   = "abort"
)

// gateIDPrefix is used when an agent publishes a gate without an id.
const gateIDPrefix = "gate-"

// ErrGateMalformed is returned when a pending-gate file exists but cannot
// be used (unparseable, or no question).
var ErrGateMalformed = errors.New("nous gate request malformed")

// ValidGateDecision reports whether s is one of GateApprove, GateReject or
// GateAbort.
func ValidGateDecision(s string) bool {
	switch s {
	case GateApprove, GateReject, GateAbort:
		return true
	}
	return false
}

// GateRequest is what the agent publishes when it reaches a gate.
type GateRequest struct {
	// ID identifies this gate; decisions carry it back so a stale answer
	// can never be applied to a newer gate.
	ID string `json:"id"`
	// Gate is the kind of gate (e.g. "design", "findings"); free-form.
	Gate     string `json:"gate,omitempty"`
	Question string `json:"question"`
	Scope    string `json:"scope,omitempty"`
	Summary  string `json:"summary,omitempty"`
	// Artifact and Reviews are passed through verbatim for the dashboard.
	Artifact    json.RawMessage `json:"artifact,omitempty"`
	Reviews     json.RawMessage `json:"reviews,omitempty"`
	RequestedAt time.Time       `json:"requested_at"`
}

// GateDecision is the owner's recorded answer to one GateRequest.
type GateDecision struct {
	GateID    string    `json:"gate_id"`
	Decision  string    `json:"decision"`
	Reason    string    `json:"reason,omitempty"`
	DecidedBy string    `json:"decided_by,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// GatesDir is <root>/gates.
func (l Layout) GatesDir() string { return filepath.Join(l.Root, GatesSubdir) }

// GatePendingPath is <root>/gates/pending.json.
func (l Layout) GatePendingPath() string { return filepath.Join(l.GatesDir(), GatePendingFile) }

// GateDecisionPath is <root>/gates/decision.json.
func (l Layout) GateDecisionPath() string { return filepath.Join(l.GatesDir(), GateDecisionFile) }

// GateHistoryPath is <root>/gates/history.jsonl.
func (l Layout) GateHistoryPath() string { return filepath.Join(l.GatesDir(), GateHistoryFile) }

// GateNotifiedPath is <root>/gates/notified.
func (l Layout) GateNotifiedPath() string { return filepath.Join(l.GatesDir(), GateNotifiedFile) }

// WriteGatePending publishes req as the pending gate, replacing any earlier
// one atomically. An empty ID is filled in from now; a zero RequestedAt is
// stamped with now. A request without a question is refused.
func (l Layout) WriteGatePending(req GateRequest, now time.Time) error {
	if strings.TrimSpace(req.Question) == "" {
		return fmt.Errorf("%w: question is required", ErrGateMalformed)
	}
	if strings.TrimSpace(req.ID) == "" {
		req.ID = gateIDPrefix + fmt.Sprint(now.UTC().UnixNano())
	}
	if req.RequestedAt.IsZero() {
		req.RequestedAt = now
	}
	req.RequestedAt = req.RequestedAt.UTC()
	data, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal nous gate request: %w", err)
	}
	if err := os.MkdirAll(l.GatesDir(), dirMode); err != nil {
		return fmt.Errorf("create nous gates dir %s: %w", l.GatesDir(), err)
	}
	return writeAtomic(l.GatePendingPath(), data)
}

// LoadGatePending reads the pending gate. The boolean is false when no gate
// is pending. A file that exists but cannot be used is an ErrGateMalformed
// error so a broken agent is reported rather than silently ignored.
func (l Layout) LoadGatePending() (GateRequest, bool, error) {
	var req GateRequest
	data, err := os.ReadFile(l.GatePendingPath())
	if errors.Is(err, fs.ErrNotExist) {
		return req, false, nil
	}
	if err != nil {
		return req, false, fmt.Errorf("read nous gate request: %w", err)
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return GateRequest{}, false, fmt.Errorf("%w: parse %s: %v", ErrGateMalformed, l.GatePendingPath(), err)
	}
	if strings.TrimSpace(req.Question) == "" {
		return GateRequest{}, false, fmt.Errorf("%w: %s has no question", ErrGateMalformed, l.GatePendingPath())
	}
	if strings.TrimSpace(req.ID) == "" {
		// An agent that omits the id still gets a stable one: the file's
		// mtime does not change while the gate is pending.
		if st, statErr := os.Stat(l.GatePendingPath()); statErr == nil {
			req.ID = gateIDPrefix + fmt.Sprint(st.ModTime().UTC().UnixNano())
		}
	}
	return req, true, nil
}

// ClearGatePending removes the pending gate file. A missing file is not an
// error.
func (l Layout) ClearGatePending() error {
	if err := os.Remove(l.GatePendingPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove nous gate request: %w", err)
	}
	return nil
}

// RecordGateDecision stores d as the current decision, appends it to the
// history and clears the pending gate. The decision must be valid and name
// a gate id; DecidedAt is stamped with now when zero. History is written
// first so an interrupted call can lose the live decision file but never
// the audit trail.
func (l Layout) RecordGateDecision(d GateDecision, now time.Time) error {
	if !ValidGateDecision(d.Decision) {
		return fmt.Errorf("invalid nous gate decision %q", d.Decision)
	}
	if strings.TrimSpace(d.GateID) == "" {
		return errors.New("nous gate decision has no gate_id")
	}
	if d.DecidedAt.IsZero() {
		d.DecidedAt = now
	}
	d.DecidedAt = d.DecidedAt.UTC()
	line, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("marshal nous gate decision: %w", err)
	}
	if err := os.MkdirAll(l.GatesDir(), dirMode); err != nil {
		return fmt.Errorf("create nous gates dir %s: %w", l.GatesDir(), err)
	}
	f, err := os.OpenFile(l.GateHistoryPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("open nous gate history: %w", err)
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("append nous gate history: %w", werr)
	}
	if err := writeAtomic(l.GateDecisionPath(), append(line, '\n')); err != nil {
		return err
	}
	return l.ClearGatePending()
}

// LoadGateDecision reads the most recent decision. The boolean is false
// when none has been recorded.
func (l Layout) LoadGateDecision() (GateDecision, bool, error) {
	var d GateDecision
	data, err := os.ReadFile(l.GateDecisionPath())
	if errors.Is(err, fs.ErrNotExist) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("read nous gate decision: %w", err)
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return GateDecision{}, false, fmt.Errorf("parse nous gate decision %s: %w", l.GateDecisionPath(), err)
	}
	return d, true, nil
}

// GateHistory returns every recorded decision, oldest first. Unparseable
// lines are skipped so one bad write cannot hide the rest of the record.
func (l Layout) GateHistory() ([]GateDecision, error) {
	data, err := os.ReadFile(l.GateHistoryPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read nous gate history: %w", err)
	}
	var out []GateDecision
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var d GateDecision
		if json.Unmarshal([]byte(line), &d) == nil {
			out = append(out, d)
		}
	}
	return out, nil
}

// LastNotifiedGate returns the id of the last gate the owner was pinged
// for, or "" when none (or the marker is unreadable).
func (l Layout) LastNotifiedGate() string {
	data, err := os.ReadFile(l.GateNotifiedPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// MarkGateNotified records id as the last gate the owner was pinged for.
func (l Layout) MarkGateNotified(id string) error {
	if err := os.MkdirAll(l.GatesDir(), dirMode); err != nil {
		return fmt.Errorf("create nous gates dir %s: %w", l.GatesDir(), err)
	}
	return writeAtomic(l.GateNotifiedPath(), []byte(id+"\n"))
}

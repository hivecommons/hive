// Package gates is the human-gate work service (hivecommons/crux#114 S3): a
// driver says "I need a person to decide something before I continue", a
// person answers from the terminal or the API, and the answer is recorded
// and survives a restart of the serving process.
//
// The persistence model is toolapprove.Inbox's: one record per gate, an
// idempotency key derived from the request (the DeriveIdempotencyKey
// pattern) so re-opening the same gate yields the same id and the recorded
// answer, pending items in a durable directory, and resolved records kept
// forever beside an append-only journal:
//
//	<dir>/pending/<id>.json   a gate still waiting for an answer
//	<dir>/resolved/<id>.json  an answered or expired gate, never removed
//	<dir>/gates.jsonl         opened/answered/expired, one line per change
//
// Answers are constrained to the offered choices; free text rides beside
// the choice as a note. Expiry is its own status, neither approval nor
// denial: the driver decides what it means. Subject, note and the string
// leaves of the context payload pass through logscrub before they are
// stored, and the journal carries none of them.
package gates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Statuses. The set is closed.
const (
	StatusWaiting  = "waiting"
	StatusAnswered = "answered"
	StatusExpired  = "expired"
)

const (
	// DirName is the per-concern directory under the work dir.
	DirName = "gates"
	// PendingDirName holds one record per waiting gate.
	PendingDirName = "pending"
	// ResolvedDirName holds one record per answered or expired gate.
	ResolvedDirName = "resolved"
	// JournalName is the append-only decision journal inside Dir.
	JournalName = "gates.jsonl"
	// IDPrefix starts every gate id.
	IDPrefix = "gate_"
	// MaxContextBytes bounds the context payload after JSON encoding.
	MaxContextBytes = 64 << 10
	// MaxSubjectLen, MaxNoteLen, MaxChoices and MaxChoiceLen bound the
	// rest of a gate so a record stays small.
	MaxSubjectLen = 1024
	MaxNoteLen    = 8 << 10
	MaxChoices    = 16
	MaxChoiceLen  = 64
	// MaxOpenedByLen bounds the driver's own label.
	MaxOpenedByLen = 128
	// MaxRunLen bounds the contained-run id a gate is opened for.
	MaxRunLen = 64

	idRandomBytes = 8
	maxIDLen      = 64
	dirMode       = 0o700
	fileMode      = 0o600
)

// Sentinel errors; Service methods wrap them so callers map them to typed
// API codes with errors.Is.
var (
	// ErrBadRequest is a malformed request (empty subject, bad timeout,
	// duplicate choices, context that is not JSON).
	ErrBadRequest = errors.New("gates: bad request")
	// ErrContextTooLarge is a context payload over MaxContextBytes.
	ErrContextTooLarge = errors.New("gates: context too large")
	// ErrNotFound is an unknown gate id.
	ErrNotFound = errors.New("gates: not found")
	// ErrInvalidChoice is an answer that is not one of the gate's choices.
	ErrInvalidChoice = errors.New("gates: answer is not one of the offered choices")
	// ErrSelfAnswer is the opener answering its own gate without the
	// explicit operator override.
	ErrSelfAnswer = errors.New("gates: the opener may not answer its own gate")
	// ErrForbidden is a caller that may not answer gates at all.
	ErrForbidden = errors.New("gates: forbidden")
	// ErrClosed is an answer to a gate that is already answered or expired.
	ErrClosed = errors.New("gates: gate is closed")
)

// DefaultChoices are offered when a request names none.
var DefaultChoices = []string{"approve", "deny"}

// Caller is the authenticated identity behind a call. Principal is the
// unique form ("operator:kelly", "project:atlas") the opener check compares;
// Name is what answered_by records.
type Caller struct {
	Name      string
	Principal string
	Operator  bool
}

// Request is what a driver asks for.
type Request struct {
	Subject string
	// Context is a driver-defined JSON payload, opaque to crux.
	Context json.RawMessage
	Choices []string
	// Timeout is how long the gate waits; zero waits indefinitely.
	Timeout time.Duration
	// OpenedBy is the driver's own label ("nous-campaign-42"); the
	// authenticated principal is recorded separately.
	OpenedBy string
	// IdempotencyKey, when set, identifies the gate instead of the request
	// contents; it is scoped to the opening principal.
	IdempotencyKey string
	// Run, when set, is the contained run (S2) the gate holds up; crux
	// serve checks the opener owns it and appends the gate's turn signals
	// (S5) to it. The gate service itself only records it.
	Run string
}

// Answer is a person's decision on a gate.
type Answer struct {
	Choice string
	Note   string
	// Override is the explicit, audited operator override that lets the
	// opener answer its own gate.
	Override bool
}

// Record is one gate as stored. Context never leaves the record for the
// journal or the event stream.
type Record struct {
	ID                  string          `json:"id"`
	Status              string          `json:"status"`
	Subject             string          `json:"subject"`
	Context             json.RawMessage `json:"context,omitempty"`
	Choices             []string        `json:"choices"`
	Timeout             string          `json:"timeout"`
	OpenedBy            string          `json:"opened_by"`
	OpenedByPrincipal   string          `json:"opened_by_principal"`
	OpenedAt            time.Time       `json:"opened_at"`
	ExpiresAt           *time.Time      `json:"expires_at,omitempty"`
	IdempotencyKey      string          `json:"idempotency_key"`
	Run                 string          `json:"run,omitempty"`
	Answer              string          `json:"answer,omitempty"`
	AnsweredBy          string          `json:"answered_by,omitempty"`
	AnsweredByPrincipal string          `json:"answered_by_principal,omitempty"`
	AnsweredAt          *time.Time      `json:"answered_at,omitempty"`
	Note                string          `json:"note,omitempty"`
	OperatorOverride    bool            `json:"operator_override,omitempty"`
	ExpiredAt           *time.Time      `json:"expired_at,omitempty"`
}

// Waiting reports whether the gate still wants an answer.
func (r Record) Waiting() bool { return r.Status == StatusWaiting }

// HasChoice reports whether choice is one the gate offers.
func (r Record) HasChoice(choice string) bool {
	for _, c := range r.Choices {
		if c == choice {
			return true
		}
	}
	return false
}

// DefaultDir is the gates directory under the runtime's work dir.
func DefaultDir(workDir string) string { return filepath.Join(workDir, DirName) }

// ParseTimeout reads the wire form of a timeout: "" and "0" wait
// indefinitely, anything else is a positive Go duration.
func ParseTimeout(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: timeout: %v", ErrBadRequest, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%w: timeout %q is negative", ErrBadRequest, s)
	}
	return d, nil
}

// FormatTimeout is ParseTimeout's inverse: "0" for indefinitely.
func FormatTimeout(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	return d.String()
}

// DeriveIdempotencyKey identifies a gate the way toolapprove's inbox
// identifies an approval request: a caller-supplied key wins, otherwise the
// key covers what is being asked — subject, canonical context, choices,
// timeout, the driver label and the run it is opened for — and never
// timestamps, so a driver that re-opens the same gate after a restart gets
// the same record. Either form is scoped to the opening principal so two
// callers cannot read each other's gates by guessing a key. req must
// already be normalized.
func DeriveIdempotencyKey(principal string, req Request) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	write("gate.v1", principal)
	if strings.TrimSpace(req.IdempotencyKey) != "" {
		write("key", req.IdempotencyKey)
		return hex.EncodeToString(h.Sum(nil))
	}
	write("request", req.Subject, string(req.Context), FormatTimeout(req.Timeout), req.OpenedBy)
	write(fmt.Sprintf("%d", len(req.Choices)))
	write(req.Choices...)
	// A gate without a run keys exactly as before runs existed, so gates
	// opened by an older server stay idempotent.
	if req.Run != "" {
		write("run", req.Run)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ValidID reports whether id has the shape of a gate id, which keeps a
// caller-supplied id from escaping the gates directory.
func ValidID(id string) bool {
	rest, ok := strings.CutPrefix(id, IDPrefix)
	if !ok || rest == "" || len(id) > maxIDLen {
		return false
	}
	for _, r := range rest {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

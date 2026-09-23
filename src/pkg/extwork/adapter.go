package extwork

import (
	"context"
	"errors"
	"io"
	"time"
)

// State is the engine-neutral observation state of a native run.
type State string

const (
	// StateAccepted means the engine admitted the keyed request but has not
	// started executing it.
	StateAccepted State = "accepted"
	// StateRunning means a stage is executing.
	StateRunning State = "running"
	// StateWaiting means the run is parked on something outside the engine
	// (a human, a budget cap, an external dependency). It is visible, not
	// failed.
	StateWaiting State = "waiting"
	// StateTerminal means the engine will not change the run again; the
	// receipt, if any, is final.
	StateTerminal State = "terminal"
	// StateUnknown means Hive could not establish the native state. A local
	// timeout or transport error yields this, never a fabricated failure.
	StateUnknown State = "unknown"
)

// ValidState reports whether s is one of the five observation states.
func ValidState(s State) bool {
	switch s {
	case StateAccepted, StateRunning, StateWaiting, StateTerminal, StateUnknown:
		return true
	default:
		return false
	}
}

// ExecutionKey is the logical execution identity Hive derives from an
// Admission. It is the idempotency key handed to the engine.
type ExecutionKey string

// StartRequest is what an adapter dispatches. Payload is the bounded context
// bundle; its digest must already be recorded in Admission.RequestDigest.
type StartRequest struct {
	Admission Admission
	Payload   []byte
}

// StartResult is the engine's native admission receipt for a keyed start.
type StartResult struct {
	RemoteRunID       string
	RemoteIncarnation string
	// Deduplicated is true when the engine matched an earlier submission under
	// the same key and payload instead of creating a second run.
	Deduplicated bool
}

// ReceiptRef locates a stage receipt inside the engine's artifact store for a
// run. Path is relative to that run; Digest is the sha256 hex of the exact
// bytes; Size is their length.
type ReceiptRef struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Observation is one native status reading.
type Observation struct {
	State             State
	RemoteRunID       string
	RemoteIncarnation string
	Stage             string
	ResultClass       string
	Receipt           *ReceiptRef
	Detail            string
	ObservedAt        time.Time
}

// CancelFacts keeps cancellation delivery, acknowledgement, and workload
// termination apart. A workload that ignores the request leaves Stopped
// false; Hive must then keep the run visibly uncertain.
type CancelFacts struct {
	Requested    bool
	Acknowledged bool
	Stopped      bool
	Detail       string
}

// Sentinel errors an Adapter returns so the binding can classify outcomes
// without parsing engine-specific text.
var (
	// ErrConflict: the execution key is already bound to a different payload.
	ErrConflict = errors.New("extwork: execution key already bound to a different payload")
	// ErrRefused: the engine refused admission (capability, version, policy).
	ErrRefused = errors.New("extwork: engine refused the request")
	// ErrNotFound: the engine holds no native run for the execution key.
	ErrNotFound = errors.New("extwork: no native run for this execution key")
	// ErrIncarnationMismatch: the native run exists but belongs to a different
	// engine incarnation than the one Hive bound; it must not be adopted.
	ErrIncarnationMismatch = errors.New("extwork: native run belongs to a different engine incarnation")
	// ErrTransport: the engine could not be reached; native state is unknown.
	ErrTransport = errors.New("extwork: transport failure; native state unknown")
)

// Adapter is the contract every external engine binding implements. Every
// method is scoped by ExecutionKey and, where the engine exposes one, the
// incarnation Hive pinned at start. There is no method that takes a URL.
type Adapter interface {
	// Engine names the engine, e.g. "flue".
	Engine() string
	// Start admits the keyed request. It must be idempotent for an identical
	// key and payload and return ErrConflict for an identical key with a
	// different payload.
	Start(ctx context.Context, req StartRequest) (StartResult, error)
	// Observe reads the native state for the key. incarnation is the value
	// pinned at start, or empty when Hive has not bound one yet.
	Observe(ctx context.Context, key ExecutionKey, incarnation string) (Observation, error)
	// Cancel requests cancellation and reports what the engine confirmed.
	Cancel(ctx context.Context, key ExecutionKey, incarnation string) (CancelFacts, error)
	// OpenArtifact streams one artifact of the run by relative path. The
	// binding, not the adapter, enforces size, path, and digest rules.
	OpenArtifact(ctx context.Context, key ExecutionKey, incarnation, path string) (io.ReadCloser, error)
}

// Pinner is implemented by adapters whose engine exposes an instance identity
// (Flue's runtime uid). The binding records it in the admission before
// dispatch so delete-and-recreate of the engine can never be mistaken for the
// same run.
type Pinner interface {
	Incarnation(ctx context.Context) (string, error)
}

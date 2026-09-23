package extwork

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/effects"
)

// Operating modes of the binding. The zero value of a config resolves to
// ModeOff; ModeShadow observes only; ModeReportOnly starts work but carries no
// publication authority.
const (
	ModeOff        = "off"
	ModeShadow     = "shadow"
	ModeReportOnly = "report-only"
)

// ValidMode reports whether mode is one of the three operating modes.
func ValidMode(mode string) bool {
	switch mode {
	case ModeOff, ModeShadow, ModeReportOnly:
		return true
	default:
		return false
	}
}

// AuthorityBinding is the Hive-side authority under which an external run is
// admitted. It names WHO holds the lease, at what tier, under which negotiated
// capability, and in which operating mode. It never carries a credential.
type AuthorityBinding struct {
	Identity   string `json:"identity"`
	Tier       string `json:"tier"`
	Capability string `json:"capability"`
	Mode       string `json:"mode"`
}

// Admission is the durable intent Hive records before it dispatches anything
// externally (#8201 section A). Every field except RequestDigest and Authority
// contributes to the ExecutionKey; RequestDigest binds the exact payload so a
// changed bundle under the same key is a conflict rather than an adoption.
type Admission struct {
	WorkKey          string           `json:"work_key"`
	AssignmentID     string           `json:"assignment_id"`
	Generation       uint64           `json:"generation"`
	Stage            string           `json:"stage"`
	ContractRevision string           `json:"contract_revision"`
	Engine           string           `json:"engine"`
	WorkflowVersion  string           `json:"workflow_version"`
	InputRevision    string           `json:"input_revision"`
	RequestDigest    string           `json:"request_digest"`
	Authority        AuthorityBinding `json:"authority"`
	// EngineIncarnation pins the engine instance (Flue's runtime uid) that was
	// live when the admission was recorded. It is not part of the execution
	// key: a recreated engine under the same key is a mismatch to refuse, not
	// a new logical execution to adopt.
	EngineIncarnation string `json:"engine_incarnation,omitempty"`
	// RemoteRunID is the native run identity, recorded once the engine
	// accepted the keyed start. Empty means the durable record knows of no
	// start; recovery uses that to tell "never dispatched" from "dispatched,
	// record not yet updated".
	RemoteRunID string `json:"remote_run_id,omitempty"`
}

// ErrInvalidAdmission wraps every Validate failure.
var ErrInvalidAdmission = errors.New("extwork: invalid admission")

// Validate checks that every identity the execution key depends on is present
// and that the authority binding names a mode the binding can honour.
func (a Admission) Validate() error {
	required := []struct{ name, value string }{
		{"work_key", a.WorkKey},
		{"assignment_id", a.AssignmentID},
		{"stage", a.Stage},
		{"contract_revision", a.ContractRevision},
		{"engine", a.Engine},
		{"workflow_version", a.WorkflowVersion},
		{"input_revision", a.InputRevision},
		{"request_digest", a.RequestDigest},
		{"authority.identity", a.Authority.Identity},
		{"authority.capability", a.Authority.Capability},
	}
	for _, r := range required {
		if strings.TrimSpace(r.value) == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidAdmission, r.name)
		}
	}
	if a.Generation == 0 {
		return fmt.Errorf("%w: generation must be greater than zero", ErrInvalidAdmission)
	}
	if !ValidMode(a.Authority.Mode) {
		return fmt.Errorf("%w: authority.mode %q is not one of off, shadow, report-only", ErrInvalidAdmission, a.Authority.Mode)
	}
	return nil
}

// ExecutionKey derives the logical execution identity. It deliberately
// excludes RequestDigest: two dispatches that agree on every identity but
// carry different payloads share a key and therefore CONFLICT at the engine,
// which is the property #8201 row 5 requires.
func (a Admission) ExecutionKey() ExecutionKey {
	return ExecutionKey(effects.StableDigest(
		a.WorkKey,
		a.AssignmentID,
		strconv.FormatUint(a.Generation, 10),
		a.Stage,
		a.ContractRevision,
		a.Engine,
		a.WorkflowVersion,
		a.InputRevision,
	))
}

// RequestDigest is the sha256 hex of a payload, the value Admission.RequestDigest
// must carry for that payload.
func RequestDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// AdmissionStore is the durable-before-dispatch seam. Callers back it with an
// existing record (the Hive task lease); the package ships MemoryStore only for
// tests. Persist must return an error whenever the record is not durable, and
// Load must report ok=false for an assignment that was never persisted so a
// recovery path cannot mint authority from nothing.
type AdmissionStore interface {
	Persist(adm Admission) error
	Load(assignmentID string) (Admission, bool, error)
}

// ReceiptStore keeps the verified receipt bytes of a settled run so a replay
// can rehydrate the typed result without a second external run.
type ReceiptStore interface {
	SaveReceipt(assignmentID string, raw []byte) error
	LoadReceipt(assignmentID string) ([]byte, bool, error)
}

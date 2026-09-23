package extwork

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// DefaultMaxReceiptBytes bounds a receipt fetch. A stage receipt is a small
// JSON document; anything near this size is not one.
const DefaultMaxReceiptBytes int64 = 1 << 20

// FetchLimits are the restrictions applied before receipt bytes are parsed.
type FetchLimits struct {
	MaxBytes int64
}

func (l FetchLimits) maxBytes() int64 {
	if l.MaxBytes <= 0 {
		return DefaultMaxReceiptBytes
	}
	return l.MaxBytes
}

// Receipt refusal reasons. Every one wraps ErrReceiptRefused so callers can
// classify with errors.Is and still read the specific cause.
var (
	ErrReceiptRefused   = errors.New("extwork: receipt refused")
	ErrReceiptPath      = fmt.Errorf("%w: artifact path is not a clean relative path", ErrReceiptRefused)
	ErrReceiptTooLarge  = fmt.Errorf("%w: artifact exceeds the size limit", ErrReceiptRefused)
	ErrReceiptTruncated = fmt.Errorf("%w: artifact bytes do not match the declared size", ErrReceiptRefused)
	ErrReceiptDigest    = fmt.Errorf("%w: artifact bytes do not match the declared digest", ErrReceiptRefused)
	ErrReceiptMissing   = fmt.Errorf("%w: artifact is missing", ErrReceiptRefused)
	ErrReceiptSchema    = fmt.Errorf("%w: receipt fails schema validation", ErrReceiptRefused)
	ErrReceiptUnbound   = fmt.Errorf("%w: receipt does not bind this admission", ErrReceiptRefused)
)

// CleanArtifactPath accepts only a relative, forward-slash path with no empty,
// ".", or ".." segments, no absolute prefix, no backslash, no NUL, and a
// bounded length. It is applied before an adapter is asked to open anything.
func CleanArtifactPath(path string) (string, error) {
	if path == "" || len(path) > outputschema.MaxArtifactPathLength {
		return "", ErrReceiptPath
	}
	if strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\\x00") || strings.Contains(path, "://") {
		return "", ErrReceiptPath
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", ErrReceiptPath
		}
	}
	return path, nil
}

// readVerified reads at most limits.maxBytes()+1 bytes from r and checks the
// declared size and digest. It returns the exact bytes on success.
func readVerified(r io.Reader, ref ReceiptRef, limits FetchLimits) ([]byte, error) {
	limit := limits.maxBytes()
	if ref.Size <= 0 || ref.Size > limit {
		return nil, ErrReceiptTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	if int64(len(raw)) > limit {
		return nil, ErrReceiptTooLarge
	}
	if int64(len(raw)) != ref.Size {
		return nil, ErrReceiptTruncated
	}
	sum := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), ref.Digest) {
		return nil, ErrReceiptDigest
	}
	return raw, nil
}

// ParseReceipt validates raw as an AgentReport of kind stage_receipt and
// returns its receipt. It applies every outputschema rule, including the
// output digest check across the listed artifacts.
func ParseReceipt(raw []byte) (*outputschema.StageReceipt, error) {
	report, err := outputschema.Validate(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrReceiptSchema, err)
	}
	if report.Kind != outputschema.KindStageReceipt || report.Receipt == nil {
		return nil, fmt.Errorf("%w: report kind is %q, want stage_receipt", ErrReceiptSchema, report.Kind)
	}
	return report.Receipt, nil
}

// BindReceipt checks that the receipt names exactly this admission: same work
// key, assignment, generation, stage, contract revision, execution key, engine
// and workflow version, and input revision. An old receipt therefore cannot
// authorise a new subject (#8201 row 11).
func BindReceipt(adm Admission, receipt *outputschema.StageReceipt) error {
	if receipt == nil {
		return fmt.Errorf("%w: nil receipt", ErrReceiptUnbound)
	}
	mismatch := func(field, got, want string) error {
		return fmt.Errorf("%w: %s is %q, admission has %q", ErrReceiptUnbound, field, got, want)
	}
	if receipt.WorkKey != adm.WorkKey {
		return mismatch("work_key", receipt.WorkKey, adm.WorkKey)
	}
	if receipt.AssignmentID != adm.AssignmentID {
		return mismatch("assignment_id", receipt.AssignmentID, adm.AssignmentID)
	}
	if receipt.Generation != adm.Generation {
		return fmt.Errorf("%w: generation is %d, admission has %d", ErrReceiptUnbound, receipt.Generation, adm.Generation)
	}
	if receipt.Stage != adm.Stage {
		return mismatch("stage", receipt.Stage, adm.Stage)
	}
	if receipt.ContractRevision != adm.ContractRevision {
		return mismatch("contract_revision", receipt.ContractRevision, adm.ContractRevision)
	}
	if receipt.ExecutionKey != string(adm.ExecutionKey()) {
		return mismatch("execution_key", receipt.ExecutionKey, string(adm.ExecutionKey()))
	}
	if receipt.Engine == nil || receipt.Engine.Name != adm.Engine {
		return fmt.Errorf("%w: engine does not match admission engine %q", ErrReceiptUnbound, adm.Engine)
	}
	if receipt.Engine.Version != adm.WorkflowVersion {
		return mismatch("engine.version", receipt.Engine.Version, adm.WorkflowVersion)
	}
	if receipt.InputRevision != adm.InputRevision {
		return mismatch("input_revision", receipt.InputRevision, adm.InputRevision)
	}
	return nil
}

// FetchReceipt opens the referenced artifact through the adapter, applies the
// path, size, and digest restrictions, parses and validates the receipt, and
// binds it to the admission. Nothing is parsed before the bytes are verified.
func FetchReceipt(ctx context.Context, adapter Adapter, adm Admission, incarnation string, ref ReceiptRef, limits FetchLimits) (*outputschema.StageReceipt, []byte, error) {
	path, err := CleanArtifactPath(ref.Path)
	if err != nil {
		return nil, nil, err
	}
	if ref.Size <= 0 || ref.Size > limits.maxBytes() {
		return nil, nil, ErrReceiptTooLarge
	}
	rc, err := adapter.OpenArtifact(ctx, adm.ExecutionKey(), incarnation, path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, ErrReceiptMissing
		}
		return nil, nil, err
	}
	defer func() { _ = rc.Close() }()
	raw, err := readVerified(rc, ref, limits)
	if err != nil {
		return nil, nil, err
	}
	receipt, err := ParseReceipt(raw)
	if err != nil {
		return nil, nil, err
	}
	if err := BindReceipt(adm, receipt); err != nil {
		return nil, nil, err
	}
	return receipt, raw, nil
}

package extwork

import (
	"errors"
	"fmt"
)

// LeaseAuthority is what a durable admission store needs from the owner of
// the task lease. Its signature carries no extwork type, so the lease owner
// (pkg/dashboard) implements it without importing this package; cmd/hive
// wires the two together.
//
// Verify reports nil only when a live lease held by identity for taskID has
// exactly this work key, tier, stage, and generation. Flush verifies the same
// and then forces the lease registry to durable storage, returning an error
// when it did not reach disk.
type LeaseAuthority interface {
	Verify(identity, taskID, workKey, tier, stage string, gen uint64) error
	Flush(identity, taskID, workKey, tier, stage string, gen uint64) error
}

// ErrLeaseAuthority wraps every refusal that comes from the lease owner.
var ErrLeaseAuthority = errors.New("extwork: admission does not match a live lease")

// LeaseStore is the AdmissionStore and ReceiptStore backed by the task lease.
// The lease is the authority record: Persist refuses an admission the lease
// owner does not vouch for and forces the lease to disk before the record
// that carries the fields a lease cannot hold (request digest, contract and
// input revisions, pinned engine incarnation) is written beside the receipt.
// Load re-checks the lease, so a record whose lease expired or was revoked
// grants nothing on recovery: no lease, no authority.
type LeaseStore struct {
	authority LeaseAuthority
	files     *FileStore
}

// NewLeaseStore composes the lease owner with a file store rooted at dir.
func NewLeaseStore(authority LeaseAuthority, dir string) *LeaseStore {
	return &LeaseStore{authority: authority, files: NewFileStore(dir)}
}

func (s *LeaseStore) flush(adm Admission) error {
	if err := s.authority.Flush(adm.Authority.Identity, adm.AssignmentID, adm.WorkKey, adm.Authority.Tier, adm.Stage, adm.Generation); err != nil {
		return fmt.Errorf("%w: %v", ErrLeaseAuthority, err)
	}
	return nil
}

func (s *LeaseStore) verify(adm Admission) error {
	if err := s.authority.Verify(adm.Authority.Identity, adm.AssignmentID, adm.WorkKey, adm.Authority.Tier, adm.Stage, adm.Generation); err != nil {
		return fmt.Errorf("%w: %v", ErrLeaseAuthority, err)
	}
	return nil
}

// Persist implements AdmissionStore.
func (s *LeaseStore) Persist(adm Admission) error {
	if err := adm.Validate(); err != nil {
		return err
	}
	if err := s.flush(adm); err != nil {
		return err
	}
	return s.files.Persist(adm)
}

// Load implements AdmissionStore. A record without a matching live lease is
// reported as absent.
func (s *LeaseStore) Load(assignmentID string) (Admission, bool, error) {
	adm, ok, err := s.files.Load(assignmentID)
	if err != nil || !ok {
		return Admission{}, false, err
	}
	if err := s.verify(adm); err != nil {
		return Admission{}, false, nil
	}
	return adm, true, nil
}

// SaveReceipt implements ReceiptStore.
func (s *LeaseStore) SaveReceipt(assignmentID string, raw []byte) error {
	return s.files.SaveReceipt(assignmentID, raw)
}

// LoadReceipt implements ReceiptStore.
func (s *LeaseStore) LoadReceipt(assignmentID string) ([]byte, bool, error) {
	return s.files.LoadReceipt(assignmentID)
}

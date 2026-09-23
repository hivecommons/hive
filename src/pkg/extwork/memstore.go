package extwork

import (
	"errors"
	"sync"
)

// MemoryStore is an in-process AdmissionStore and ReceiptStore for tests and
// for the second-host seam. It is not durable and must never back a production
// binding; the dashboard backs the interfaces with the task lease and the
// agent report directory instead.
type MemoryStore struct {
	mu         sync.Mutex
	admissions map[string]Admission
	receipts   map[string][]byte
	// FailPersist, when set, makes every Persist fail with it so a test can
	// prove that an unpersisted admission never becomes active.
	FailPersist error
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{admissions: map[string]Admission{}, receipts: map[string][]byte{}}
}

// ErrStoreUnavailable is the error MemoryStore.FailPersist is usually set to.
var ErrStoreUnavailable = errors.New("extwork: admission store unavailable")

// Persist records the admission, refusing when FailPersist is set.
func (m *MemoryStore) Persist(adm Admission) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailPersist != nil {
		return m.FailPersist
	}
	m.admissions[adm.AssignmentID] = adm
	return nil
}

// Load returns the admission recorded for assignmentID.
func (m *MemoryStore) Load(assignmentID string) (Admission, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	adm, ok := m.admissions[assignmentID]
	return adm, ok, nil
}

// SaveReceipt records verified receipt bytes.
func (m *MemoryStore) SaveReceipt(assignmentID string, raw []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.receipts[assignmentID] = append([]byte(nil), raw...)
	return nil
}

// LoadReceipt returns the recorded receipt bytes.
func (m *MemoryStore) LoadReceipt(assignmentID string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.receipts[assignmentID]
	return raw, ok, nil
}

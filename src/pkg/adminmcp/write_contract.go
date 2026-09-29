package adminmcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ToolWritePreview = "write_preview"
	ToolWriteConfirm = "write_confirm"

	DefaultConfirmationTTL = 10 * time.Minute

	// MaxPendingConfirmations bounds how many unconfirmed previews a store
	// keeps. Previews are free to issue and most are never confirmed, so
	// without a cap an exploring model grows the store (and the file rewritten
	// on every call) until entries expire. The oldest entry is evicted first.
	MaxPendingConfirmations = 64
)

var (
	ErrWritesDisabled      = errors.New("admin MCP writes are disabled")
	ErrConfirmationExpired = errors.New("admin MCP confirmation expired")
	ErrConfirmationMissing = errors.New("admin MCP confirmation not found")
)

type WriteClient interface {
	ExecuteWrite(ctx context.Context, req WriteRequest) (any, error)
}

type WriteRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   any    `json:"body,omitempty"`
}

type WritePreview struct {
	Operation           string         `json:"operation"`
	Summary             string         `json:"summary"`
	Target              string         `json:"target"`
	Request             WriteRequest   `json:"request"`
	Effects             []string       `json:"effects"`
	WideningDisclosure  string         `json:"widening_disclosure"`
	ConfirmationMessage string         `json:"confirmation_message"`
	Details             map[string]any `json:"details,omitempty"`
}

type WriteOp interface {
	Name() string
	Description() string
	InputSchema() map[string]any
	Preview(ctx context.Context, args map[string]any) (WritePreview, error)
}

type WriteRegistry struct {
	mu  sync.RWMutex
	ops map[string]WriteOp
}

func NewWriteRegistry(ops ...WriteOp) *WriteRegistry {
	r := &WriteRegistry{ops: map[string]WriteOp{}}
	for _, op := range ops {
		if op != nil {
			r.Register(op)
		}
	}
	return r
}

func DefaultWriteRegistry() *WriteRegistry { return NewWriteRegistry(DefaultWriteOps()...) }

func (r *WriteRegistry) Register(op WriteOp) {
	if op == nil || strings.TrimSpace(op.Name()) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops[op.Name()] = op
}

func (r *WriteRegistry) Get(name string) (WriteOp, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	op, ok := r.ops[strings.TrimSpace(name)]
	return op, ok
}

func (r *WriteRegistry) Ops() []WriteOp {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.ops))
	for name := range r.ops {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]WriteOp, 0, len(names))
	for _, name := range names {
		out = append(out, r.ops[name])
	}
	return out
}

type PendingConfirmation struct {
	ID        string         `json:"id"`
	Operation string         `json:"operation"`
	Args      map[string]any `json:"args"`
	Preview   WritePreview   `json:"preview"`
	Hive      string         `json:"hive"`
	CreatedAt time.Time      `json:"created_at"`
	ExpiresAt time.Time      `json:"expires_at"`
}

type PendingStore interface {
	Put(ctx context.Context, pending PendingConfirmation) error
	Take(ctx context.Context, id string) (PendingConfirmation, error)
	List(ctx context.Context) ([]PendingConfirmation, error)
}

type MemoryPendingStore struct {
	mu      sync.Mutex
	pending map[string]PendingConfirmation
}

func NewMemoryPendingStore() *MemoryPendingStore {
	return &MemoryPendingStore{pending: map[string]PendingConfirmation{}}
}

func (s *MemoryPendingStore) Put(_ context.Context, pending PendingConfirmation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now().UTC())
	s.pending[pending.ID] = pending
	capPendingConfirmations(s.pending, MaxPendingConfirmations)
	return nil
}

func (s *MemoryPendingStore) Take(_ context.Context, id string) (PendingConfirmation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now().UTC())
	pending, ok := s.pending[id]
	if !ok {
		return PendingConfirmation{}, ErrConfirmationMissing
	}
	delete(s.pending, id)
	return pending, nil
}

func (s *MemoryPendingStore) List(_ context.Context) ([]PendingConfirmation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now().UTC())
	out := make([]PendingConfirmation, 0, len(s.pending))
	for _, pending := range s.pending {
		out = append(out, pending)
	}
	return out, nil
}

func (s *MemoryPendingStore) pruneLocked(now time.Time) {
	for id, pending := range s.pending {
		if !pending.ExpiresAt.IsZero() && !now.Before(pending.ExpiresAt) {
			delete(s.pending, id)
		}
	}
}

type FilePendingStore struct {
	path string
}

var filePendingStoreLocks sync.Map

func NewFilePendingStore(path string) *FilePendingStore { return &FilePendingStore{path: path} }

func (s *FilePendingStore) Put(ctx context.Context, pending PendingConfirmation) error {
	unlock := s.lockPath()
	defer unlock()
	items, _, err := s.loadLocked(ctx)
	if err != nil {
		return err
	}
	items[pending.ID] = pending
	capPendingConfirmations(items, MaxPendingConfirmations)
	return s.saveLocked(items)
}

func (s *FilePendingStore) Take(ctx context.Context, id string) (PendingConfirmation, error) {
	unlock := s.lockPath()
	defer unlock()
	items, pruned, err := s.loadLocked(ctx)
	if err != nil {
		return PendingConfirmation{}, err
	}
	pending, ok := items[id]
	if !ok {
		if pruned {
			_ = s.saveLocked(items)
		}
		return PendingConfirmation{}, ErrConfirmationMissing
	}
	delete(items, id)
	if err := s.saveLocked(items); err != nil {
		return PendingConfirmation{}, err
	}
	return pending, nil
}

func (s *FilePendingStore) List(ctx context.Context) ([]PendingConfirmation, error) {
	unlock := s.lockPath()
	defer unlock()
	items, pruned, err := s.loadLocked(ctx)
	if err != nil {
		return nil, err
	}
	if pruned {
		if err := s.saveLocked(items); err != nil {
			return nil, err
		}
	}
	out := make([]PendingConfirmation, 0, len(items))
	for _, pending := range items {
		out = append(out, pending)
	}
	return out, nil
}

func (s *FilePendingStore) lockPath() func() {
	key := strings.TrimSpace(s.path)
	if key == "" {
		key = "<empty>"
	}
	muAny, _ := filePendingStoreLocks.LoadOrStore(key, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *FilePendingStore) loadLocked(ctx context.Context) (map[string]PendingConfirmation, bool, error) {
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	default:
	}
	items := map[string]PendingConfirmation{}
	if strings.TrimSpace(s.path) == "" {
		return items, false, fmt.Errorf("pending confirmation store path is empty")
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return items, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return items, false, nil
	}
	if err := json.Unmarshal(data, &items); err != nil {
		// A torn or hand-edited file must not disable every admin write
		// until an operator deletes it. Pending confirmations are cheap to
		// re-issue, so set the bad file aside for inspection and start empty.
		if qerr := s.quarantineLocked(err); qerr != nil {
			return nil, false, fmt.Errorf("pending confirmation store %s is unreadable (%v) and could not be set aside: %w", s.path, err, qerr)
		}
		return map[string]PendingConfirmation{}, false, nil
	}
	return items, prunePendingConfirmations(items, time.Now().UTC()), nil
}

func (s *FilePendingStore) quarantineLocked(cause error) error {
	dest := fmt.Sprintf("%s.corrupt-%s", s.path, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(s.path, dest); err != nil {
		return err
	}
	slog.Warn("admin MCP pending confirmation store was unreadable; moved aside and started empty", "path", s.path, "moved_to", dest, "err", cause)
	return nil
}

func (s *FilePendingStore) saveLocked(items map[string]PendingConfirmation) error {
	prunePendingConfirmations(items, time.Now().UTC())
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, append(data, '\n'), 0o600)
}

// writeFileAtomic replaces path via a synced temp file in the same directory
// and a rename, so a crash mid-write leaves the previous complete file instead
// of a truncated one.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func prunePendingConfirmations(items map[string]PendingConfirmation, now time.Time) bool {
	pruned := false
	for id, pending := range items {
		if !pending.ExpiresAt.IsZero() && !now.Before(pending.ExpiresAt) {
			delete(items, id)
			pruned = true
		}
	}
	return pruned
}

// capPendingConfirmations evicts the oldest entries (by CreatedAt, then ID
// for a stable order) until at most limit remain.
func capPendingConfirmations(items map[string]PendingConfirmation, limit int) {
	if limit <= 0 || len(items) <= limit {
		return
	}
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := items[ids[i]], items[ids[j]]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids[:len(ids)-limit] {
		delete(items, id)
	}
}

func newConfirmationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type HiveRefusal struct {
	Type       string `json:"type"`
	StatusCode int    `json:"status_code"`
	Message    string `json:"message,omitempty"`
	Body       any    `json:"body,omitempty"`
	RawBody    string `json:"raw_body,omitempty"`
}

type HiveRefusalError struct {
	StatusCode int
	Message    string
	Body       []byte
}

func (e *HiveRefusalError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("hive refused write with HTTP %d", e.StatusCode)
}

func HiveRefusalFromError(err error) (HiveRefusal, bool) {
	var refusal *HiveRefusalError
	if !errors.As(err, &refusal) {
		return HiveRefusal{}, false
	}
	out := HiveRefusal{Type: "hive-refusal", StatusCode: refusal.StatusCode, Message: refusal.Message}
	if len(refusal.Body) > 0 {
		var body any
		if json.Unmarshal(refusal.Body, &body) == nil {
			out.Body = body
		} else {
			out.RawBody = string(refusal.Body)
		}
	}
	return out, true
}

package adminmcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	s.pending[pending.ID] = pending
	return nil
}

func (s *MemoryPendingStore) Take(_ context.Context, id string) (PendingConfirmation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	out := make([]PendingConfirmation, 0, len(s.pending))
	for _, pending := range s.pending {
		out = append(out, pending)
	}
	return out, nil
}

type FilePendingStore struct {
	path string
}

var filePendingStoreLocks sync.Map

func NewFilePendingStore(path string) *FilePendingStore { return &FilePendingStore{path: path} }

func (s *FilePendingStore) Put(ctx context.Context, pending PendingConfirmation) error {
	unlock := s.lockPath()
	defer unlock()
	items, err := s.loadLocked(ctx)
	if err != nil {
		return err
	}
	items[pending.ID] = pending
	return s.saveLocked(items)
}

func (s *FilePendingStore) Take(ctx context.Context, id string) (PendingConfirmation, error) {
	unlock := s.lockPath()
	defer unlock()
	items, err := s.loadLocked(ctx)
	if err != nil {
		return PendingConfirmation{}, err
	}
	pending, ok := items[id]
	if !ok {
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
	items, err := s.loadLocked(ctx)
	if err != nil {
		return nil, err
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

func (s *FilePendingStore) loadLocked(ctx context.Context) (map[string]PendingConfirmation, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	items := map[string]PendingConfirmation{}
	if strings.TrimSpace(s.path) == "" {
		return items, fmt.Errorf("pending confirmation store path is empty")
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return items, nil
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *FilePendingStore) saveLocked(items map[string]PendingConfirmation) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, append(data, '\n'), 0o600)
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

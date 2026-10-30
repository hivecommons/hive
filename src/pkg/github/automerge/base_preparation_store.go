package automerge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// BasePreparationStore serializes final preparation independently of merge
// train membership. A nonempty directory must be on the hive's durable volume.
// Each destination has its own flock and atomic snapshot, including the
// write-ahead update identity, so overlapping sweeps and restarting pods agree.
type BasePreparationStore struct {
	dir   string
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	items map[string]basePreparation
}

type preparationCandidate struct {
	Number int       `json:"number"`
	Head   string    `json:"head"`
	Lane   string    `json:"lane"`
	Repair bool      `json:"repair,omitempty"`
	Since  time.Time `json:"since"`
}

type preparationAttempt struct {
	Head string    `json:"head"`
	Base string    `json:"base"`
	At   time.Time `json:"at"`
}

type basePreparation struct {
	Owner    preparationCandidate       `json:"owner"`
	Waiting  []preparationCandidate     `json:"waiting,omitempty"`
	Attempts map[int]preparationAttempt `json:"attempts,omitempty"`
}

// NewBasePreparationStore accepts an empty directory for isolated, in-memory
// tests. Production uses a directory beside the durable stage store.
func NewBasePreparationStore(dir string) *BasePreparationStore {
	return &BasePreparationStore{dir: dir, items: map[string]basePreparation{}, locks: map[string]*sync.Mutex{}}
}

func preparationKey(owner, repo, base string) string {
	// GitHub repository names are case insensitive; refs are not.
	return strings.ToLower(strings.TrimSpace(owner)+"/"+strings.TrimSpace(repo)) + ":" + strings.TrimSpace(base)
}

func (s *BasePreparationStore) change(key string, fn func(*basePreparation) error) error {
	s.mu.Lock()
	lockMu := s.locks[key]
	if lockMu == nil {
		lockMu = &sync.Mutex{}
		s.locks[key] = lockMu
	}
	s.mu.Unlock()
	lockMu.Lock()
	defer lockMu.Unlock()
	if s.dir == "" {
		s.mu.Lock()
		st := s.items[key]
		s.mu.Unlock()
		if err := fn(&st); err != nil {
			return err
		}
		s.mu.Lock()
		s.items[key] = st
		s.mu.Unlock()
		return nil
	}
	const directoryMode = 0o775
	const fileMode = 0o664
	if err := os.MkdirAll(s.dir, directoryMode); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(key))
	path := filepath.Join(s.dir, hex.EncodeToString(sum[:]))
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) // process death also releases the lock
	var st basePreparation
	data, err := os.ReadFile(path + ".json")
	if err == nil {
		if err := json.Unmarshal(data, &st); err != nil {
			return err // corrupt or unreadable ownership must fail closed
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	data, err = json.Marshal(st)
	if err != nil {
		return err
	}
	// The separate lock inode remains stable while the snapshot is replaced.
	f, err := os.OpenFile(path+".next", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(path+".next", path+".json"); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

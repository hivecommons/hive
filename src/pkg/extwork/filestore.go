package extwork

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// FileStore keeps, per assignment, the admission record and the verified
// receipt bytes as two JSON files under one directory. It exists for the
// parts of an admission the Hive task lease cannot hold (request digest,
// contract and input revisions, pinned engine incarnation) and for receipt
// replay; the lease remains the authority record, and the dashboard checks
// the two agree before it trusts either. Writes are temp-file plus rename so
// a crash never leaves a partial record.
type FileStore struct {
	dir string
}

const (
	admissionFileSuffix = ".admission.json"
	receiptFileSuffix   = ".receipt.json"
	fileStoreDirPerm    = 0o755
	fileStoreFilePerm   = 0o600
)

// NewFileStore returns a store rooted at dir, creating it on first write.
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir} }

// ErrBadAssignmentID rejects an assignment id that cannot be a safe file name.
var ErrBadAssignmentID = errors.New("extwork: assignment id is not a safe file name")

func safeName(assignmentID string) (string, error) {
	if assignmentID == "" || len(assignmentID) > 200 {
		return "", ErrBadAssignmentID
	}
	for _, r := range assignmentID {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' || r == ':') {
			return "", ErrBadAssignmentID
		}
	}
	if strings.HasPrefix(assignmentID, ".") {
		return "", ErrBadAssignmentID
	}
	return assignmentID, nil
}

func (s *FileStore) write(name string, raw []byte) error {
	if err := os.MkdirAll(s.dir, fileStoreDirPerm); err != nil {
		return err
	}
	path := filepath.Join(s.dir, name)
	tmp, err := os.CreateTemp(s.dir, name+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(fileStoreFilePerm); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	keep = true
	return nil
}

func (s *FileStore) read(name string) ([]byte, bool, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// Persist writes the admission record.
func (s *FileStore) Persist(adm Admission) error {
	if err := adm.Validate(); err != nil {
		return err
	}
	name, err := safeName(adm.AssignmentID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(adm)
	if err != nil {
		return err
	}
	return s.write(name+admissionFileSuffix, raw)
}

// Load reads the admission record.
func (s *FileStore) Load(assignmentID string) (Admission, bool, error) {
	name, err := safeName(assignmentID)
	if err != nil {
		return Admission{}, false, err
	}
	raw, ok, err := s.read(name + admissionFileSuffix)
	if err != nil || !ok {
		return Admission{}, false, err
	}
	var adm Admission
	if err := json.Unmarshal(raw, &adm); err != nil {
		return Admission{}, false, fmt.Errorf("corrupt admission record for %s: %w", assignmentID, err)
	}
	return adm, true, nil
}

// SaveReceipt writes verified receipt bytes.
func (s *FileStore) SaveReceipt(assignmentID string, raw []byte) error {
	name, err := safeName(assignmentID)
	if err != nil {
		return err
	}
	return s.write(name+receiptFileSuffix, raw)
}

// LoadReceipt reads verified receipt bytes.
func (s *FileStore) LoadReceipt(assignmentID string) ([]byte, bool, error) {
	name, err := safeName(assignmentID)
	if err != nil {
		return nil, false, err
	}
	return s.read(name + receiptFileSuffix)
}

// Package store persists workflow models on the hive's data volume
// (hivecommons/hive#17620, phase 2).
//
// Layout under Root:
//
//	workflows/<name>/<version>.json   the model, as saved
//	workflows/index.json              catalogue: one entry per saved version
//
// hive-default is virtual: it is generated, listed read-only, and never
// written. Every save recompiles; a model that does not compile is still
// saved (so an editor can keep a draft) but its entry carries the
// diagnostics and Compiled=false, and Profile refuses it.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// Layout constants.
const (
	// DefaultRoot is the spoke data volume.
	DefaultRoot = "/data"
	// EnvRoot overrides the root (test seam and operator relocation).
	EnvRoot = "HIVE_WORKFLOWS_DATA_DIR"
	// Subdir is the directory under the root.
	Subdir = "workflows"
	// IndexFile is the catalogue inside Subdir.
	IndexFile = "index.json"
)

// Errors.
var (
	ErrNotFound         = errors.New("workflow model not found")
	ErrReadOnly         = errors.New("workflow model is read-only")
	ErrBadName          = errors.New("workflow model name must be lower-case letters, digits and hyphens")
	ErrBadVersion       = errors.New("workflow model version must be letters, digits, dots and hyphens")
	ErrNotCompiled      = errors.New("workflow model does not compile")
	ErrVersionExhausted = errors.New("no higher integer workflow version is available")
)

var (
	nameRe           = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	versionRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,31}$`)
	decimalVersionRe = regexp.MustCompile(`^[0-9]{1,32}$`)
)

// Entry is one catalogue line.
type Entry struct {
	Name        string                     `json:"name"`
	Version     string                     `json:"version"`
	Hash        string                     `json:"hash"`
	Description string                     `json:"description,omitempty"`
	Author      string                     `json:"author,omitempty"`
	Source      workflowmodel.MetaSource   `json:"source,omitempty"`
	Provenance  workflowmodel.Provenance   `json:"provenance,omitempty"`
	Compiled    bool                       `json:"compiled"`
	Diagnostics []workflowmodel.Diagnostic `json:"diagnostics,omitempty"`
	States      int                        `json:"states"`
	Columns     int                        `json:"columns"`
	SavedAt     time.Time                  `json:"savedAt,omitzero"`
	SavedBy     string                     `json:"savedBy,omitempty"`
	ReadOnly    bool                       `json:"readOnly,omitempty"`
	// Reserved for the marketplace and the benchmark / rating work (P7+):
	// never written by the spoke store, carried verbatim across saves.
	Adoption   json.RawMessage `json:"adoption,omitempty"`
	Ratings    json.RawMessage `json:"ratings,omitempty"`
	Benchmarks json.RawMessage `json:"benchmarks,omitempty"`
}

// Key is name@version.
func (e Entry) Key() string { return e.Name + "@" + e.Version }

// Store is the on-disk model library.
type Store struct {
	root string
	mu   sync.Mutex
}

// Root resolves the data root: EnvRoot, else configured, else DefaultRoot.
func Root(configured string) string {
	if v := strings.TrimSpace(os.Getenv(EnvRoot)); v != "" {
		return v
	}
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	return DefaultRoot
}

// Open returns a Store rooted at root (the data dir, not the workflows
// subdirectory). Nothing is created until the first save.
func Open(root string) *Store { return &Store{root: root} }

func (s *Store) dir() string { return filepath.Join(s.root, Subdir) }

func (s *Store) modelPath(name, version string) string {
	return filepath.Join(s.dir(), name, version+".json")
}

func checkKey(name, version string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if !versionRe.MatchString(version) {
		return fmt.Errorf("%w: %q", ErrBadVersion, version)
	}
	return nil
}

// IsBuiltin reports whether name is the generated, read-only model.
func IsBuiltin(name string) bool { return name == hivedefault.Name }

func builtinEntry() Entry {
	m := hivedefault.Generate()
	p := hivedefault.Profile()
	return Entry{
		Name: p.Name, Version: p.Version, Hash: p.Hash,
		Description: m.Spec.Meta.Description, Author: m.Spec.Meta.Author,
		Source: m.Spec.Meta.Source, Provenance: workflowmodel.ProvenanceBuiltIn,
		Compiled: true, States: len(p.States), Columns: len(p.Columns), ReadOnly: true,
	}
}

func entryFor(m *workflowmodel.Model, by string, now time.Time) Entry {
	e := Entry{
		Name: m.Metadata.Name, Version: m.Spec.Meta.Version, Hash: m.Hash(),
		Description: m.Spec.Meta.Description, Author: m.Spec.Meta.Author,
		Source: m.Spec.Meta.Source, Provenance: m.Spec.Meta.Provenance,
		States: len(m.Spec.States), Columns: len(m.Spec.Columns),
		SavedBy: by,
	}
	if !now.IsZero() {
		e.SavedAt = now.UTC().Truncate(time.Second)
	}
	if e.Provenance == "" {
		e.Provenance = workflowmodel.ProvenanceLocal
	}
	p, diags, err := workflowmodel.Compile(m)
	e.Diagnostics = diags
	e.Compiled = err == nil && p != nil
	return e
}

// List returns the catalogue: hive-default first, then saved entries by
// name then version.
func (s *Store) List() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, err := s.readIndex()
	if err != nil {
		return nil, err
	}
	return append([]Entry{builtinEntry()}, idx...), nil
}

// Get returns a saved model (or the generated hive-default) with its entry.
func (s *Store) Get(name, version string) (*workflowmodel.Model, Entry, error) {
	if IsBuiltin(name) {
		if version != "" && version != hivedefault.Version {
			return nil, Entry{}, fmt.Errorf("%w: %s@%s", ErrNotFound, name, version)
		}
		return hivedefault.Generate(), builtinEntry(), nil
	}
	if err := checkKey(name, version); err != nil {
		return nil, Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.modelPath(name, version))
	if errors.Is(err, os.ErrNotExist) {
		return nil, Entry{}, fmt.Errorf("%w: %s@%s", ErrNotFound, name, version)
	}
	if err != nil {
		return nil, Entry{}, err
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, Entry{}, fmt.Errorf("%s@%s: %w", name, version, err)
	}
	idx, err := s.readIndex()
	if err != nil {
		return nil, Entry{}, err
	}
	for _, e := range idx {
		if e.Name == name && e.Version == version {
			return &m, e, nil
		}
	}
	return &m, entryFor(&m, "", time.Time{}), nil
}

// Profile returns the compiled profile for name@version; a saved draft
// that does not compile is ErrNotCompiled.
func (s *Store) Profile(name, version string) (*workflowmodel.Profile, error) {
	if IsBuiltin(name) && (version == "" || version == hivedefault.Version) {
		return hivedefault.Profile(), nil
	}
	m, _, err := s.Get(name, version)
	if err != nil {
		return nil, err
	}
	p, diags, err := workflowmodel.Compile(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %s@%s: %s", ErrNotCompiled, name, version, summarize(diags))
	}
	return p, nil
}

func summarize(diags []workflowmodel.Diagnostic) string {
	var parts []string
	for _, d := range diags {
		if d.Severity == workflowmodel.DiagError {
			parts = append(parts, d.Code)
		}
	}
	return strings.Join(parts, ", ")
}

// Save writes m as metadata.name@spec.meta.version and refreshes the
// index. The model must validate (shape); a compile failure is recorded
// on the entry, not refused: drafts live in the library, never in a
// binding.
func (s *Store) Save(m *workflowmodel.Model, by string, now time.Time) (Entry, error) {
	return s.save(m, by, now, replaceVersion)
}

// SaveNextVersion assigns and saves the next integer version under the same
// lock. The supplied model is left unchanged.
func (s *Store) SaveNextVersion(m *workflowmodel.Model, by string, now time.Time) (Entry, error) {
	return s.save(m, by, now, nextVersion)
}

// SaveImportedVersion keeps a free requested version, or assigns the next
// integer version when the requested slot is occupied or empty.
func (s *Store) SaveImportedVersion(m *workflowmodel.Model, by string, now time.Time) (Entry, error) {
	return s.save(m, by, now, preferVersion)
}

type versionMode uint8

const (
	replaceVersion versionMode = iota
	nextVersion
	preferVersion
)

func (s *Store) save(m *workflowmodel.Model, by string, now time.Time, mode versionMode) (Entry, error) {
	if m == nil {
		return Entry{}, errors.New("nil model")
	}
	name := m.Metadata.Name
	if IsBuiltin(name) {
		return Entry{}, fmt.Errorf("%w: %s", ErrReadOnly, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var idx []Entry
	if mode != replaceVersion {
		if !nameRe.MatchString(name) {
			return Entry{}, fmt.Errorf("%w: %q", ErrBadName, name)
		}
		var err error
		idx, err = s.readIndex()
		if err != nil {
			return Entry{}, err
		}
		preferred := m.Spec.Meta.Version
		allocate := mode == nextVersion || preferred == ""
		if !allocate {
			occupied, err := s.versionOccupied(name, preferred, idx)
			if err != nil {
				return Entry{}, err
			}
			allocate = occupied
		}
		if allocate {
			version, err := s.nextVersion(name, idx)
			if err != nil {
				return Entry{}, err
			}
			versioned := *m
			versioned.Spec.Meta.Version = version
			m = &versioned
		}
	}
	version := m.Spec.Meta.Version
	if err := checkKey(name, version); err != nil {
		return Entry{}, err
	}
	if err := m.Validate(); err != nil {
		return Entry{}, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Entry{}, err
	}
	if mode == replaceVersion {
		idx, err = s.readIndex()
		if err != nil {
			return Entry{}, err
		}
	}
	e := entryFor(m, by, now)
	replaced := false
	for i := range idx {
		if idx[i].Name == name && idx[i].Version == version {
			e.Adoption, e.Ratings, e.Benchmarks = idx[i].Adoption, idx[i].Ratings, idx[i].Benchmarks
			idx[i] = e
			replaced = true
		}
	}
	if !replaced {
		idx = append(idx, e)
	}
	if err := s.saveModelAndIndex(s.modelPath(name, version), b, idx); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// versionOccupied requires s.mu.
func (s *Store) versionOccupied(name, version string, idx []Entry) (bool, error) {
	if err := checkKey(name, version); err != nil {
		return false, err
	}
	for _, e := range idx {
		if e.Name == name && e.Version == version {
			return true, nil
		}
	}
	_, err := os.Lstat(s.modelPath(name, version))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// The staged catalogue catches write failures before publishing the model.
// Roll back the model if the catalogue rename fails. An interruption between
// the two renames can still require Rebuild.
func (s *Store) saveModelAndIndex(path string, model []byte, idx []Entry) error {
	index, err := marshalIndex(idx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	indexPath := filepath.Join(s.dir(), IndexFile)
	indexTmp, err := stageWrite(indexPath, index)
	if err != nil {
		return err
	}
	defer os.Remove(indexTmp)

	backup, err := backupModel(path)
	if err != nil {
		return err
	}
	defer func() {
		if backup != "" {
			os.Remove(backup)
			os.Remove(filepath.Dir(backup))
		}
	}()
	if err := writeAtomic(path, model); err != nil {
		return err
	}
	if err := os.Rename(indexTmp, indexPath); err != nil {
		var rollbackErr error
		if backup != "" {
			rollbackErr = os.Rename(backup, path)
		} else {
			rollbackErr = os.Remove(path)
		}
		if rollbackErr != nil {
			if backup != "" {
				rollbackErr = fmt.Errorf("restore workflow model from %s: %w", backup, rollbackErr)
				backup = ""
			} else {
				rollbackErr = fmt.Errorf("remove unpublished workflow model: %w", rollbackErr)
			}
			return errors.Join(err, rollbackErr)
		}
		return err
	}
	return nil
}

const modelBackupFile = "model"

// backupModel preserves the original inode when hard links are available.
func backupModel(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("workflow model path is a directory: %s", path)
	}
	dir, err := os.MkdirTemp(filepath.Dir(path), ".rollback-")
	if err != nil {
		return "", err
	}
	backup := filepath.Join(dir, modelBackupFile)
	if err := os.Link(path, backup); err == nil {
		return backup, nil
	}
	// Some filesystems lack hard links. Keep a copy with the original mode.
	if err := copyModelBackup(path, dir, info.Mode()); err != nil {
		os.Remove(backup)
		os.Remove(dir)
		return "", err
	}
	return backup, nil
}

func copyModelBackup(path, dir string, mode os.FileMode) error {
	backupDir, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer backupDir.Close()
	previous, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := backupDir.WriteFile(modelBackupFile, previous, 0o600); err != nil {
		return err
	}
	return backupDir.Chmod(modelBackupFile, mode)
}

// Delete removes one version. Whether a repository is bound to it is the
// caller's check: the config owns bindings.
func (s *Store) Delete(name, version string) error {
	if IsBuiltin(name) {
		return fmt.Errorf("%w: %s", ErrReadOnly, name)
	}
	if err := checkKey(name, version); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.modelPath(name, version)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s@%s", ErrNotFound, name, version)
		}
		return err
	}
	_ = os.Remove(filepath.Join(s.dir(), name)) // succeeds only when empty
	idx, err := s.readIndex()
	if err != nil {
		return err
	}
	out := idx[:0]
	for _, e := range idx {
		if e.Name != name || e.Version != version {
			out = append(out, e)
		}
	}
	return s.writeIndex(out)
}

// Versions lists the entries of name, newest save first.
func (s *Store) Versions(name string) ([]Entry, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range all {
		if e.Name == name {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SavedAt.After(out[j].SavedAt) })
	return out, nil
}

// Rebuild regenerates the index from the model files on disk (recovery
// after a partial write or a hand-copied directory).
func (s *Store) Rebuild(now time.Time) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := os.ReadDir(s.dir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, s.writeIndex(nil)
	}
	if err != nil {
		return nil, err
	}
	var idx []Entry
	for _, d := range names {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.dir(), d.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.dir(), d.Name(), f.Name()))
			if err != nil {
				return nil, err
			}
			var m workflowmodel.Model
			if err := json.Unmarshal(b, &m); err != nil {
				continue // not a model; leave it alone
			}
			if m.Metadata.Name != d.Name() || m.Spec.Meta.Version+".json" != f.Name() {
				continue // misfiled
			}
			idx = append(idx, entryFor(&m, "", now))
		}
	}
	return idx, s.writeIndex(idx)
}

func sortIndex(idx []Entry) {
	sort.SliceStable(idx, func(i, j int) bool {
		if idx[i].Name != idx[j].Name {
			return idx[i].Name < idx[j].Name
		}
		return idx[i].Version < idx[j].Version
	})
}

func (s *Store) readIndex() ([]Entry, error) {
	b, err := os.ReadFile(filepath.Join(s.dir(), IndexFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var idx []Entry
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("%s: %w", IndexFile, err)
	}
	return idx, nil
}

func marshalIndex(idx []Entry) ([]byte, error) {
	sortIndex(idx)
	if idx == nil {
		idx = []Entry{}
	}
	return json.MarshalIndent(idx, "", "  ")
}

func (s *Store) writeIndex(idx []Entry) error {
	b, err := marshalIndex(idx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir(), IndexFile), b)
}

func stageWrite(path string, b []byte) (string, error) {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	if err := writeStagedFile(file, b); err != nil {
		return "", err
	}
	return tmp, nil
}

func writeStagedFile(file *os.File, b []byte) error {
	_, writeErr := file.Write(b)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		if cleanupErr := os.Remove(file.Name()); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove incomplete staging file: %w", cleanupErr))
		}
		return err
	}
	return nil
}

func writeAtomic(path string, b []byte) error {
	tmp, err := stageWrite(path, b)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

// NextVersion returns the next free integer version of name: one more
// than the largest integer version saved so far, or "1" for a name with no
// integer versions. Non-integer versions (semver, tags) are ignored.
// ErrVersionExhausted means the next integer would exceed the version limit.
func (s *Store) NextVersion(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readIndex()
	if err != nil {
		return "", err
	}
	if IsBuiltin(name) {
		all = append(all, builtinEntry())
	}
	return s.nextVersion(name, all)
}

// nextVersion requires s.mu.
func (s *Store) nextVersion(name string, all []Entry) (string, error) {
	var max big.Int
	consider := func(version string) {
		if !decimalVersionRe.MatchString(version) {
			return
		}
		var n big.Int
		n.SetString(version, 10)
		if n.Cmp(&max) > 0 {
			max.Set(&n)
		}
	}
	for _, e := range all {
		if e.Name == name {
			consider(e.Version)
		}
	}
	files, err := os.ReadDir(filepath.Join(s.dir(), name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// A partial save can leave a model file outside the index. Directories
	// only occupy their exact filename and don't affect the maximum version.
	occupied := map[string]bool{}
	for _, file := range files {
		version, ok := strings.CutSuffix(file.Name(), ".json")
		if !ok {
			continue
		}
		occupied[version] = true
		if !file.IsDir() {
			consider(version)
		}
	}
	one := big.NewInt(1)
	for {
		next := max.Add(&max, one).String()
		if !decimalVersionRe.MatchString(next) {
			return "", fmt.Errorf("%w for %s", ErrVersionExhausted, name)
		}
		if !occupied[next] {
			return next, nil
		}
	}
}

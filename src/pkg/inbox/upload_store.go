package inbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Upload store layout: <Dir>/<id>/ (0700) holds meta.json, original (the
// file as dropped) and extracted.md, each 0600. Nothing under it is ever
// executed or served raw to a browser.
const (
	uploadDirMode      = 0o700
	uploadFileMode     = 0o600
	uploadMetaFile     = "meta.json"
	uploadOriginalFile = "original"
	uploadTextFile     = "extracted.md"
	// uploadIDRandBytes is the random suffix of an upload id.
	uploadIDRandBytes = 4
	// uploadIDTimeLayout is the sortable time prefix of an upload id.
	uploadIDTimeLayout = "20060102t150405"
	// hoursPerDay converts retention days.
	hoursPerDay = 24
)

// uploadIDRe is the only id shape the store reads, so a path segment from a
// request can never leave the store directory.
var uploadIDRe = regexp.MustCompile(`^[0-9]{8}t[0-9]{6}-[0-9a-f]{8}$`)

// ErrUploadNotFound is returned for an unknown or malformed upload id.
var ErrUploadNotFound = errors.New("unknown upload")

// UploadMeta is what the store keeps about one upload besides its bytes.
type UploadMeta struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// MIME is the playback Content-Type of an audio or video upload.
	MIME string `json:"mime,omitempty"`
	// Origin is "upload" for a dropped file, "link" for a fetched URL.
	Origin   string `json:"origin"`
	Filename string `json:"filename,omitempty"`
	// URL and Host are set for links; the URL is provenance only.
	URL     string    `json:"url,omitempty"`
	Host    string    `json:"host,omitempty"`
	Title   string    `json:"title"`
	Summary string    `json:"summary,omitempty"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	AddedBy string    `json:"added_by"`
	AddedAt time.Time `json:"added_at"`
	// OriginalDeletedAt is set once retention removed the original file.
	OriginalDeletedAt *time.Time `json:"original_deleted_at,omitempty"`
	// FactSlug/FactLayer locate the Knowledge fact written for the upload.
	FactSlug  string `json:"fact_slug,omitempty"`
	FactLayer string `json:"fact_layer,omitempty"`
}

// UploadStore keeps uploads on the hive's data volume.
type UploadStore struct {
	Dir string
	Now func() time.Time
	// mu serialises the quota check with the write, and meta rewrites.
	mu sync.Mutex
}

// NewUploadStore returns a store rooted at dir (created on first write).
func NewUploadStore(dir string) *UploadStore { return &UploadStore{Dir: dir} }

func (s *UploadStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// NewID returns a fresh, time-sortable upload id.
func (s *UploadStore) NewID() (string, error) {
	b := make([]byte, uploadIDRandBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return s.now().Format(uploadIDTimeLayout) + "-" + hex.EncodeToString(b), nil
}

func (s *UploadStore) dir(id string) (string, error) {
	if !uploadIDRe.MatchString(id) {
		return "", ErrUploadNotFound
	}
	return filepath.Join(s.Dir, id), nil
}

// Put stores a new upload: the original bytes, the extracted text and meta.
// It refuses the write when it would take the store past quota bytes.
func (s *UploadStore) Put(meta UploadMeta, original []byte, extracted string, quota int64) error {
	dir, err := s.dir(meta.ID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	need := int64(len(original) + len(extracted))
	if quota > 0 {
		used, err := s.usage()
		if err != nil {
			return err
		}
		if used+need > quota {
			return uploadErr(UploadErrQuota, "the Inbox upload quota is full (%s of %s used; this file needs %s): remove old uploads or raise inbox.uploads.quota_bytes", humanBytes(used), humanBytes(quota), humanBytes(need))
		}
	}
	if err := os.MkdirAll(s.Dir, uploadDirMode); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, uploadDirMode); err != nil {
		return err
	}
	if err := os.Mkdir(dir, uploadDirMode); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := writeNew(filepath.Join(dir, uploadOriginalFile), original); err != nil {
		return err
	}
	if err := writeNew(filepath.Join(dir, uploadTextFile), []byte(extracted)); err != nil {
		return err
	}
	if err := s.writeMeta(dir, meta); err != nil {
		return err
	}
	ok = true
	return nil
}

// PutStream stores an upload whose original is streamed from r (audio and
// video), at most limit bytes and within quota. The extracted text starts
// empty (the machine transcript is kept by the transcript store). It
// returns the stored size.
func (s *UploadStore) PutStream(meta UploadMeta, r io.Reader, limit, quota int64) (int64, error) {
	dir, err := s.dir(meta.ID)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if quota > 0 {
		used, err := s.usage()
		if err != nil {
			return 0, err
		}
		if used >= quota {
			return 0, uploadErr(UploadErrQuota, "the Inbox upload quota is full (%s of %s used): remove old uploads or raise inbox.uploads.quota_bytes", humanBytes(used), humanBytes(quota))
		}
		if room := quota - used; limit <= 0 || room < limit {
			limit = room
		}
	}
	if err := os.MkdirAll(s.Dir, uploadDirMode); err != nil {
		return 0, err
	}
	if err := os.Chmod(s.Dir, uploadDirMode); err != nil {
		return 0, err
	}
	if err := os.Mkdir(dir, uploadDirMode); err != nil {
		return 0, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	f, err := os.OpenFile(filepath.Join(dir, uploadOriginalFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, uploadFileMode)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if n > limit {
		return 0, uploadErr(UploadErrTooLarge, "the file is too large or the upload quota is nearly full (the limit for this file is %s)", humanBytes(limit))
	}
	if n == 0 {
		return 0, uploadErr(UploadErrEmpty, "the file is empty")
	}
	if err := writeNew(filepath.Join(dir, uploadTextFile), nil); err != nil {
		return 0, err
	}
	meta.Size = n
	if err := s.writeMeta(dir, meta); err != nil {
		return 0, err
	}
	ok = true
	return n, nil
}

// OpenOriginal opens an upload's original file (until retention removes it).
func (s *UploadStore) OpenOriginal(id string) (*os.File, error) {
	dir, err := s.dir(id)
	if err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(dir, uploadOriginalFile))
}

// writeNew creates path exclusively with uploadFileMode.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, uploadFileMode)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(path, uploadFileMode)
	}
	return werr
}

func (s *UploadStore) writeMeta(dir string, meta UploadMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, uploadMetaFile+".tmp")
	_ = os.Remove(tmp)
	if err := writeNew(tmp, data); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, uploadMetaFile))
}

// UpdateMeta rewrites one upload's meta through fn.
func (s *UploadStore) UpdateMeta(id string, fn func(*UploadMeta)) (UploadMeta, error) {
	dir, err := s.dir(id)
	if err != nil {
		return UploadMeta{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := readMeta(dir)
	if err != nil {
		return UploadMeta{}, err
	}
	fn(&m)
	return m, s.writeMeta(dir, m)
}

func readMeta(dir string) (UploadMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, uploadMetaFile))
	if errors.Is(err, fs.ErrNotExist) {
		return UploadMeta{}, ErrUploadNotFound
	}
	if err != nil {
		return UploadMeta{}, err
	}
	var m UploadMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return UploadMeta{}, fmt.Errorf("upload meta: %w", err)
	}
	return m, nil
}

// Get returns one upload's meta.
func (s *UploadStore) Get(id string) (UploadMeta, error) {
	dir, err := s.dir(id)
	if err != nil {
		return UploadMeta{}, err
	}
	return readMeta(dir)
}

// Text returns one upload's extracted markdown.
func (s *UploadStore) Text(id string) (string, error) {
	dir, err := s.dir(id)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, uploadTextFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrUploadNotFound
	}
	return string(data), err
}

// List returns every upload, newest first. Entries with unreadable meta are
// skipped.
func (s *UploadStore) List() ([]UploadMeta, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []UploadMeta
	for _, e := range entries {
		if !e.IsDir() || !uploadIDRe.MatchString(e.Name()) {
			continue
		}
		m, err := readMeta(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].AddedAt.Equal(out[j].AddedAt) {
			return out[i].AddedAt.After(out[j].AddedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// Usage returns the bytes stored under the store directory.
func (s *UploadStore) Usage() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage()
}

func (s *UploadStore) usage() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.Dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

// SweepRetention deletes original files past their retention: media
// originals after mediaDays, text-document originals after docDays (0 keeps
// them). Extracted text and meta always stay, so the item, its fact and its
// share history survive. It returns how many originals it removed.
func (s *UploadStore) SweepRetention(mediaDays, docDays int) (int, error) {
	list, err := s.List()
	if err != nil {
		return 0, err
	}
	now := s.now()
	removed := 0
	var errs []error
	for _, m := range list {
		if m.OriginalDeletedAt != nil {
			continue
		}
		days := docDays
		if uploadKindIsMedia(m.Kind) {
			days = mediaDays
		}
		if days <= 0 || now.Sub(m.AddedAt) < time.Duration(days)*hoursPerDay*time.Hour {
			continue
		}
		dir, err := s.dir(m.ID)
		if err != nil {
			continue
		}
		if err := os.Remove(filepath.Join(dir, uploadOriginalFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		if _, err := s.UpdateMeta(m.ID, func(mm *UploadMeta) { t := now; mm.OriginalDeletedAt = &t }); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// HasOriginal reports whether the original file is still stored.
func (s *UploadStore) HasOriginal(id string) bool {
	dir, err := s.dir(id)
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, uploadOriginalFile))
	return err == nil && info.Mode().IsRegular()
}

// humanBytes renders a byte count for messages.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

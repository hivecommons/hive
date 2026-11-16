package inbox

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Recordings made in the dashboard (● Record). The browser uploads the
// MediaRecorder output in small, ordered chunks while it records; each chunk
// is fsynced as its own part file, and finalize concatenates the parts into
// one audio file. The hive never decodes the audio itself: it only checks
// the container signature on the first chunk, and the transcription
// provider (the sandboxed whisper subprocess by default) does the decoding.
const (
	// RecordingType is the Inbox source type of dashboard recordings.
	RecordingType = "hive-recording"
	// RecordingConnector is the connector name written into their facts.
	RecordingConnector = "hive"
	// RecordingConsentVersion names the consent notice the dashboard shows
	// before every recording. A start request must acknowledge this version;
	// it is stored in the recording's metadata with the user.
	RecordingConsentVersion = "2026-11-v1"
	// RecordingMaxChunkBytes caps one uploaded chunk (512 KiB), well under
	// the 1 MiB ingress body default and quick enough for the dashboard's
	// 30 s read timeout on a slow uplink.
	RecordingMaxChunkBytes = 512 << 10
	// RecordingChunkMillis is the MediaRecorder timeslice the dashboard
	// uses: one chunk every 10 s (about 40 KB at 32 kbit/s).
	RecordingChunkMillis = 10000
	// RecordingAudioBitsPerSecond is the bitrate the dashboard asks for.
	RecordingAudioBitsPerSecond = 32000
	// RecordingTitleRunes bounds a recording title.
	RecordingTitleRunes = 200
	// RecordingPendingText is the note a recording's fact carries until it
	// has a transcript.
	RecordingPendingText = "Recorded in this hive; no transcript yet."

	// Recording states.
	RecordingActive    = "recording"
	RecordingFinalized = "finalized"

	// Accepted containers (sniffed from the first chunk).
	RecordingContainerWebM = "webm"
	RecordingContainerMP4  = "mp4"

	recordingDirMode  = 0o700
	recordingFileMode = 0o600
	recordingMetaFile = "meta.json"
	// recordingClockGrace tolerates clock and upload lag past max_seconds.
	recordingClockGrace = 5 * time.Minute
	// recordingIDRandBytes is the random suffix length of a recording id.
	recordingIDRandBytes = 4
	// recordingIDTimeLayout is the sortable time prefix of a recording id.
	recordingIDTimeLayout = "20060102t150405"
	// mp4BoxHeader is the size+type header of an ISO BMFF box; a size of 1
	// means a 64-bit size follows (mp4LargeBoxHeader in all).
	mp4BoxHeader      = 8
	mp4LargeBoxHeader = 16
)

// recordingIDRe matches the ids NewRecording mints, so an id is always a
// safe single path element.
var recordingIDRe = regexp.MustCompile(`^[0-9]{8}t[0-9]{6}-[0-9a-f]{8}$`)

// webmMagic is the EBML header every WebM/Matroska file starts with.
var webmMagic = []byte{0x1A, 0x45, 0xDF, 0xA3}

// recordingMIMEs maps the accepted MediaRecorder base types to containers.
var recordingMIMEs = map[string]string{
	"audio/webm": RecordingContainerWebM,
	"audio/mp4":  RecordingContainerMP4,
}

// Recording errors. The dashboard maps each to an HTTP status.
var (
	ErrRecordingDisabled   = errors.New("recording is not enabled on this hive (inbox.recording.enabled)")
	ErrRecordingNotFound   = errors.New("unknown recording")
	ErrRecordingNotYours   = errors.New("this recording was started by someone else")
	ErrRecordingClosed     = errors.New("this recording has already ended")
	ErrRecordingConsent    = errors.New("the recording consent notice must be acknowledged (consent_version " + RecordingConsentVersion + ")")
	ErrRecordingMIME       = errors.New("unsupported recording type: only audio/webm and audio/mp4 are accepted")
	ErrRecordingNotMedia   = errors.New("the first chunk is not a WebM or MP4 recording")
	ErrRecordingTooLarge   = errors.New("the recording is larger than inbox.recording.max_bytes")
	ErrRecordingTooLong    = errors.New("the recording is longer than inbox.recording.max_seconds")
	ErrRecordingQuota      = errors.New("this hive's recording storage is full (inbox.recording.quota_bytes)")
	ErrRecordingChunkSize  = errors.New("a chunk must be between 1 byte and the max_chunk_bytes the start response returned")
	ErrRecordingMismatch   = errors.New("a chunk with this seq was already stored with different bytes")
	ErrRecordingEmpty      = errors.New("no audio was received for this recording")
	ErrRecordingExpired    = errors.New("the audio of this recording has expired")
	ErrRecordingBadRequest = errors.New("invalid recording request")
	// ErrRecordingNotFinished refuses a delete of a recording in progress
	// (discard it from the recorder instead).
	ErrRecordingNotFinished = errors.New("this recording is still in progress; discard it from the recorder")
	// ErrRecordingTranscribing refuses a delete while the recording's
	// transcription is queued or running.
	ErrRecordingTranscribing = errors.New("this recording is being transcribed; delete it once the transcription finishes")
)

// RecordingActiveError is returned when the user already has a recording in
// progress (one at a time per user); ID lets the dashboard resume it.
type RecordingActiveError struct{ ID string }

func (e *RecordingActiveError) Error() string {
	return "you already have a recording in progress (" + e.ID + "); stop it before starting another"
}

// RecordingGapError is an out-of-order chunk: Expected is the next seq the
// hive will accept.
type RecordingGapError struct {
	Got, Expected int
}

func (e *RecordingGapError) Error() string {
	return fmt.Sprintf("chunk %d is out of order: the next chunk must be seq=%d", e.Got, e.Expected)
}

// RecordingMeta is one recording's metadata (meta.json). It never holds the
// audio itself.
type RecordingMeta struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	MIME       string `json:"mime"`
	Container  string `json:"container,omitempty"`
	RecordedBy string `json:"recorded_by"`
	Role       string `json:"role,omitempty"`
	// ConsentVersion / ConsentAt record which notice the recorder
	// acknowledged before this recording, and when.
	ConsentVersion   string    `json:"consent_version"`
	ConsentAt        time.Time `json:"consent_at"`
	IncludesTabAudio bool      `json:"includes_tab_audio"`
	State            string    `json:"state"`
	CreatedAt        time.Time `json:"created_at"`
	LastChunkAt      time.Time `json:"last_chunk_at,omitzero"`
	FinalizedAt      time.Time `json:"finalized_at,omitzero"`
	AutoFinalized    bool      `json:"auto_finalized,omitempty"`
	Chunks           int       `json:"chunks"`
	Bytes            int64     `json:"bytes"`
	// ChunkSHA256 holds each stored chunk's digest, by seq, so a re-sent
	// chunk is recognised.
	ChunkSHA256 []string `json:"chunk_sha256,omitempty"`
	DurationMS  int64    `json:"duration_ms,omitempty"`
	AudioFile   string   `json:"audio_file,omitempty"`
	// AudioDeletedAt is when retention removed the audio file.
	AudioDeletedAt time.Time `json:"audio_deleted_at,omitzero"`
	// ConcatWarning is set when the concatenated file's container structure
	// did not check out (the file is kept; the provider decides).
	ConcatWarning string `json:"concat_warning,omitempty"`
}

// AudioExpired reports that retention deleted the audio.
func (m RecordingMeta) AudioExpired() bool { return !m.AudioDeletedAt.IsZero() }

// audioBytes is what the recording occupies against the quota.
func (m RecordingMeta) audioBytes() int64 {
	if m.AudioExpired() {
		return 0
	}
	return m.Bytes
}

// RecordingStart is a start request.
type RecordingStart struct {
	Title            string
	MIME             string
	ConsentVersion   string
	IncludesTabAudio bool
	User             string
	Role             string
}

// RecordingChunk is a stored (or recognised) chunk.
type RecordingChunk struct {
	Seq       int   `json:"seq"`
	NextSeq   int   `json:"next_seq"`
	Bytes     int64 `json:"bytes"`
	Duplicate bool  `json:"duplicate,omitempty"`
}

// RecordingStore keeps recordings under Dir/<id>/ (0700 directories, 0600
// files): part-<seq>.bin while recording, then audio.webm or audio.mp4 plus
// meta.json. It is safe for concurrent use.
type RecordingStore struct {
	Dir string
	// Now is injectable for tests.
	Now func() time.Time

	mu     sync.Mutex
	loaded bool
	metas  map[string]*RecordingMeta
}

// NewRecordingStore returns a store rooted at dir.
func NewRecordingStore(dir string) *RecordingStore { return &RecordingStore{Dir: dir} }

func (s *RecordingStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// load reads every meta.json once. Callers hold s.mu.
func (s *RecordingStore) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.metas = map[string]*RecordingMeta{}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !recordingIDRe.MatchString(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, e.Name(), recordingMetaFile))
		if err != nil {
			continue
		}
		var m RecordingMeta
		if json.Unmarshal(data, &m) != nil || m.ID != e.Name() {
			continue
		}
		s.metas[m.ID] = &m
	}
}

func (s *RecordingStore) dir(id string) string { return filepath.Join(s.Dir, id) }

func partName(seq int) string { return fmt.Sprintf("part-%06d.bin", seq) }

// usedBytes sums the audio held on disk. Callers hold s.mu.
func (s *RecordingStore) usedBytes() int64 {
	var n int64
	for _, m := range s.metas {
		n += m.audioBytes()
	}
	return n
}

// UsedBytes returns the recording audio currently kept on this hive.
func (s *RecordingStore) UsedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return s.usedBytes()
}

// RecordingContainer returns the container for a MediaRecorder MIME type
// ("audio/webm;codecs=opus" → webm), or "" when it is not accepted.
func RecordingContainer(mimeType string) string {
	base, _, err := mime.ParseMediaType(strings.TrimSpace(mimeType))
	if err != nil {
		return ""
	}
	return recordingMIMEs[strings.ToLower(base)]
}

// SniffRecordingContainer names the container a chunk starts with: webm for
// the EBML header, mp4 for an ISO BMFF ftyp box, "" for anything else.
func SniffRecordingContainer(head []byte) string {
	switch {
	case bytes.HasPrefix(head, webmMagic):
		return RecordingContainerWebM
	case len(head) >= mp4BoxHeader && string(head[4:8]) == "ftyp":
		return RecordingContainerMP4
	}
	return ""
}

// newRecordingID mints a sortable, path-safe id.
func newRecordingID(now time.Time) (string, error) {
	b := make([]byte, recordingIDRandBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return now.UTC().Format(recordingIDTimeLayout) + "-" + hex.EncodeToString(b), nil
}

// Create starts a recording for req.User. Only one recording per user may
// be in progress; consent must name RecordingConsentVersion.
func (s *RecordingStore) Create(req RecordingStart, cfg config.InboxRecording) (RecordingMeta, error) {
	if strings.TrimSpace(req.ConsentVersion) != RecordingConsentVersion {
		return RecordingMeta{}, ErrRecordingConsent
	}
	container := RecordingContainer(req.MIME)
	if container == "" {
		return RecordingMeta{}, ErrRecordingMIME
	}
	if strings.TrimSpace(req.User) == "" {
		return RecordingMeta{}, ErrRecordingBadRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	for _, m := range s.metas {
		if m.State == RecordingActive && m.RecordedBy == req.User {
			return RecordingMeta{}, &RecordingActiveError{ID: m.ID}
		}
	}
	if s.usedBytes() >= cfg.EffectiveQuotaBytes() {
		return RecordingMeta{}, ErrRecordingQuota
	}
	now := s.now()
	id, err := newRecordingID(now)
	if err != nil {
		return RecordingMeta{}, err
	}
	title := truncateRunes(strings.TrimSpace(req.Title), RecordingTitleRunes)
	if title == "" {
		title = "Recording " + now.Format("2006-01-02 15:04") + " UTC"
	}
	m := &RecordingMeta{
		ID: id, Title: title, MIME: strings.TrimSpace(req.MIME), Container: container,
		RecordedBy: req.User, Role: req.Role, ConsentVersion: RecordingConsentVersion, ConsentAt: now,
		IncludesTabAudio: req.IncludesTabAudio, State: RecordingActive, CreatedAt: now,
	}
	if err := os.MkdirAll(s.Dir, recordingDirMode); err != nil {
		return RecordingMeta{}, err
	}
	if err := os.Mkdir(s.dir(id), recordingDirMode); err != nil {
		return RecordingMeta{}, err
	}
	if err := s.writeMeta(m); err != nil {
		_ = os.RemoveAll(s.dir(id))
		return RecordingMeta{}, err
	}
	s.metas[id] = m
	return *m, nil
}

// get returns the meta for id owned by user. Callers hold s.mu.
func (s *RecordingStore) get(id, user string) (*RecordingMeta, error) {
	s.load()
	m, ok := s.metas[id]
	if !ok {
		return nil, ErrRecordingNotFound
	}
	if user != "" && m.RecordedBy != user {
		return nil, ErrRecordingNotYours
	}
	return m, nil
}

// Append stores chunk seq of recording id. Chunks are accepted in order
// only: a gap is a *RecordingGapError naming the expected seq, and a re-sent
// chunk with the same bytes (same sha256) is acknowledged again without
// being written twice. Each part is fsynced before the ack.
func (s *RecordingStore) Append(id, user string, seq int, data []byte, cfg config.InboxRecording) (RecordingChunk, error) {
	if len(data) == 0 || len(data) > RecordingMaxChunkBytes {
		return RecordingChunk{}, ErrRecordingChunkSize
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.get(id, user)
	if err != nil {
		return RecordingChunk{}, err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if seq >= 0 && seq < m.Chunks {
		if seq < len(m.ChunkSHA256) && m.ChunkSHA256[seq] == digest {
			return RecordingChunk{Seq: seq, NextSeq: m.Chunks, Bytes: m.Bytes, Duplicate: true}, nil
		}
		return RecordingChunk{}, ErrRecordingMismatch
	}
	if m.State != RecordingActive {
		return RecordingChunk{}, ErrRecordingClosed
	}
	if seq != m.Chunks {
		return RecordingChunk{}, &RecordingGapError{Got: seq, Expected: m.Chunks}
	}
	if seq == 0 && SniffRecordingContainer(data) != m.Container {
		return RecordingChunk{}, ErrRecordingNotMedia
	}
	now := s.now()
	if now.Sub(m.CreatedAt) > time.Duration(cfg.EffectiveMaxSeconds())*time.Second+recordingClockGrace {
		return RecordingChunk{}, ErrRecordingTooLong
	}
	if m.Bytes+int64(len(data)) > cfg.EffectiveMaxBytes() {
		return RecordingChunk{}, ErrRecordingTooLarge
	}
	if s.usedBytes()+int64(len(data)) > cfg.EffectiveQuotaBytes() {
		return RecordingChunk{}, ErrRecordingQuota
	}
	if err := writeFileSync(filepath.Join(s.dir(id), partName(seq)), data); err != nil {
		return RecordingChunk{}, err
	}
	next := *m
	next.Chunks++
	next.Bytes += int64(len(data))
	next.ChunkSHA256 = append(append([]string(nil), m.ChunkSHA256...), digest)
	next.LastChunkAt = now
	if err := s.writeMeta(&next); err != nil {
		_ = os.Remove(filepath.Join(s.dir(id), partName(seq)))
		return RecordingChunk{}, err
	}
	*m = next
	return RecordingChunk{Seq: seq, NextSeq: m.Chunks, Bytes: m.Bytes}, nil
}

// Finalize ends recording id: the parts are concatenated in order into
// audio.<container> and removed. durationMS is the recorder's own count
// (pauses excluded); it is clamped to the recording's wall-clock span and
// max_seconds, and replaced by that span when the client sent none. user ""
// is the idle sweep. A recording with no chunk is removed and reported as
// ErrRecordingEmpty.
func (s *RecordingStore) Finalize(id, user string, durationMS int64, cfg config.InboxRecording) (RecordingMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.get(id, user)
	if err != nil {
		return RecordingMeta{}, err
	}
	if m.State != RecordingActive {
		return RecordingMeta{}, ErrRecordingClosed
	}
	if m.Chunks == 0 {
		_ = os.RemoveAll(s.dir(id))
		delete(s.metas, id)
		return RecordingMeta{}, ErrRecordingEmpty
	}
	next := *m
	next.AudioFile = "audio." + m.Container
	warn, err := s.concat(&next)
	if err != nil {
		return RecordingMeta{}, err
	}
	next.ConcatWarning = warn
	span := next.LastChunkAt.Sub(next.CreatedAt).Milliseconds()
	maxMS := int64(cfg.EffectiveMaxSeconds()) * int64(time.Second/time.Millisecond)
	if durationMS <= 0 || (span > 0 && durationMS > span+int64(RecordingChunkMillis)) {
		durationMS = span
	}
	if durationMS > maxMS {
		durationMS = maxMS
	}
	if durationMS < 0 {
		durationMS = 0
	}
	next.DurationMS = durationMS
	next.State = RecordingFinalized
	next.FinalizedAt = s.now()
	next.AutoFinalized = user == ""
	if err := s.writeMeta(&next); err != nil {
		return RecordingMeta{}, err
	}
	for seq := 0; seq < next.Chunks; seq++ {
		_ = os.Remove(filepath.Join(s.dir(id), partName(seq)))
	}
	*m = next
	return *m, nil
}

// concat writes the parts of m into its audio file through a temp file,
// fsyncs and renames it. It returns a warning when the container structure
// of the result does not check out.
func (s *RecordingStore) concat(m *RecordingMeta) (string, error) {
	dir := s.dir(m.ID)
	tmp, err := os.CreateTemp(dir, ".audio-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Chmod(recordingFileMode); err != nil {
		return fail(err)
	}
	for seq := 0; seq < m.Chunks; seq++ {
		part, err := os.Open(filepath.Join(dir, partName(seq)))
		if err != nil {
			return fail(fmt.Errorf("chunk %d: %w", seq, err))
		}
		_, err = io.Copy(tmp, part)
		_ = part.Close()
		if err != nil {
			return fail(err)
		}
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	final := filepath.Join(dir, m.AudioFile)
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	syncDir(dir)
	return checkRecordingContainer(final, m.Container), nil
}

// checkRecordingContainer verifies the concatenated file: for mp4 every
// top-level ISO BMFF box must chain exactly to the end of the file, starting
// with ftyp (fragmented MP4 from Safari is ftyp, moov, then moof+mdat pairs);
// for webm the EBML header must lead. It never decodes audio.
func checkRecordingContainer(path, container string) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	if container == RecordingContainerWebM {
		head := make([]byte, len(webmMagic))
		if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, webmMagic) {
			return "the recording does not start with a WebM header"
		}
		return ""
	}
	types, err := MP4TopLevelBoxes(f)
	if err != nil {
		return err.Error()
	}
	if len(types) == 0 || types[0] != "ftyp" {
		return "the recording does not start with an MP4 ftyp box"
	}
	return ""
}

// MP4TopLevelBoxes walks the top-level boxes of an ISO BMFF stream and
// returns their types in order. It fails when a box size overruns the
// stream or is malformed, which is how a broken concatenation shows.
func MP4TopLevelBoxes(r io.ReadSeeker) ([]string, error) {
	var types []string
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	var off int64
	hdr := make([]byte, mp4LargeBoxHeader)
	for off < end {
		if end-off < mp4BoxHeader {
			return types, fmt.Errorf("mp4: %d trailing bytes after the last box", end-off)
		}
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return types, err
		}
		if _, err := io.ReadFull(r, hdr[:mp4BoxHeader]); err != nil {
			return types, err
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		switch size {
		case 0:
			size = end - off
		case 1:
			if _, err := io.ReadFull(r, hdr[mp4BoxHeader:mp4LargeBoxHeader]); err != nil {
				return types, err
			}
			size = int64(binary.BigEndian.Uint64(hdr[mp4BoxHeader:mp4LargeBoxHeader]))
			if size < mp4LargeBoxHeader {
				return types, fmt.Errorf("mp4: box %q has an invalid size", typ)
			}
		default:
			if size < mp4BoxHeader {
				return types, fmt.Errorf("mp4: box %q has an invalid size", typ)
			}
		}
		if off+size > end {
			return types, fmt.Errorf("mp4: box %q overruns the file", typ)
		}
		types = append(types, typ)
		off += size
	}
	return types, nil
}

// Abort removes an in-progress recording. The recorder may abort their own;
// an owner (asOwner) may abort anyone's.
func (s *RecordingStore) Abort(id, user string, asOwner bool) (RecordingMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := user
	if asOwner {
		owner = ""
	}
	m, err := s.get(id, owner)
	if err != nil {
		return RecordingMeta{}, err
	}
	if m.State != RecordingActive {
		return RecordingMeta{}, ErrRecordingClosed
	}
	out := *m
	if err := os.RemoveAll(s.dir(id)); err != nil {
		return RecordingMeta{}, err
	}
	delete(s.metas, id)
	return out, nil
}

// Delete removes a finished recording: its directory (audio, any chunk
// left over, meta.json) and its metadata. The recorder may delete their own;
// an owner (asOwner) may delete anyone's. A recording still in progress is
// aborted, not deleted (ErrRecordingNotFinished).
func (s *RecordingStore) Delete(id, user string, asOwner bool) (RecordingMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := user
	if asOwner {
		owner = ""
	} else if user == "" {
		return RecordingMeta{}, ErrRecordingNotYours
	}
	m, err := s.get(id, owner)
	if err != nil {
		return RecordingMeta{}, err
	}
	if m.State != RecordingFinalized {
		return RecordingMeta{}, ErrRecordingNotFinished
	}
	out := *m
	if err := os.RemoveAll(s.dir(id)); err != nil {
		return RecordingMeta{}, err
	}
	delete(s.metas, id)
	return out, nil
}

// Get returns one recording's metadata.
func (s *RecordingStore) Get(id string) (RecordingMeta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.get(id, "")
	if err != nil {
		return RecordingMeta{}, false
	}
	return *m, true
}

// List returns every recording, newest first.
func (s *RecordingStore) List() []RecordingMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	out := make([]RecordingMeta, 0, len(s.metas))
	for _, m := range s.metas {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// OpenAudio opens a finalized recording's audio file for reading.
func (s *RecordingStore) OpenAudio(id string) (*os.File, RecordingMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.get(id, "")
	if err != nil {
		return nil, RecordingMeta{}, err
	}
	if m.State != RecordingFinalized || m.AudioFile == "" {
		return nil, *m, ErrNoAudio
	}
	if m.AudioExpired() {
		return nil, *m, ErrRecordingExpired
	}
	f, err := os.Open(filepath.Join(s.dir(id), m.AudioFile))
	if err != nil {
		return nil, *m, err
	}
	return f, *m, nil
}

// RecordingSweepResult is what one sweep changed.
type RecordingSweepResult struct {
	// Finalized were in progress and idle past idle_finalize_minutes.
	Finalized []RecordingMeta
	// Removed were idle with no audio at all.
	Removed []RecordingMeta
	// Expired had their audio deleted by retention.
	Expired []RecordingMeta
}

// Sweep finalizes idle recordings and deletes audio past retention. The
// metadata, the transcript and the Knowledge fact are kept.
func (s *RecordingStore) Sweep(cfg config.InboxRecording) (RecordingSweepResult, error) {
	var res RecordingSweepResult
	var errs []error
	now := s.now()
	for _, m := range s.List() {
		switch {
		case m.State == RecordingActive:
			last := m.LastChunkAt
			if last.IsZero() {
				last = m.CreatedAt
			}
			if now.Sub(last) < cfg.EffectiveIdleFinalize() {
				continue
			}
			fin, err := s.Finalize(m.ID, "", 0, cfg)
			switch {
			case errors.Is(err, ErrRecordingEmpty):
				res.Removed = append(res.Removed, m)
			case err != nil:
				errs = append(errs, fmt.Errorf("finalize %s: %w", m.ID, err))
			default:
				res.Finalized = append(res.Finalized, fin)
			}
		case m.State == RecordingFinalized && !m.AudioExpired() && m.AudioFile != "":
			if now.Sub(m.FinalizedAt) < cfg.EffectiveRetention() {
				continue
			}
			exp, err := s.expire(m.ID)
			if err != nil {
				errs = append(errs, fmt.Errorf("expire %s: %w", m.ID, err))
				continue
			}
			res.Expired = append(res.Expired, exp)
		}
	}
	return res, errors.Join(errs...)
}

// expire deletes one recording's audio file and records when.
func (s *RecordingStore) expire(id string) (RecordingMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.get(id, "")
	if err != nil {
		return RecordingMeta{}, err
	}
	if err := os.Remove(filepath.Join(s.dir(id), m.AudioFile)); err != nil && !os.IsNotExist(err) {
		return RecordingMeta{}, err
	}
	next := *m
	next.AudioDeletedAt = s.now()
	if err := s.writeMeta(&next); err != nil {
		return RecordingMeta{}, err
	}
	*m = next
	return *m, nil
}

// writeMeta writes meta.json through a temp file, fsync and rename.
func (s *RecordingStore) writeMeta(m *RecordingMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dir := s.dir(m.ID)
	tmp, err := os.CreateTemp(dir, ".meta-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Chmod(recordingFileMode)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, filepath.Join(dir, recordingMetaFile))
	}
	if werr != nil {
		_ = os.Remove(name)
		return werr
	}
	syncDir(dir)
	return nil
}

// writeFileSync writes data to path (0600) and fsyncs it.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, recordingFileMode)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
	}
	return werr
}

// syncDir fsyncs a directory so a rename or new file survives a crash. It
// is best effort: some filesystems refuse directory fsync.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

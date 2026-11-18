package inbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Transcription statuses for one Inbox item.
const (
	TranscriptionQueued  = "queued"
	TranscriptionRunning = "running"
	TranscriptionDone    = "done"
	TranscriptionFailed  = "failed"
	TranscriptionSkipped = "skipped"
)

// Skip reasons prefix TranscriptionState.Reason for skipped items.
const (
	SkipTooLong     = "too long"
	SkipTooLarge    = "too large"
	SkipUnsupported = "unsupported"
	SkipBudget      = "budget"
)

const (
	// transcriptDirMode / transcriptFileMode keep transcripts owner-only on
	// the spoke's data volume.
	transcriptDirMode  = 0o700
	transcriptFileMode = 0o600
	// transcriptUsageFile is the per-month minutes / per-day runs ledger.
	transcriptUsageFile = "_usage.json"
	// usageKeepDays / usageKeepMonths bound the ledger's history.
	usageKeepDays   = 40
	usageKeepMonths = 13
	// usageDayLayout / usageMonthLayout key the ledger (UTC).
	usageDayLayout   = "2006-01-02"
	usageMonthLayout = "2006-01"
	// safeNameHashLen is the hex digest suffix added to a rewritten name.
	safeNameHashLen = 8
)

// TranscriptionState is what an Inbox item reports about its machine
// transcript.
type TranscriptionState struct {
	Status string `json:"status"`
	// Reason explains a failed or skipped run ("budget: ...").
	Reason    string    `json:"reason,omitempty"`
	Attempts  int       `json:"attempts,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`

	// Live progress while queued or running (zero otherwise). Progress is
	// 0..1; Estimated marks progress and ETA taken from the provider's
	// typical (or this hive's measured) speed rather than measured output.
	Phase            string     `json:"phase,omitempty"`
	Progress         float64    `json:"progress,omitempty"`
	Estimated        bool       `json:"estimated,omitempty"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	AudioSeconds     float64    `json:"audio_seconds,omitempty"`
	ProcessedSeconds float64    `json:"processed_seconds,omitempty"`
	// ETASeconds is the estimated time left; 0 when unknown or done.
	ETASeconds float64 `json:"eta_seconds,omitempty"`
	// QueuePosition is how many jobs run before this one (0 when running).
	QueuePosition int `json:"queue_position,omitempty"`
}

// TranscriptRecord is one item's machine transcript and run state, stored at
// connector-state/transcripts/<source>/<id>.json on this spoke only. It never
// holds the audio URL or the provider key.
type TranscriptRecord struct {
	Source   string `json:"source"`
	ID       string `json:"id"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Attempts int    `json:"attempts"`
	// AudioSize / AudioETag identify the audio the transcript was made from
	// (from the download's Content-Length and ETag), so a replaced recording
	// is recognisable.
	AudioSize    int64     `json:"audio_size,omitempty"`
	AudioETag    string    `json:"audio_etag,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model,omitempty"`
	Language     string    `json:"language,omitempty"`
	AudioSeconds float64   `json:"audio_seconds,omitempty"`
	Text         string    `json:"text,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
	// StartedAt is when the current or last run started.
	StartedAt time.Time `json:"started_at,omitzero"`
}

// State returns the item-facing view of the record.
func (r TranscriptRecord) State() TranscriptionState {
	st := TranscriptionState{Status: r.Status, Reason: r.Reason, Attempts: r.Attempts, Provider: r.Provider, Model: r.Model, UpdatedAt: r.UpdatedAt}
	if !r.StartedAt.IsZero() && r.Status == TranscriptionRunning {
		started := r.StartedAt
		st.StartedAt = &started
	}
	return st
}

// Label is the heading machine text is shown under.
func (r TranscriptRecord) Label() string { return MachineTranscriptLabel(r.Provider, r.Model) }

// MachineTranscriptLabel is "Transcript (machine: deepgram/nova-3)".
func MachineTranscriptLabel(provider, model string) string {
	return "Transcript (machine: " + provider + "/" + model + ")"
}

// TranscriptUsage is the spend this spoke has recorded.
type TranscriptUsage struct {
	// MonthSeconds is the audio transcribed this UTC month.
	MonthSeconds float64
	// TodayItems is how many runs started this UTC day.
	TodayItems int
}

type usageLedger struct {
	Months map[string]float64 `json:"months"`
	Days   map[string]int     `json:"days"`
	// Speeds is this hive's measured speed per "provider/model": an
	// exponential moving average of the real-time factor of finished runs.
	Speeds map[string]SpeedSample `json:"speeds,omitempty"`
}

// SpeedSample is the measured real-time factor of one provider/model on
// this hive (wall-clock seconds per audio second) and how many runs fed it.
type SpeedSample struct {
	RTF  float64 `json:"rtf"`
	Runs int     `json:"runs"`
}

const (
	// speedEMAAlpha weighs the newest run in the measured speed.
	speedEMAAlpha = 0.3
	// minSpeedRTF / maxSpeedRTF clamp one run's sample so a clock jump or a
	// near-empty recording cannot poison the average.
	minSpeedRTF = 0.001
	maxSpeedRTF = 20
)

// Speed returns the measured speed for provider/model, if any run finished.
func (s *TranscriptStore) Speed(provider, model string) (SpeedSample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.ledger().Speeds[provider+"/"+model]
	return v, ok && v.RTF > 0
}

// RecordSpeed folds one finished run (wall seconds for audio seconds) into
// the measured speed of provider/model.
func (s *TranscriptStore) RecordSpeed(provider, model string, wall, audio float64) error {
	if wall <= 0 || audio <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.ledger()
	if l.Speeds == nil {
		l.Speeds = map[string]SpeedSample{}
	}
	k := provider + "/" + model
	l.Speeds[k] = foldSpeed(l.Speeds[k], wall/audio)
	return s.writeLedger(l)
}

// foldSpeed applies one clamped sample to the moving average.
func foldSpeed(prev SpeedSample, rtf float64) SpeedSample {
	rtf = min(max(rtf, minSpeedRTF), maxSpeedRTF)
	if prev.Runs == 0 || prev.RTF <= 0 {
		return SpeedSample{RTF: rtf, Runs: 1}
	}
	return SpeedSample{RTF: speedEMAAlpha*rtf + (1-speedEMAAlpha)*prev.RTF, Runs: prev.Runs + 1}
}

// TranscriptStore keeps machine transcripts and the usage ledger under Dir
// (connector-state/transcripts on the spoke's own data volume). Files are
// written 0600 through a temp file and rename.
type TranscriptStore struct {
	Dir string
	mu  sync.Mutex
}

// NewTranscriptStore returns a store rooted at dir.
func NewTranscriptStore(dir string) *TranscriptStore { return &TranscriptStore{Dir: dir} }

var unsafeNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// safeName maps an upstream id to one path component. Anything rewritten
// gets a digest suffix so two ids never collide.
func safeName(s string) string {
	out := unsafeNameRe.ReplaceAllString(s, "_")
	if out == s && out != "" && out[0] != '.' {
		return out
	}
	sum := sha256.Sum256([]byte(s))
	return "_" + out + "-" + hex.EncodeToString(sum[:])[:safeNameHashLen]
}

func (s *TranscriptStore) path(source, id string) string {
	return filepath.Join(s.Dir, safeName(source), safeName(id)+".json")
}

// Get reads one record.
func (s *TranscriptStore) Get(source, id string) (TranscriptRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rec TranscriptRecord
	data, err := os.ReadFile(s.path(source, id))
	if err != nil || json.Unmarshal(data, &rec) != nil {
		return TranscriptRecord{}, false
	}
	return rec, true
}

// Put writes one record atomically.
func (s *TranscriptStore) Put(rec TranscriptRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path(rec.Source, rec.ID), data)
}

// List reads every record (one directory per source). Files that are not
// records (the usage ledger, temp files of an interrupted write) and records
// that do not parse are skipped; a missing store is empty.
func (s *TranscriptStore) List() ([]TranscriptRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirs, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []TranscriptRecord
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.Dir, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(s.Dir, d.Name(), f.Name()))
			var rec TranscriptRecord
			if err != nil || json.Unmarshal(data, &rec) != nil || rec.Source == "" || rec.ID == "" {
				continue
			}
			out = append(out, rec)
		}
	}
	return out, nil
}

// Delete removes one record; a missing record is not an error.
func (s *TranscriptStore) Delete(source, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(source, id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Usage returns what was spent this month and today (UTC).
func (s *TranscriptStore) Usage(now time.Time) TranscriptUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.ledger()
	now = now.UTC()
	return TranscriptUsage{MonthSeconds: l.Months[now.Format(usageMonthLayout)], TodayItems: l.Days[now.Format(usageDayLayout)]}
}

// AddUsage records seconds of transcribed audio and runs started, and
// drops ledger entries older than the retention window.
func (s *TranscriptStore) AddUsage(now time.Time, seconds float64, items int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.ledger()
	now = now.UTC()
	l.Months[now.Format(usageMonthLayout)] += seconds
	l.Days[now.Format(usageDayLayout)] += items
	pruneOldest(l.Days, usageKeepDays)
	pruneOldest(l.Months, usageKeepMonths)
	return s.writeLedger(l)
}

// writeLedger persists l; the caller holds s.mu.
func (s *TranscriptStore) writeLedger(l usageLedger) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.Dir, transcriptUsageFile), data)
}

func (s *TranscriptStore) ledger() usageLedger {
	var l usageLedger
	if data, err := os.ReadFile(filepath.Join(s.Dir, transcriptUsageFile)); err == nil {
		_ = json.Unmarshal(data, &l)
	}
	if l.Months == nil {
		l.Months = map[string]float64{}
	}
	if l.Days == nil {
		l.Days = map[string]int{}
	}
	return l
}

// pruneOldest keeps the keep lexically newest keys (ISO dates and months
// sort by time).
func pruneOldest[V any](m map[string]V, keep int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i := 0; i < len(keys)-keep; i++ {
		delete(m, keys[i])
	}
}

// writeFileAtomic writes data to path with mode 0600 via a temp file in the
// same directory and a rename, creating the directory 0700.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, transcriptDirMode); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, transcriptFileMode)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

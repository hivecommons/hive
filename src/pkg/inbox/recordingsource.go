package inbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

const (
	// RecordingLayer is the Knowledge layer recording facts are written to:
	// personal, so a recording is never published to the hub.
	RecordingLayer = "personal"
	// recordingAudioPathPrefix is the same-origin audio endpoint.
	recordingAudioPathPrefix = "/api/inbox/recordings/"
	// recordingAudioLinkTTL is how long a same-origin audio link is quoted
	// as valid (it needs the dashboard session either way).
	recordingAudioLinkTTL = time.Hour
	// RecordingSweepActor is the audit actor of sweep-made changes.
	RecordingSweepActor = "inbox-recording-sweep"
)

// LocalAudio is an audio file on this hive's own disk.
type LocalAudio struct {
	File    *os.File
	Name    string
	MIME    string
	Size    int64
	ModTime time.Time
}

// AudioOpener is optionally implemented by sources whose audio lives on this
// hive: playback serves the file directly, and nothing makes an HTTP call to
// the hive itself.
type AudioOpener interface {
	OpenAudio(ctx context.Context, it Item) (LocalAudio, error)
}

// RecordingSource lists the recordings made in this hive's dashboard. It is
// a builtin source (listed whatever inbox.sources says). Each finalized
// recording has a Knowledge fact (personal layer) that starts pending and
// receives the machine transcript; the raw audio is deleted after
// inbox.recording.retention_days while the fact and transcript stay.
type RecordingSource struct {
	Store *RecordingStore
	// FactDir is <knowledge base>/connectors/<RecordingLayer>.
	FactDir string
	// RetentionDays reports inbox.recording.retention_days for listings.
	RetentionDays func() int
}

// Type implements Source.
func (r *RecordingSource) Type() string { return RecordingType }

// Builtin implements BuiltinSource.
func (r *RecordingSource) Builtin() bool { return true }

// factPath is where recording id's fact lives.
func (r *RecordingSource) factPath(id string) string {
	return filepath.Join(r.FactDir, connector.Slug(RecordingType, RecordingConnector, id)+".md")
}

// List implements Source: finalized recordings, newest first.
func (r *RecordingSource) List(ctx context.Context) ([]Item, error) {
	days := config.DefaultInboxRecordingRetentionDays
	if r.RetentionDays != nil {
		days = r.RetentionDays()
	}
	var out []Item
	for _, m := range r.Store.List() {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if m.State != RecordingFinalized {
			continue
		}
		it := Item{
			Source: RecordingType, Connector: RecordingConnector, ID: m.ID, Title: m.Title,
			RecordedAt: m.CreatedAt, DurationMS: m.DurationMS, Tags: []string{"recording"},
			RecordedBy: m.RecordedBy, AudioExpired: m.AudioExpired(), AudioRetentionDays: days,
			Pending: true, FactState: connector.FactContentPending,
		}
		if m.IncludesTabAudio {
			it.Tags = append(it.Tags, "tab-audio")
		}
		path := r.factPath(m.ID)
		if data, err := os.ReadFile(path); err == nil {
			fm, body := splitFrontMatter(string(data))
			fact := itemFromFact(fm, body, RecordingLayer, path)
			it.FactSlug, it.FactPath, it.Layer, it.FactState = fact.FactSlug, fact.FactPath, fact.Layer, fact.FactState
			it.Summary, it.HasTranscript, it.Pending, it.Excerpt = fact.Summary, fact.HasTranscript, fact.Pending, fact.Excerpt
		}
		out = append(out, it)
	}
	sortItems(out)
	return out, nil
}

// Fetch implements Source from the recording's fact.
func (r *RecordingSource) Fetch(ctx context.Context, it Item) (Content, error) {
	provenance := RecordingType + "://recordings/" + it.ID
	if it.FactPath == "" {
		return Content{Title: it.Title, Pending: true, Provenance: provenance}, nil
	}
	c, err := (&FactSource{SourceType: RecordingType}).Fetch(ctx, it)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return Content{Title: it.Title, Pending: true, Provenance: provenance}, nil
	}
	return c, err
}

// AudioLink implements AudioSource with the hive's own same-origin audio
// endpoint; there is no third-party storage behind a recording.
func (r *RecordingSource) AudioLink(_ context.Context, it Item) (AudioLink, error) {
	m, ok := r.Store.Get(it.ID)
	if !ok || m.State != RecordingFinalized {
		return AudioLink{}, ErrNoAudio
	}
	if m.AudioExpired() {
		return AudioLink{}, fmt.Errorf("%w: %v", ErrNoAudio, ErrRecordingExpired)
	}
	return AudioLink{URL: recordingAudioPathPrefix + m.ID + "/audio", ExpiresAt: time.Now().Add(recordingAudioLinkTTL).UTC()}, nil
}

// OpenAudio implements AudioOpener.
func (r *RecordingSource) OpenAudio(_ context.Context, it Item) (LocalAudio, error) {
	f, m, err := r.Store.OpenAudio(it.ID)
	if err != nil {
		return LocalAudio{}, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return LocalAudio{}, err
	}
	return LocalAudio{File: f, Name: m.AudioFile, MIME: recordingAudioMIME(m.Container), Size: st.Size(), ModTime: st.ModTime()}, nil
}

// recordingAudioMIME is the Content-Type a recording is served with.
func recordingAudioMIME(container string) string {
	if container == RecordingContainerMP4 {
		return "audio/mp4"
	}
	return "audio/webm"
}

// writeFact writes the pending Knowledge fact of a finalized recording. A
// fact that already holds a machine transcript is left alone.
func (r *RecordingSource) writeFact(m RecordingMeta) error {
	if r.FactDir == "" {
		return nil
	}
	if err := os.MkdirAll(r.FactDir, recordingDirMode); err != nil {
		return err
	}
	dur := time.Duration(m.DurationMS) * time.Millisecond
	var md strings.Builder
	md.WriteString("# " + m.Title + "\n\n")
	md.WriteString("- **Recorded:** " + m.CreatedAt.UTC().Format(time.RFC3339) + "\n")
	md.WriteString("- **Recorded by:** " + m.RecordedBy + "\n")
	md.WriteString("- **Duration:** " + dur.Round(time.Second).String() + "\n")
	if m.IncludesTabAudio {
		md.WriteString("- **Audio:** microphone and a browser tab\n")
	} else {
		md.WriteString("- **Audio:** microphone\n")
	}
	md.WriteString("\n_" + RecordingPendingText + "_\n")
	w := &connector.FactWriter{Dir: r.FactDir}
	_, _, err := w.Write(connector.ConnectorConfig{Name: RecordingConnector, Type: RecordingType, Layer: RecordingLayer}, connector.Page{
		ID:    m.ID,
		Title: m.Title,
		Attrs: map[string]string{
			"hive_recording_content": connector.FactContentPending,
			"recorded_at":            m.CreatedAt.UTC().Format(time.RFC3339),
			"duration_ms":            strconv.FormatInt(m.DurationMS, 10),
			"recorded_by":            m.RecordedBy,
			"consent_version":        m.ConsentVersion,
			"includes_tab_audio":     strconv.FormatBool(m.IncludesTabAudio),
			"tags":                   "recording",
		},
		Markdown:  md.String(),
		UpdatedAt: m.CreatedAt,
	})
	return err
}

// RecordingSettings is the dashboard's view of inbox.recording.
type RecordingSettings struct {
	Enabled        bool   `json:"enabled"`
	ConsentVersion string `json:"consent_version"`
	RetentionDays  int    `json:"retention_days"`
	MaxSeconds     int    `json:"max_seconds"`
	MaxBytes       int64  `json:"max_bytes"`
	MaxChunkBytes  int    `json:"max_chunk_bytes"`
	QuotaBytes     int64  `json:"quota_bytes"`
	UsedBytes      int64  `json:"used_bytes"`
	ChunkMillis    int    `json:"chunk_ms"`
	BitsPerSecond  int    `json:"audio_bits_per_second"`
	// Transcribes reports that a finished recording is transcribed
	// automatically right now (transcription enabled with consent).
	Transcribes bool `json:"transcribes"`
}

// RecordingAudit is one recording audit entry.
type RecordingAudit struct {
	Actor  string
	Action string
	Fields []string
}

// Detail renders the entry as "key=value, key=value".
func (a RecordingAudit) Detail() string {
	parts := make([]string, 0, len(a.Fields)/2)
	for i := 0; i+1 < len(a.Fields); i += 2 {
		parts = append(parts, a.Fields[i]+"="+a.Fields[i+1])
	}
	return strings.Join(parts, ", ")
}

// RecordingOptions carries who acts and where audit entries go.
type RecordingOptions struct {
	Actor string
	Audit func(RecordingAudit)
}

func (o RecordingOptions) audit(action string, fields ...string) {
	if o.Audit != nil {
		o.Audit(RecordingAudit{Actor: o.Actor, Action: action, Fields: fields})
	}
}

// EnableRecordings wires dashboard recordings; the source is registered as
// a builtin and survives SetSources.
func (s *Service) EnableRecordings(src *RecordingSource) {
	s.mu.Lock()
	s.recordings = src
	if src != nil {
		s.sources[src.Type()] = src
	}
	s.mu.Unlock()
}

// recordingSource returns the wired recording source, or nil.
func (s *Service) recordingSource() *RecordingSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.recordings
}

// RecordingSettings returns inbox.recording with defaults applied.
func (s *Service) RecordingSettings() RecordingSettings {
	c := s.Config()
	r := c.Recording
	out := RecordingSettings{
		Enabled: r.EffectiveEnabled() && s.recordingSource() != nil, ConsentVersion: RecordingConsentVersion,
		RetentionDays: r.EffectiveRetentionDays(), MaxSeconds: r.EffectiveMaxSeconds(), MaxBytes: r.EffectiveMaxBytes(),
		MaxChunkBytes: RecordingMaxChunkBytes, QuotaBytes: r.EffectiveQuotaBytes(), ChunkMillis: RecordingChunkMillis,
		BitsPerSecond: RecordingAudioBitsPerSecond, Transcribes: c.Transcription.Active(),
	}
	if src := s.recordingSource(); src != nil {
		out.UsedBytes = src.Store.UsedBytes()
	}
	return out
}

// recordingStore returns the store when recording is enabled.
func (s *Service) recordingStore() (*RecordingSource, config.InboxRecording, error) {
	src := s.recordingSource()
	cfg := s.Config().Recording
	if src == nil || !cfg.EffectiveEnabled() {
		return nil, cfg, ErrRecordingDisabled
	}
	return src, cfg, nil
}

// StartRecording creates a recording for req.User.
func (s *Service) StartRecording(req RecordingStart, opts RecordingOptions) (RecordingMeta, error) {
	src, cfg, err := s.recordingStore()
	if err != nil {
		return RecordingMeta{}, err
	}
	m, err := src.Store.Create(req, cfg)
	if err != nil {
		return RecordingMeta{}, err
	}
	opts.audit("inbox_recording_start", "recording", m.ID, "consent_version", m.ConsentVersion, "includes_tab_audio", strconv.FormatBool(m.IncludesTabAudio), "mime", m.MIME)
	return m, nil
}

// AppendRecordingChunk stores chunk seq of one of user's recordings.
func (s *Service) AppendRecordingChunk(id, user string, seq int, data []byte) (RecordingChunk, error) {
	src, cfg, err := s.recordingStore()
	if err != nil {
		return RecordingChunk{}, err
	}
	return src.Store.Append(id, user, seq, data, cfg)
}

// FinalizeRecording ends one of user's recordings and writes its pending
// Knowledge fact.
func (s *Service) FinalizeRecording(id, user string, durationMS int64, opts RecordingOptions) (RecordingMeta, error) {
	src, cfg, err := s.recordingStore()
	if err != nil {
		return RecordingMeta{}, err
	}
	m, err := src.Store.Finalize(id, user, durationMS, cfg)
	if err != nil {
		return RecordingMeta{}, err
	}
	s.recordingFinalized(src, m, opts)
	return m, nil
}

// recordingFinalized writes the fact and audits a finished recording.
func (s *Service) recordingFinalized(src *RecordingSource, m RecordingMeta, opts RecordingOptions) {
	if err := src.writeFact(m); err != nil {
		s.Logger.Warn("inbox: write recording fact", "recording", m.ID, "error", err)
	}
	if m.ConcatWarning != "" {
		s.Logger.Warn("inbox: recording container check", "recording", m.ID, "warning", m.ConcatWarning)
	}
	opts.audit("inbox_recording_finalize", "recording", m.ID, "chunks", strconv.Itoa(m.Chunks), "bytes", strconv.FormatInt(m.Bytes, 10),
		"duration_ms", strconv.FormatInt(m.DurationMS, 10), "auto", strconv.FormatBool(m.AutoFinalized))
}

// AbortRecording discards an in-progress recording.
func (s *Service) AbortRecording(id, user string, asOwner bool, opts RecordingOptions) (RecordingMeta, error) {
	src := s.recordingSource()
	if src == nil {
		return RecordingMeta{}, ErrRecordingDisabled
	}
	m, err := src.Store.Abort(id, user, asOwner)
	if err != nil {
		return RecordingMeta{}, err
	}
	opts.audit("inbox_recording_abort", "recording", m.ID, "recorded_by", m.RecordedBy, "chunks", strconv.Itoa(m.Chunks))
	return m, nil
}

// OpenRecordingAudio opens a finalized recording's audio (playback works
// while recording is disabled: the recordings stay until retention).
func (s *Service) OpenRecordingAudio(id string) (LocalAudio, error) {
	src := s.recordingSource()
	if src == nil {
		return LocalAudio{}, ErrRecordingNotFound
	}
	return src.OpenAudio(context.Background(), Item{Source: RecordingType, ID: id})
}

// OpenLocalAudio opens an item's audio when its source keeps it on this
// hive; ok is false for sources whose audio lives elsewhere.
func (s *Service) OpenLocalAudio(ctx context.Context, it Item) (LocalAudio, bool, error) {
	op, ok := s.source(it.Source).(AudioOpener)
	if !ok {
		return LocalAudio{}, false, nil
	}
	a, err := op.OpenAudio(ctx, it)
	return a, true, err
}

// RecordingSweep finalizes idle recordings and deletes audio past
// retention (it runs periodically from boot). Every change is logged and
// audited with RecordingSweepActor.
func (s *Service) RecordingSweep(opts RecordingOptions) (RecordingSweepResult, error) {
	src := s.recordingSource()
	if src == nil {
		return RecordingSweepResult{}, nil
	}
	if opts.Actor == "" {
		opts.Actor = RecordingSweepActor
	}
	cfg := s.Config().Recording
	res, err := src.Store.Sweep(cfg)
	for _, m := range res.Finalized {
		s.Logger.Info("inbox: recording auto-finalized after idle", "recording", m.ID, "bytes", m.Bytes)
		s.recordingFinalized(src, m, opts)
	}
	for _, m := range res.Removed {
		s.Logger.Info("inbox: empty idle recording removed", "recording", m.ID)
		opts.audit("inbox_recording_abort", "recording", m.ID, "recorded_by", m.RecordedBy, "chunks", "0", "reason", "idle")
	}
	for _, m := range res.Expired {
		s.Logger.Info("inbox: recording audio deleted (retention)", "recording", m.ID, "retention_days", cfg.EffectiveRetentionDays(), "bytes", m.Bytes)
		opts.audit("inbox_recording_audio_expired", "recording", m.ID, "retention_days", strconv.Itoa(cfg.EffectiveRetentionDays()), "bytes", strconv.FormatInt(m.Bytes, 10))
	}
	return res, err
}

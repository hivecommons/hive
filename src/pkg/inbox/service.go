package inbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

// routeTimeout bounds one background delivery.
const routeTimeout = 15 * time.Minute

// Settings is the operator-editable part of the Inbox (mirrors inbox.*
// YAML minus binary/agent plumbing).
type Settings struct {
	Sources  []string           `json:"sources"`
	Rules    []config.InboxRule `json:"rules"`
	SpekRepo string             `json:"spek_repo"`
	DibsURL  string             `json:"dibs_url"`
	// DibsTokenSet reports whether inbox.dibs.token_env resolves; the token
	// itself is never returned.
	DibsTokenSet bool `json:"dibs_token_set"`
	SpekReady    bool `json:"spek_ready"`
	// Transcription is inbox.transcription with defaults, key and budget
	// status applied.
	Transcription TranscriptionSettings `json:"transcription"`
	// Uploads are the drop-zone limits; Enabled is false when this hive has
	// no upload store.
	Uploads UploadSettings `json:"uploads"`
	// Recording is inbox.recording with defaults applied.
	Recording RecordingSettings `json:"recording"`
}

// Service lists items across sources, applies persisted state, runs
// auto-routing rules and executes manual routes in the background.
type Service struct {
	Store  *Store
	Logger *slog.Logger
	// Now is injectable for tests.
	Now func() time.Time
	// RFCPublishWait bounds how long a delegated RFC share may stay in
	// progress; zero means DefaultRFCPublishWait.
	RFCPublishWait time.Duration

	mu      sync.RWMutex
	cfg     config.InboxConfig
	sources map[string]Source
	dests   map[string]Destination
	wg      sync.WaitGroup
	// Run executes a delivery; nil runs it in a goroutine. Tests set it to
	// run inline.
	Run func(fn func())
	// Status snapshots for the settings view.
	spekReady    func() bool
	dibsTokenSet func() bool

	// Machine transcription (see transcription.go). transcripts is nil until
	// EnableTranscription; txMu guards the queue check-and-set. txQueue
	// holds queued and running jobs in order (the first
	// transcribeConcurrency run) with their live progress, guarded by txQMu.
	transcripts *TranscriptStore
	// intentRunners are the registered transcribe-then-share handlers.
	intentRunners map[string]IntentRunner
	// rfcFinder completes delegated RFC shares; see CheckRFCPublications.
	rfcFinder RFCFinder
	txMu      sync.Mutex
	txQMu     sync.Mutex
	txQueue   []*txJob
	// TranscribeConcurrency is how many transcriptions run at once; zero
	// reads HIVE_INBOX_TRANSCRIBE_CONCURRENCY (default
	// DefaultTranscribeConcurrency). Fixed on first use.
	TranscribeConcurrency int
	txConcOnce            sync.Once
	txConc                int
	// NewTranscriber builds the provider for a run; nil uses
	// DefaultTranscriber with the key from Getenv. Tests inject a fake.
	NewTranscriber func(config.InboxTranscription) (transcribe.Transcriber, error)
	// AudioClient downloads audio for transcription; nil uses
	// http.DefaultClient (the run's context bounds it).
	AudioClient *http.Client
	// Getenv reads the provider key variable; nil uses os.Getenv.
	Getenv func(string) string
	// TranscribeBackoff is the wait between upload attempts; nil doubles
	// from five seconds.
	TranscribeBackoff func(attempt int) time.Duration

	// Same-origin playback (see audiostream.go). AudioStreamMaxBytes caps
	// one proxied response; 0 uses DefaultAudioStreamMaxBytes. streamLinks
	// keeps minted links in memory only, so seeking does not re-mint.
	AudioStreamMaxBytes int64
	streamLinks         audioLinkCache
	// Whisper locates the built-in whisper engine; nil reads the
	// HIVE_WHISPER_* environment on first use. Tests inject a fake sandbox.
	Whisper   *WhisperRuntime
	whisperMu sync.Mutex
	// factMu serialises machine-transcript write-backs into Knowledge facts
	// (see factwriteback.go).
	factMu sync.Mutex
	// uploads holds files and links added on the dashboard (see
	// upload_source.go); nil until EnableUploads.
	uploads *UploadSource
	// recordings is the dashboard recording source (see recordingsource.go);
	// nil until EnableRecordings.
	recordings *RecordingSource
	// email holds files mailed to hive@<host> (email_source.go); nil until
	// EnableEmail.
	email *EmailSource
}

// NewService builds a service for cfg with the given sources/destinations.
func NewService(cfg config.InboxConfig, store *Store, sources []Source, dests []Destination, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{Store: store, Logger: logger, cfg: cfg, sources: map[string]Source{}, dests: map[string]Destination{}}
	for _, src := range sources {
		s.sources[src.Type()] = src
	}
	for _, d := range dests {
		s.dests[d.Name()] = d
	}
	return s
}

// SetSources replaces the sources, keyed by type (#18091): a voice connector
// added through the dashboard becomes an inbox source without a restart.
// Items and Sweep pick the new set up on their next call.
func (s *Service) SetSources(sources []Source) {
	next := make(map[string]Source, len(sources))
	for _, src := range sources {
		next[src.Type()] = src
	}
	s.mu.Lock()
	if s.uploads != nil {
		next[UploadType] = s.uploads
	}
	if s.recordings != nil {
		if _, ok := next[RecordingType]; !ok {
			next[RecordingType] = s.recordings
		}
	}
	if s.email != nil {
		next[EmailType] = s.email
	}
	s.sources = next
	s.mu.Unlock()
}

// EnableUploads registers the upload source; it stays registered across
// SetSources.
func (s *Service) EnableUploads(u *UploadSource) {
	if u != nil && u.Limits == nil {
		u.Limits = func() config.InboxUploads { return s.Config().Uploads }
	}
	if u != nil && u.RecordingLimits == nil {
		u.RecordingLimits = func() config.InboxRecording { return s.Config().Recording }
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads = u
	if u != nil {
		s.sources[UploadType] = u
	}
}

// EnableEmail registers the email spool source (#18395).
func (s *Service) EnableEmail(e *EmailSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.email = e
	if e != nil {
		s.sources[EmailType] = e
	}
}

// Email returns the email source, or nil when it is not enabled.
func (s *Service) Email() *EmailSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.email
}

// Uploads returns the upload source, or nil when uploads are not enabled.
func (s *Service) Uploads() *UploadSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.uploads
}

// AddUpload stores one dropped file and returns its Inbox item.
func (s *Service) AddUpload(ctx context.Context, in UploadInput) (Item, error) {
	u := s.Uploads()
	if u == nil {
		return Item{}, ErrUploadsDisabled
	}
	m, err := u.Add(ctx, in)
	if err != nil {
		return Item{}, err
	}
	return u.item(m), nil
}

// AddMedia stores one dropped audio or video file and, when transcription
// is enabled and consented, queues its machine transcription at once
// through AutoTranscribe (opts audits the run like a manual Transcribe).
func (s *Service) AddMedia(ctx context.Context, in MediaInput, opts TranscribeOptions) (Item, error) {
	u := s.Uploads()
	if u == nil {
		return Item{}, ErrUploadsDisabled
	}
	m, err := u.AddMedia(ctx, in)
	if err != nil {
		return Item{}, err
	}
	it := u.item(m)
	s.AutoTranscribe(ctx, it, opts)
	return it, nil
}

// AddLink fetches one URL and returns its Inbox item.
func (s *Service) AddLink(ctx context.Context, in LinkInput) (Item, error) {
	u := s.Uploads()
	if u == nil {
		return Item{}, ErrUploadsDisabled
	}
	m, err := u.AddLink(ctx, in)
	if err != nil {
		return Item{}, err
	}
	return u.item(m), nil
}

// ErrUploadsDisabled is returned when the hive has no upload store.
var ErrUploadsDisabled = errors.New("uploads are not available on this hive")

// HasSource reports whether a source of typ is wired.
func (s *Service) HasSource(typ string) bool {
	return s.source(typ) != nil
}

// SetReadiness installs the probes behind Settings.SpekReady / DibsTokenSet.
func (s *Service) SetReadiness(spek, dibs func() bool) {
	s.spekReady, s.dibsTokenSet = spek, dibs
}

// Config returns the current inbox config.
func (s *Service) Config() config.InboxConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// UpdateConfig replaces the routing config (after the dashboard saved it).
func (s *Service) UpdateConfig(cfg config.InboxConfig) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// Settings returns the operator view of the config.
func (s *Service) Settings() Settings {
	cfg := s.Config()
	st := Settings{
		Sources:  cfg.EffectiveSources(),
		Rules:    cfg.Rules,
		SpekRepo: cfg.Spek.Repo,
		DibsURL:  cfg.Dibs.URL,
	}
	if st.Rules == nil {
		st.Rules = []config.InboxRule{}
	}
	if s.spekReady != nil {
		st.SpekReady = s.spekReady()
	}
	if s.dibsTokenSet != nil {
		st.DibsTokenSet = s.dibsTokenSet()
	}
	st.Transcription = s.TranscriptionSettings()
	if u := s.Uploads(); u != nil {
		st.Uploads = u.Settings()
	}
	st.Recording = s.RecordingSettings()
	return st
}

// Items lists every item across the configured sources with state applied,
// newest first.
func (s *Service) Items(ctx context.Context) ([]Item, error) {
	cfg := s.Config()
	var out []Item
	var errs []error
	seen := map[string]bool{}
	types := append(append([]string(nil), cfg.EffectiveSources()...), s.builtinTypes()...)
	for _, typ := range types {
		src := s.source(typ)
		if src == nil || seen[typ] {
			continue
		}
		seen[typ] = true
		items, err := src.List(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", typ, err))
		}
		out = append(out, items...)
	}
	out = s.Store.Live(out)
	s.Store.Apply(out)
	s.applyTranscripts(out)
	sortItems(out)
	return out, errors.Join(errs...)
}

// BuiltinSource marks a source hive provides itself (the meetings it
// attended): it is listed whether or not inbox.sources names it.
type BuiltinSource interface {
	Builtin() bool
}

// builtinTypes returns the registered builtin source types, sorted.
func (s *Service) builtinTypes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for typ, src := range s.sources {
		if b, ok := src.(BuiltinSource); ok && b.Builtin() {
			out = append(out, typ)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Service) source(typ string) Source {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sources[typ]
}

// Find returns one item by key.
func (s *Service) Find(ctx context.Context, source, id string) (Item, bool, error) {
	items, err := s.Items(ctx)
	for _, it := range items {
		if it.Source == source && it.ID == id {
			return it, true, err
		}
	}
	return Item{}, false, err
}

// Content reads the full body of one item from its source. While the
// source has no text, a done machine transcript stands in for it.
func (s *Service) Content(ctx context.Context, it Item) (Content, error) {
	src := s.source(it.Source)
	if src == nil {
		return Content{}, fmt.Errorf("source %q is not configured", it.Source)
	}
	c, err := src.Fetch(ctx, it)
	return s.withMachineTranscript(it, c, err)
}

// AudioLink mints a fresh audio link for one item through its source. The
// link is handed back and forgotten: the service never stores it.
func (s *Service) AudioLink(ctx context.Context, it Item) (AudioLink, error) {
	src := s.source(it.Source)
	if src == nil {
		return AudioLink{}, fmt.Errorf("source %q is not configured", it.Source)
	}
	audio, ok := src.(AudioSource)
	if !ok {
		return AudioLink{}, ErrNoAudio
	}
	return audio.AudioLink(ctx, it)
}

// Unprocessed counts items with no filed route and not dismissed.
func Unprocessed(items []Item) int {
	n := 0
	for _, it := range items {
		if !it.Processed {
			n++
		}
	}
	return n
}

// Dismiss marks an item knowledge-only.
func (s *Service) Dismiss(source, id string, dismissed bool) error {
	return s.Store.Dismiss(Key(source, id), dismissed)
}

// DismissBy marks an item knowledge-only on behalf of actor and records the
// share in its history. Clearing the mark records nothing.
func (s *Service) DismissBy(actor, source, id string, dismissed bool) error {
	if err := s.Store.Dismiss(Key(source, id), dismissed); err != nil || !dismissed {
		return err
	}
	_, err := s.RecordShare(Key(source, id), ShareRecord{Destination: ShareDestKnowledge, Actor: actor, Status: ShareDone, FinishedAt: ptrTime(s.now())})
	return err
}

func ptrTime(t time.Time) *time.Time { return &t }

// ErrUnknownDestination is returned for a destination the service lacks.
var ErrUnknownDestination = errors.New("unknown destination")

// Route queues it for dest and runs the delivery in the background. The
// returned state is the queued state; callers poll Items for progress.
func (s *Service) Route(ctx context.Context, it Item, req Request) (RouteState, error) {
	if it.AwaitingApproval && req.Destination != DestDismiss {
		return RouteState{}, ErrAwaitingApproval
	}
	if req.Destination == DestDismiss {
		if req.ShareID != "" {
			ref := refFromResult(it, DestDismiss, Result{})
			if err := s.Store.Dismiss(it.Key(), true); err != nil {
				return RouteState{}, err
			}
			_ = s.FinishShare(it.Key(), req.ShareID, ShareDone, "", ref)
		} else if err := s.DismissBy(req.Actor, it.Source, it.ID, true); err != nil {
			return RouteState{}, err
		}
		s.handOff(ctx, it, DestDismiss)
		return RouteState{Status: StatusFiled, UpdatedAt: s.now()}, nil
	}
	s.mu.RLock()
	dest := s.dests[req.Destination]
	s.mu.RUnlock()
	if dest == nil {
		return RouteState{}, fmt.Errorf("%w: %q", ErrUnknownDestination, req.Destination)
	}
	if req.Destination == DestSpekRFC && strings.TrimSpace(req.Repo) == "" {
		req.Repo = s.Config().Spek.Repo
	}
	st, err := s.Store.Begin(it.Key(), req.Destination, req.Repo)
	if err != nil {
		return st, err
	}
	if req.ShareID == "" {
		id, rerr := s.RecordShare(it.Key(), ShareRecord{Destination: shareDestFor(req.Destination), Actor: req.Actor, Status: ShareQueued})
		if rerr != nil {
			s.Logger.Warn("inbox: record share", "item", it.Key(), "error", rerr)
		}
		req.ShareID = id
	} else {
		_ = s.FinishShare(it.Key(), req.ShareID, ShareQueued, "", nil)
	}
	s.wg.Add(1)
	run := s.Run
	if run == nil {
		run = func(fn func()) { go fn() }
	}
	run(func() {
		defer s.wg.Done()
		s.deliver(it, dest, req)
	})
	return st, nil
}

func (s *Service) deliver(it Item, dest Destination, req Request) {
	key := it.Key()
	ctx, cancel := context.WithTimeout(context.Background(), routeTimeout)
	defer cancel()
	if _, err := s.Store.Set(key, req.Destination, StatusRunning, Result{}, ""); err != nil {
		s.Logger.Warn("inbox: persist running state", "item", key, "error", err)
	}
	if req.ShareID != "" {
		_ = s.FinishShare(key, req.ShareID, ShareInProgress, "", nil)
	}
	content, err := s.Content(ctx, it)
	if err != nil {
		s.fail(key, req.Destination, fmt.Errorf("fetch: %w", err), req.ShareID)
		return
	}
	res, err := dest.Deliver(ctx, it, content, req)
	if err != nil {
		s.fail(key, req.Destination, err, req.ShareID)
		return
	}
	status := StatusFiled
	if res.URL == "" && res.Note != "" {
		status = StatusDelegated
	}
	if _, err := s.Store.Set(key, req.Destination, status, res, ""); err != nil {
		s.Logger.Warn("inbox: persist filed state", "item", key, "error", err)
	}
	if req.ShareID != "" {
		if status == StatusDelegated {
			// Handed to an agent: still in progress until the RFC exists.
			_ = s.FinishShare(key, req.ShareID, ShareInProgress, res.Note, nil)
		} else {
			_ = s.FinishShare(key, req.ShareID, ShareDone, "", refFromResult(it, req.Destination, res))
		}
	}
	s.Logger.Info("inbox: routed", "item", key, "destination", req.Destination, "status", status, "url", res.URL)
	s.handOff(ctx, it, req.Destination)
}

// handOff tells a HandOffSource that it left the inbox. A failure is logged:
// the route itself succeeded.
func (s *Service) handOff(ctx context.Context, it Item, dest string) {
	hs, ok := s.source(it.Source).(HandOffSource)
	if !ok {
		return
	}
	if err := hs.HandedOff(ctx, it, dest); err != nil {
		s.Logger.Warn("inbox: source hand-off", "item", it.Key(), "destination", dest, "error", err)
	}
}

func (s *Service) fail(key, dest string, err error, shareID string) {
	if shareID != "" {
		_ = s.FinishShare(key, shareID, ShareFailed, err.Error(), nil)
	}
	s.Logger.Warn("inbox: route failed", "item", key, "destination", dest, "error", err)
	if _, perr := s.Store.Set(key, dest, StatusFailed, Result{}, err.Error()); perr != nil {
		s.Logger.Warn("inbox: persist failed state", "item", key, "error", perr)
	}
}

// Sweep applies auto-routing rules to items the store has never seen and
// marks every listed item seen, so a rule added later never back-fires on
// old recordings. It is called after each connector sync.
func (s *Service) Sweep(ctx context.Context) (routed int, err error) {
	s.ResumeIntents(ctx)
	s.CheckRFCPublications(ctx)
	items, lerr := s.Items(ctx)
	rules := s.Config().Rules
	for _, it := range items {
		if s.Store.Seen(it.Key()) || it.AwaitingApproval {
			// An unapproved item is not marked seen: once the owner
			// approves it, the rules apply on the next sweep.
			continue
		}
		rule, ok := FirstRule(rules, it)
		if !ok {
			_ = s.Store.MarkSeen(it.Key())
			continue
		}
		if _, rerr := s.Route(ctx, it, Request{Destination: rule.Destination, Repo: rule.Repo, Actor: AutoRouteActor}); rerr != nil {
			s.Logger.Warn("inbox: auto-route", "item", it.Key(), "error", rerr)
			_ = s.Store.MarkSeen(it.Key())
			continue
		}
		routed++
	}
	return routed, lerr
}

// Wait blocks until background deliveries finish (tests, shutdown).
func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func sortItems(items []Item) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].RecordedAt.After(items[j-1].RecordedAt); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

package inbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
	"github.com/hivecommons/hive/pkg/transcribe"
)

const (
	// TranscriptionMaxAttempts bounds upload attempts per run (429/5xx and
	// network failures are retried with backoff).
	TranscriptionMaxAttempts = 3
	// transcribeJobTimeout bounds one whole run (download, upload, reply);
	// whisper runs get more for long recordings (jobTimeout).
	transcribeJobTimeout = 30 * time.Minute
	// transcribeRetryBase is the first retry wait; it doubles per attempt.
	transcribeRetryBase = 5 * time.Second
	// bytesPerMB converts max_bytes for the skip message.
	bytesPerMB = 1 << 20
)

// Transcription errors returned before anything is queued.
var (
	ErrTranscriptionDisabled = errors.New("transcription is not enabled on this hive (inbox.transcription.enabled)")
	ErrTranscriptionConsent  = errors.New("transcription needs inbox.transcription.consent_acknowledged: true")
	ErrTranscriptionBusy     = errors.New("a transcription for this item is already queued or running")
	ErrAlreadyTranscribed    = errors.New("this item already has a machine transcript")
	ErrNotPending            = errors.New("this item already has text from its source")
)

// TranscriptionSettings is the operator view of inbox.transcription with
// defaults applied, plus the key and budget status. The key value is never
// included, only whether the named variable is set.
type TranscriptionSettings struct {
	Enabled              bool     `json:"enabled"`
	Mode                 string   `json:"mode"`
	Provider             string   `json:"provider"`
	Model                string   `json:"model"`
	APIKeyEnv            string   `json:"api_key_env"`
	KeyPresent           bool     `json:"key_present"`
	Sources              []string `json:"sources"`
	Diarize              bool     `json:"diarize"`
	Language             string   `json:"language"`
	MaxDurationMinutes   int      `json:"max_duration_minutes"`
	MaxBytes             int64    `json:"max_bytes"`
	MonthlyBudgetMinutes int      `json:"monthly_budget_minutes"`
	DailyMaxItems        int      `json:"daily_max_items"`
	ConsentAcknowledged  bool     `json:"consent_acknowledged"`
	// MonthMinutesUsed / TodayItems are this spoke's recorded spend.
	MonthMinutesUsed float64 `json:"month_minutes_used"`
	TodayItems       int     `json:"today_items"`
	// EgressHosts are the hosts the hive must reach while transcription is
	// enabled with the resolved provider, for egress allowlists.
	EgressHosts []string `json:"egress_hosts"`
	// ResolvedProvider / ResolvedModel are what a run would use now (auto
	// resolved by availability); Providers is every provider's readiness.
	ResolvedProvider string           `json:"resolved_provider"`
	ResolvedModel    string           `json:"resolved_model"`
	Providers        []ProviderStatus `json:"providers"`
}

// TranscribeAudit is one inbox_transcribe audit entry. It never carries the
// audio URL or the provider key.
type TranscribeAudit struct {
	Actor        string
	Source       string
	ItemID       string
	Provider     string
	Model        string
	AudioSeconds float64
	EstimatedUSD float64
	Outcome      string
	Reason       string
}

// Fields returns the entry as key, value pairs for the audit detail.
func (a TranscribeAudit) Fields() []string {
	kv := []string{
		"source", a.Source, "item", a.ItemID, "provider", a.Provider, "model", a.Model,
		"audio_seconds", strconv.FormatFloat(a.AudioSeconds, 'f', 0, 64),
		"estimated_usd", strconv.FormatFloat(a.EstimatedUSD, 'f', 4, 64),
		"outcome", a.Outcome,
	}
	if a.Reason != "" {
		kv = append(kv, "reason", a.Reason)
	}
	return kv
}

// TranscribeOptions carries who asked and where the audit entry goes.
type TranscribeOptions struct {
	Actor string
	Audit func(TranscribeAudit)
}

// EnableTranscription wires the transcript store. Without it every
// transcription call reports ErrTranscriptionDisabled.
func (s *Service) EnableTranscription(store *TranscriptStore) {
	s.mu.Lock()
	s.transcripts = store
	s.mu.Unlock()
}

func (s *Service) transcriptStore() *TranscriptStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.transcripts
}

func (s *Service) getenv(name string) string {
	if s.Getenv != nil {
		return s.Getenv(name)
	}
	return os.Getenv(name)
}

// TranscriptionSettings returns the settings view with usage applied.
func (s *Service) TranscriptionSettings() TranscriptionSettings {
	t := s.Config().Transcription
	out := TranscriptionSettings{
		Enabled: t.Enabled, Mode: t.EffectiveMode(), Provider: t.EffectiveProvider(), Model: t.EffectiveModel(),
		APIKeyEnv: t.EffectiveAPIKeyEnv(), Sources: append([]string{}, t.Sources...), Diarize: t.EffectiveDiarize(),
		Language: t.EffectiveLanguage(), MaxDurationMinutes: int(t.EffectiveMaxDuration() / time.Minute),
		MaxBytes: t.EffectiveMaxBytes(), MonthlyBudgetMinutes: int(t.EffectiveMonthlyBudget() / time.Minute),
		DailyMaxItems: t.EffectiveDailyMaxItems(), ConsentAcknowledged: t.ConsentAcknowledged,
	}
	out.KeyPresent = strings.TrimSpace(s.getenv(out.APIKeyEnv)) != ""
	resolved := s.resolveProvider(t)
	out.ResolvedProvider, out.ResolvedModel = resolved.Name, resolved.Model
	out.Providers = s.providerStatuses(t)
	out.EgressHosts = TranscriptionEgressHosts(t, resolved.Name)
	if t.Enabled && len(resolved.Egress) > 0 {
		out.EgressHosts = append(out.EgressHosts, resolved.Egress...)
	}
	if store := s.transcriptStore(); store != nil {
		u := store.Usage(s.now())
		out.MonthMinutesUsed, out.TodayItems = u.MonthSeconds/time.Minute.Seconds(), u.TodayItems
	}
	return out
}

// DefaultTranscriber builds the provider named in t.Provider, which the
// Service sets to the resolved provider (auto is resolved before this is
// called), with any key read from the named environment variable.
func DefaultTranscriber(t config.InboxTranscription, getenv func(string) string) (transcribe.Transcriber, error) {
	p := t.EffectiveProvider()
	if p == config.InboxTranscriptionProviderAuto {
		p = config.InboxTranscriptionProviderDeepgram
	}
	if p != transcribe.DeepgramName {
		return nil, fmt.Errorf("transcription provider %q is not available on this hive version", p)
	}
	env := t.EffectiveAPIKeyEnv()
	key := strings.TrimSpace(getenv(env))
	if key == "" {
		return nil, fmt.Errorf("%w: set $%s on this hive to the owner's Deepgram API key", transcribe.ErrNoKey, env)
	}
	return &transcribe.Deepgram{APIKey: key, KeyEnv: env, Model: t.EffectiveModel(), Diarize: t.EffectiveDiarize(), Language: t.EffectiveLanguage()}, nil
}

func (s *Service) transcriber(t config.InboxTranscription) (transcribe.Transcriber, error) {
	build := s.NewTranscriber
	if build == nil {
		build = func(t config.InboxTranscription) (transcribe.Transcriber, error) {
			switch t.EffectiveProvider() {
			case config.InboxTranscriptionProviderWhisper:
				return s.whisperTranscriber(t)
			case config.InboxTranscriptionProviderGemini:
				return s.geminiTranscriber(t)
			}
			return DefaultTranscriber(t, s.getenv)
		}
	}
	tr, err := build(t)
	if err != nil {
		return nil, err
	}
	backoff := s.TranscribeBackoff
	if backoff == nil {
		backoff = transcribe.ExponentialBackoff(transcribeRetryBase)
	}
	return transcribe.Retry(tr, TranscriptionMaxAttempts, backoff), nil
}

// hasAudio reports whether the item's source can mint audio links.
func (s *Service) hasAudio(it Item) bool {
	src := s.source(it.Source)
	if _, ok := src.(AudioSource); ok {
		return !it.AudioExpired
	}
	if _, ok := src.(AudioOpener); ok {
		return it.Pending
	}
	return false
}

// Reason codes for Item.TranscribeUnavailable. no_key means the resolved
// provider is not ready (a missing key or credentials, or an engine this
// hive does not have); the ⚙ settings name which.
const (
	TranscribeBlockedDisabled = "disabled"
	TranscribeBlockedNoKey    = "no_key"
	TranscribeBlockedConsent  = "no_consent"
	TranscribeBlockedBudget   = "budget"
)

// transcribeBlockedReason names why transcription is unavailable, or "".
func (s *Service) transcribeBlockedReason(t config.InboxTranscription, store *TranscriptStore) string {
	switch {
	case !t.Enabled:
		return TranscribeBlockedDisabled
	case !s.resolveProvider(t).Available:
		return TranscribeBlockedNoKey
	case !t.ConsentAcknowledged:
		return TranscribeBlockedConsent
	case store != nil && time.Duration(store.Usage(s.now()).MonthSeconds*float64(time.Second)) >= t.EffectiveMonthlyBudget():
		return TranscribeBlockedBudget
	}
	return ""
}

// applyTranscripts sets each item's transcription state: a done machine
// transcript clears Pending (an upstream transcript, once the source has
// one, already cleared it and wins), and Transcribable marks items the
// Transcribe button applies to.
func (s *Service) applyTranscripts(items []Item) {
	store := s.transcriptStore()
	if store == nil {
		return
	}
	txCfg := s.Config().Transcription
	active := txCfg.Active()
	var blocked string
	var blockedKnown bool
	for i := range items {
		it := &items[i]
		rec, ok := store.Get(it.Source, it.ID)
		if ok {
			st := rec.State()
			s.txLive(it.Source, it.ID, &st)
			it.Transcription = &st
			if rec.Status == TranscriptionDone && it.FactState == connector.FactContentPending {
				// A transcript that finished before the fact existed (or
				// before write-back did) reaches Knowledge now.
				if changed, err := s.writeMachineFact(*it, rec); err != nil {
					s.Logger.Warn("inbox: write machine transcript to fact", "item", it.Key(), "error", err)
				} else if changed {
					it.FactState = connector.FactContentMachine
				}
			}
			if rec.Status == TranscriptionDone && it.Pending {
				it.Pending, it.HasTranscript = false, true
			}
		}
		busy := ok && (rec.Status == TranscriptionQueued || rec.Status == TranscriptionRunning || rec.Status == TranscriptionDone)
		it.Transcribable = active && it.Pending && !busy && !it.AwaitingApproval && s.hasAudio(*it)
		it.AutoTranscribe = active && it.Pending && !it.AwaitingApproval && s.hasAudio(*it) && s.autoOptedIn(*it, txCfg)
		if it.Pending && !busy && s.hasAudio(*it) {
			if !blockedKnown {
				blocked, blockedKnown = s.transcribeBlockedReason(txCfg, store), true
			}
			it.TranscribeUnavailable = blocked
		}
	}
}

// withMachineTranscript fills c from a done machine transcript when the
// source has no text (or could not be read). Source text always wins.
func (s *Service) withMachineTranscript(it Item, c Content, err error) (Content, error) {
	store := s.transcriptStore()
	if store == nil {
		return c, err
	}
	// Source text wins. A fact holding this hive's own machine transcript
	// is not source text: the stored record serves it unwrapped, labelled.
	if err == nil && !c.Pending && !c.machineFact && (c.Transcript != "" || c.Summary != "") {
		return c, nil
	}
	rec, ok := store.Get(it.Source, it.ID)
	if !ok || rec.Status != TranscriptionDone || rec.Text == "" {
		return c, err
	}
	if err != nil {
		c = Content{Title: it.Title, Provenance: it.Source + "://recordings/" + it.ID}
	}
	c.Transcript, c.TranscriptLabel, c.Pending = rec.Text, rec.Label(), false
	return c, nil
}

// Transcribe queues a machine transcription of one pending item and runs it
// in the background, one job at a time. Checks that need no upload (source
// without audio, too long, budget) are recorded as skipped immediately.
func (s *Service) Transcribe(_ context.Context, it Item, opts TranscribeOptions) (TranscriptionState, error) {
	cfg := s.Config().Transcription
	store := s.transcriptStore()
	if store == nil || !cfg.Enabled {
		return TranscriptionState{}, ErrTranscriptionDisabled
	}
	if !cfg.ConsentAcknowledged {
		return TranscriptionState{}, ErrTranscriptionConsent
	}
	s.txMu.Lock()
	defer s.txMu.Unlock()
	prev, _ := store.Get(it.Source, it.ID)
	switch prev.Status {
	case TranscriptionQueued, TranscriptionRunning:
		return prev.State(), ErrTranscriptionBusy
	case TranscriptionDone:
		return prev.State(), ErrAlreadyTranscribed
	}
	if !it.Pending {
		return TranscriptionState{}, ErrNotPending
	}
	resolved := s.resolveProvider(cfg)
	rec := TranscriptRecord{
		Source: it.Source, ID: it.ID, Attempts: prev.Attempts + 1,
		Provider: resolved.Name, Model: resolved.Model, UpdatedAt: s.now(),
	}
	if reason := s.precheck(it, cfg, store); reason != "" {
		rec.Status, rec.Reason = TranscriptionSkipped, reason
		if err := store.Put(rec); err != nil {
			return TranscriptionState{}, err
		}
		s.auditTranscription(opts, rec, 0)
		return rec.State(), nil
	}
	rec.Status = TranscriptionQueued
	if err := store.Put(rec); err != nil {
		return TranscriptionState{}, err
	}
	job := s.txEnqueue(it, rec.Provider, rec.Model, expectedSpeed(store, rec.Provider, rec.Model))
	queued := rec.State()
	s.txLive(it.Source, it.ID, &queued)
	// The run builds the resolved provider, never auto.
	cfg.Provider = rec.Provider
	s.wg.Add(1)
	run := s.Run
	if run == nil {
		run = func(fn func()) { go fn() }
	}
	run(func() {
		defer s.wg.Done()
		s.runTranscription(it, cfg, rec, opts, job)
	})
	return queued, nil
}

// precheck returns a skip reason, or "" when the item may be uploaded.
func (s *Service) precheck(it Item, cfg config.InboxTranscription, store *TranscriptStore) string {
	if !s.hasAudio(it) {
		return SkipUnsupported + ": the " + it.Source + " source offers no audio for this item"
	}
	dur := time.Duration(it.DurationMS) * time.Millisecond
	if limit := cfg.EffectiveMaxDuration(); dur > limit {
		return fmt.Sprintf("%s: the recording is %d minutes, the limit is %d (inbox.transcription.max_duration_minutes)", SkipTooLong, int(dur.Minutes()), int(limit.Minutes()))
	}
	u := store.Usage(s.now())
	if daily := cfg.EffectiveDailyMaxItems(); u.TodayItems >= daily {
		return fmt.Sprintf("%s: %d transcriptions already ran today, the daily limit is %d (inbox.transcription.daily_max_items)", SkipBudget, u.TodayItems, daily)
	}
	budget := cfg.EffectiveMonthlyBudget()
	used := time.Duration(u.MonthSeconds * float64(time.Second))
	if used+dur > budget {
		return fmt.Sprintf("%s: %d of %d minutes used this month, this recording needs %d more (inbox.transcription.monthly_budget_minutes)", SkipBudget, int(used.Minutes()), int(budget.Minutes()), int((dur + time.Minute - 1).Minutes()))
	}
	return ""
}

// runTranscription is one background run: download the audio through the
// item's source, stream it to the provider, store the transcript.
func (s *Service) runTranscription(it Item, cfg config.InboxTranscription, rec TranscriptRecord, opts TranscribeOptions, job *txJob) {
	// Runs execute one at a time, in the order they were queued.
	<-job.ready
	defer s.txDequeue(job)
	store := s.transcriptStore()
	// Estimate from the speed measured up to now, including the runs that
	// finished while this one waited.
	s.txSetSpeed(job, expectedSpeed(store, rec.Provider, rec.Model))
	key := it.Key()
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout(rec.Provider, it.DurationMS))
	defer cancel()
	started := s.now()
	rec.Status, rec.UpdatedAt, rec.StartedAt = TranscriptionRunning, started, started
	s.txProgress(job, transcribe.Progress{Phase: transcribe.PhaseDownloading, ProcessedSeconds: -1})
	s.putTranscript(store, rec)
	if err := store.AddUsage(s.now(), 0, 1); err != nil {
		s.Logger.Warn("inbox: transcription usage", "item", key, "error", err)
	}
	res, err := s.transcribeItem(ctx, it, cfg, &rec, job)
	s.txProgress(job, transcribe.Progress{Phase: transcribe.PhaseFinalizing, ProcessedSeconds: -1})
	rec.UpdatedAt = s.now()
	seconds := res.Duration.Seconds()
	if seconds == 0 {
		seconds = float64(it.DurationMS) / float64(time.Second/time.Millisecond)
	}
	switch {
	case err == nil && res.Text() == "":
		rec.Status, rec.Reason = TranscriptionFailed, "the provider returned no speech for this recording"
	case err == nil:
		rec.Status, rec.Reason, rec.Text, rec.Language, rec.AudioSeconds = TranscriptionDone, "", res.Text(), res.Language, seconds
		if res.Model != "" {
			rec.Model = res.Model
		}
		// Teach the next estimate how fast this provider is on this hive.
		if serr := store.RecordSpeed(rec.Provider, rec.Model, rec.UpdatedAt.Sub(started).Seconds(), seconds); serr != nil {
			s.Logger.Warn("inbox: transcription speed", "item", key, "error", serr)
		}
	case errors.Is(err, transcribe.ErrTooLarge):
		rec.Status, rec.Reason = TranscriptionSkipped, fmt.Sprintf("%s: the audio is larger than %d MB (inbox.transcription.max_bytes)", SkipTooLarge, cfg.EffectiveMaxBytes()/bytesPerMB)
	case errors.Is(err, ErrNoAudio), errors.Is(err, connector.ErrPlaudNoAudio), errors.Is(err, ErrRecordingExpired):
		rec.Status, rec.Reason = TranscriptionSkipped, SkipUnsupported+": "+err.Error()
	default:
		rec.Status, rec.Reason = TranscriptionFailed, err.Error()
	}
	billed := 0.0
	if rec.Status == TranscriptionDone || (rec.Status == TranscriptionFailed && err == nil) {
		billed = seconds
		if uerr := store.AddUsage(s.now(), seconds, 0); uerr != nil {
			s.Logger.Warn("inbox: transcription usage", "item", key, "error", uerr)
		}
	}
	s.putTranscript(store, rec)
	if rec.Status == TranscriptionDone {
		// The transcript becomes the item's Knowledge fact text, so search,
		// nous and agents see it, not only the Inbox.
		if _, ferr := s.writeMachineFact(it, rec); ferr != nil {
			s.Logger.Warn("inbox: write machine transcript to fact", "item", key, "error", ferr)
		}
	}
	s.finishIntents(it, rec)
	s.auditTranscription(opts, rec, billed)
	s.Logger.Info("inbox: transcription finished", "item", key, "status", rec.Status, "provider", rec.Provider, "model", rec.Model, "audio_seconds", int(billed))
}

func (s *Service) putTranscript(store *TranscriptStore, rec TranscriptRecord) {
	if err := store.Put(rec); err != nil {
		s.Logger.Warn("inbox: persist transcript", "item", Key(rec.Source, rec.ID), "error", err)
	}
}

func (s *Service) auditTranscription(opts TranscribeOptions, rec TranscriptRecord, seconds float64) {
	if opts.Audit == nil {
		return
	}
	opts.Audit(TranscribeAudit{
		Actor: opts.Actor, Source: rec.Source, ItemID: rec.ID, Provider: rec.Provider, Model: rec.Model,
		AudioSeconds: seconds, EstimatedUSD: transcribe.EstimateCostUSD(rec.Provider, rec.Model, time.Duration(seconds*float64(time.Second))),
		Outcome: rec.Status, Reason: rec.Reason,
	})
}

// transcribeItem mints a fresh audio link through the source, then lets the
// provider read the audio from a hive-side download. The link itself is
// never sent to the provider, stored, or logged (only its host).
func (s *Service) transcribeItem(ctx context.Context, it Item, cfg config.InboxTranscription, rec *TranscriptRecord, job *txJob) (transcribe.Result, error) {
	tr, err := s.transcriber(cfg)
	if err != nil {
		return transcribe.Result{}, err
	}
	if op, ok := s.source(it.Source).(AudioOpener); ok {
		// Audio kept on this hive (recordings, uploads) is read from disk:
		// the spoke never makes an HTTP call to itself.
		audio, cleanup, err := s.localTranscribeAudio(ctx, it, op, cfg, rec, job)
		if err != nil {
			return transcribe.Result{}, err
		}
		defer cleanup()
		return tr.Transcribe(ctx, audio)
	}
	link, err := s.AudioLink(ctx, it)
	if err != nil {
		return transcribe.Result{}, err
	}
	host := linkHost(link.URL)
	s.Logger.Info("inbox: transcribing", "item", it.Key(), "provider", tr.Name(), "audio_host", host)
	maxBytes := cfg.EffectiveMaxBytes()
	audio := transcribe.Audio{
		MIME:     mimeFromURL(link.URL),
		Duration: time.Duration(it.DurationMS) * time.Millisecond,
		Lang:     cfg.EffectiveLanguage(),
		Progress: func(p transcribe.Progress) { s.txProgress(job, p) },
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			s.txProgress(job, transcribe.Progress{Phase: transcribe.PhaseDownloading, ProcessedSeconds: -1})
			resp, err := s.downloadAudio(ctx, link.URL, host)
			if err != nil {
				return nil, err
			}
			rec.AudioETag = strings.Trim(resp.Header.Get("ETag"), `"`)
			if resp.ContentLength >= 0 {
				rec.AudioSize = resp.ContentLength
			}
			if maxBytes > 0 && resp.ContentLength > maxBytes {
				_ = resp.Body.Close()
				return nil, transcribe.ErrTooLarge
			}
			return readCloser{Reader: transcribe.LimitReader(resp.Body, maxBytes), Closer: resp.Body}, nil
		},
	}
	return tr.Transcribe(ctx, audio)
}

type readCloser struct {
	io.Reader
	io.Closer
}

// downloadAudio GETs the presigned link. Errors name the host only, so the
// signed query string never reaches logs, state or the UI.
func (s *Service) downloadAudio(ctx context.Context, link, host string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, fmt.Errorf("audio download from %s: invalid link", host)
	}
	client := s.AudioClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, &transcribe.RetryableError{Err: fmt.Errorf("audio download from %s: %v", host, redactLink(err.Error(), link))}
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		err := fmt.Errorf("audio download from %s: HTTP %d", host, resp.StatusCode)
		if resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests {
			return nil, &transcribe.RetryableError{Err: err}
		}
		return nil, err
	}
	return resp, nil
}

func redactLink(msg, link string) string {
	if link == "" {
		return msg
	}
	return strings.ReplaceAll(msg, link, "[audio link]")
}

// linkHost returns the host of a link, or "unknown host".
func linkHost(link string) string {
	if u, err := url.Parse(link); err == nil && u.Host != "" {
		return u.Host
	}
	return "unknown host"
}

// audioMIMEByExt maps common recording extensions to content types; others
// are sent as application/octet-stream and detected by the provider.
var audioMIMEByExt = map[string]string{
	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".mp4":  "audio/mp4",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".opus": "audio/ogg",
	".webm": "audio/webm",
	".flac": "audio/flac",
}

func mimeFromURL(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return audioMIMEByExt[strings.ToLower(path.Ext(u.Path))]
}

package inbox

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Auto-transcription of recordings that land in the Inbox. Audio this hive
// holds itself (dashboard recordings, dropped audio and video, mailed-in
// audio: any source implementing AudioOpener) is transcribed as soon as it
// arrives, whatever inbox.transcription.mode says, once transcription is
// enabled with consent; other sources follow mode: auto and
// inbox.transcription.sources. No click is needed: the 🎧 Retry
// transcription button stays for failed and skipped items.
//
// Three hooks start runs:
//   - creation: FinalizeRecording (and the idle finalize of the recording
//     sweep), AddMedia and the mail-in handlers call AutoTranscribe for the
//     new item;
//   - boot: RecoverTranscriptions marks the runs the previous process left
//     queued or running as interrupted, and the boot transcription sweep
//     (TranscribeSweep) queues them again with every item still pending;
//   - the periodic sweeps pick up anything that arrived while transcription
//     was off.
//
// Runs execute in the background, at most transcribeConcurrency at a time.
// Transcribe's check-and-set under txMu makes every hook idempotent: an item
// queued, running or done is never started twice.

const (
	// TranscribeConcurrencyEnv sets how many transcriptions run at once.
	TranscribeConcurrencyEnv = "HIVE_INBOX_TRANSCRIBE_CONCURRENCY"
	// DefaultTranscribeConcurrency is one run at a time: whisper decodes on
	// this hive's own CPU, and a paid provider's budget is spent in order.
	DefaultTranscribeConcurrency = 1
	// maxTranscribeConcurrency caps the override so a typo cannot start a
	// whisper process per recording.
	maxTranscribeConcurrency = 8
	// ReasonInterrupted prefixes the Reason of a run the hive restarted
	// under; autoDue retries it at the next sweep without a backoff.
	ReasonInterrupted = "interrupted"
)

// transcribeConcurrency is how many runs may execute at once: the
// TranscribeConcurrency field, else $HIVE_INBOX_TRANSCRIBE_CONCURRENCY, else
// DefaultTranscribeConcurrency, capped at maxTranscribeConcurrency. It is
// read once, so the queue's slot count never changes under queued jobs.
func (s *Service) transcribeConcurrency() int {
	s.txConcOnce.Do(func() {
		n := s.TranscribeConcurrency
		if n <= 0 {
			n = DefaultTranscribeConcurrency
			if v := strings.TrimSpace(s.getenv(TranscribeConcurrencyEnv)); v != "" {
				parsed, err := strconv.Atoi(v)
				switch {
				case err != nil || parsed < 1:
					if s.Logger != nil {
						s.Logger.Warn("inbox: ignoring invalid transcription concurrency", "env", TranscribeConcurrencyEnv, "value", v, "default", DefaultTranscribeConcurrency)
					}
				default:
					n = parsed
				}
			}
		}
		s.txConc = min(n, maxTranscribeConcurrency)
	})
	return s.txConc
}

// hasLocalAudio reports whether the item's audio lives on this hive.
func (s *Service) hasLocalAudio(it Item) bool {
	_, ok := s.source(it.Source).(AudioOpener)
	return ok
}

// hasLocalAudioSource reports whether any source keeps audio on this hive.
func (s *Service) hasLocalAudioSource() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, src := range s.sources {
		if _, ok := src.(AudioOpener); ok {
			return true
		}
	}
	return false
}

// autoOptedIn reports whether the hive transcribes it without a click:
// audio it holds itself always, other sources in auto mode when listed.
func (s *Service) autoOptedIn(it Item, cfg config.InboxTranscription) bool {
	return s.hasLocalAudio(it) || cfg.AutoSource(it.Source) || cfg.AutoSource(it.Connector)
}

// AutoTranscribe queues the machine transcription of an item that just
// landed in the Inbox, when it is a pending recording the hive transcribes
// on its own (autoOptedIn) and transcription is enabled with consent. It
// never blocks on the run. It is idempotent: an item already queued,
// running or done, or a failure whose retry is not due (autoDue), is left
// alone. ok reports that a run was queued (or recorded as skipped);
// failures to queue are logged, never returned, so the caller's create
// still succeeds. opts.Actor defaults to AutoTranscribeActor.
func (s *Service) AutoTranscribe(ctx context.Context, it Item, opts TranscribeOptions) (TranscriptionState, bool) {
	cfg := s.Config().Transcription
	store := s.transcriptStore()
	if store == nil || !cfg.Active() || !it.Pending || it.AwaitingApproval || !s.hasAudio(it) || !s.autoOptedIn(it, cfg) {
		return TranscriptionState{}, false
	}
	if rec, ok := store.Get(it.Source, it.ID); ok {
		st := rec.State()
		if !autoDue(&st, s.now()) {
			return st, false
		}
	}
	if opts.Actor == "" {
		opts.Actor = AutoTranscribeActor
	}
	st, err := s.Transcribe(ctx, it, opts)
	switch {
	case errors.Is(err, ErrTranscriptionBusy), errors.Is(err, ErrAlreadyTranscribed):
		// Another hook got there first.
		return st, false
	case err != nil:
		s.Logger.Warn("inbox: auto-transcription not queued", "item", it.Key(), "error", err)
		return st, false
	}
	return st, true
}

// RecoverTranscriptions runs once at boot, before anything is queued: a
// record still queued or running has no job in this process, so the
// previous one died under it. Each becomes failed with a ReasonInterrupted
// reason, which shows on the item and lets the boot sweep queue it again
// at once. A run that had started keeps its attempt, so a recording that
// crashes the hive is retried at most TranscriptionMaxAttempts times in
// all; one that was only waiting in the queue gets its attempt back.
func (s *Service) RecoverTranscriptions() (int, error) {
	store := s.transcriptStore()
	if store == nil {
		return 0, nil
	}
	recs, err := store.List()
	if err != nil {
		return 0, err
	}
	s.txMu.Lock()
	defer s.txMu.Unlock()
	recovered := 0
	for _, rec := range recs {
		if rec.Status != TranscriptionQueued && rec.Status != TranscriptionRunning {
			continue
		}
		if s.txIsLive(rec.Source, rec.ID) {
			continue
		}
		if rec.Status == TranscriptionQueued && rec.Attempts > 0 {
			rec.Attempts--
		}
		rec.Reason = ReasonInterrupted + ": the hive restarted while this transcription was " + rec.Status + "; it is retried automatically"
		rec.Status, rec.UpdatedAt = TranscriptionFailed, s.now()
		if err := store.Put(rec); err != nil {
			s.Logger.Warn("inbox: recover interrupted transcription", "item", Key(rec.Source, rec.ID), "error", err)
			continue
		}
		s.Logger.Warn("inbox: transcription interrupted by a restart", "item", Key(rec.Source, rec.ID), "attempts", rec.Attempts)
		recovered++
	}
	return recovered, nil
}

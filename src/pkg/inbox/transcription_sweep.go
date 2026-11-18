package inbox

import (
	"context"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

const (
	// AutoTranscribeActor is the audit actor of sweep-started runs.
	AutoTranscribeActor = "inbox-auto-transcribe"
	// autoRetryBase is the wait before the sweep retries a failed item; it
	// doubles per attempt (1h, 2h) until TranscriptionMaxAttempts.
	autoRetryBase = time.Hour
)

// TranscribeSweep queues machine transcriptions for pending items from the
// sources listed in inbox.transcription.sources (auto mode) and for every
// pending item whose audio this hive holds itself (recordings, uploads,
// mail-in audio: see autoOptedIn), with transcription enabled and
// consented. It is called after each connector sync, by the recording
// sweep and once at boot, after RecoverTranscriptions, so a run the hive
// restarted under is picked up again. Runs still execute one at a time; the sweep only queues
// what today's item cap and this month's minutes leave room for, so a
// backlog never turns into a burst of budget skips.
//
// Retries: a failed item is retried after a backoff (1h, then 2h) until it
// has had TranscriptionMaxAttempts runs, across restarts too (the state is
// on disk); a run interrupted by a restart is retried at the next sweep
// without a backoff, within the same attempt bound; an item skipped for
// budget is retried once the UTC day has turned; too long, too large and
// unsupported skips are never retried automatically (the owner can still
// click Retry transcription).
func (s *Service) TranscribeSweep(ctx context.Context, opts TranscribeOptions) (queued int, err error) {
	cfg := s.Config().Transcription
	store := s.transcriptStore()
	// Hive-local audio is transcribed whatever the mode: recording or
	// dropping it opts it in, once transcription is enabled with consent.
	if store == nil || !cfg.Active() {
		return 0, nil
	}
	if cfg.EffectiveMode() != config.InboxTranscriptionModeAuto && !s.hasLocalAudioSource() {
		return 0, nil
	}
	items, err := s.Items(ctx)
	now := s.now()
	u := store.Usage(now)
	itemsLeft := cfg.EffectiveDailyMaxItems() - u.TodayItems
	timeLeft := cfg.EffectiveMonthlyBudget() - time.Duration(u.MonthSeconds*float64(time.Second))
	seen := map[string]bool{}
	for _, it := range items {
		if itemsLeft <= 0 {
			break
		}
		if seen[it.Key()] || !it.Transcribable || !s.autoOptedIn(it, cfg) || !autoDue(it.Transcription, now) {
			continue
		}
		seen[it.Key()] = true
		dur := time.Duration(it.DurationMS) * time.Millisecond
		if dur > timeLeft {
			// Leave it pending for next month instead of recording a skip.
			continue
		}
		st, terr := s.Transcribe(ctx, it, opts)
		if terr != nil {
			s.Logger.Warn("inbox: auto transcription", "item", it.Key(), "error", terr)
			continue
		}
		if st.Status == TranscriptionQueued {
			queued++
			itemsLeft--
			timeLeft -= dur
		}
	}
	return queued, err
}

// autoDue reports whether the sweep should (re)try an item now.
func autoDue(st *TranscriptionState, now time.Time) bool {
	if st == nil || st.Status == "" {
		return true
	}
	if st.Attempts >= TranscriptionMaxAttempts {
		return false
	}
	switch st.Status {
	case TranscriptionFailed:
		if strings.HasPrefix(st.Reason, ReasonInterrupted) {
			// The hive restarted under the run: no backoff, the attempt
			// bound above still stops a recording that keeps crashing it.
			return true
		}
		return !now.Before(st.UpdatedAt.Add(transcribe.ExponentialBackoff(autoRetryBase)(st.Attempts)))
	case TranscriptionSkipped:
		return strings.HasPrefix(st.Reason, SkipBudget) && now.UTC().Format(usageDayLayout) != st.UpdatedAt.UTC().Format(usageDayLayout)
	}
	return false
}

// Detail renders the entry as "key=value, key=value", the audit-log detail
// shape the dashboard writes for request-driven runs.
func (a TranscribeAudit) Detail() string {
	kv := a.Fields()
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, kv[i]+"="+kv[i+1])
	}
	return strings.Join(parts, ", ")
}

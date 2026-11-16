package inbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Share intent statuses.
const (
	// IntentWaiting: the share runs when the machine transcript completes.
	IntentWaiting = "waiting"
	// IntentFailed: the transcript did not complete (or the share could not
	// start); nothing was sent. The Share menu offers a retry.
	IntentFailed = "failed"
)

// ShareIntent is a share chosen for an audio-only item before it has any
// text. The hive transcribes first and runs the share when the transcript
// completes. It holds only the choices made in the dialog, never item text
// or secrets, and is persisted with the item state so a restart resumes it.
type ShareIntent struct {
	// Destination is spek_rfc, dismiss (Knowledge) or a destination with a
	// registered IntentRunner (e.g. the spek session).
	Destination string `json:"destination"`
	// Actor is the dashboard user who chose the share.
	Actor string `json:"actor"`
	// Repo is the RFC repository picked in the dialog.
	Repo string `json:"repo,omitempty"`
	// Params carries other short dialog choices (e.g. a spek session name or
	// "new session" flag). Values are choices, not item content.
	Params map[string]string `json:"params,omitempty"`
	// RecordID is the share-history record that tracks this share.
	RecordID  string    `json:"record_id,omitempty"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IntentRunner performs a share for an item that now has text. Built in for
// spek_rfc and dismiss; other destinations register one with
// RegisterIntentRunner.
type IntentRunner func(ctx context.Context, it Item, in ShareIntent) error

// ErrTranscribeUnavailable wraps the reason transcription cannot run now
// (a TranscribeBlocked* code); shares that need a transcript are refused.
var ErrTranscribeUnavailable = errors.New("transcription is not available")

// ShareOutcome says what ShareWhenReady did.
type ShareOutcome struct {
	// Waiting is true when the share is queued behind a transcription.
	Waiting bool
	// State is the route state when the share started immediately.
	State RouteState
}

// RegisterIntentRunner is the hook other destinations use to take part in
// transcribe-then-share. The spek session action registers "spek" here, with
// a runner that seeds the session from ShareIntent.Params.
func (s *Service) RegisterIntentRunner(dest string, run IntentRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.intentRunners == nil {
		s.intentRunners = map[string]IntentRunner{}
	}
	s.intentRunners[dest] = run
}

func (s *Service) intentRunner(dest string) (IntentRunner, bool) {
	s.mu.RLock()
	run, ok := s.intentRunners[dest]
	s.mu.RUnlock()
	if ok {
		return run, true
	}
	switch dest {
	case DestSpekRFC, DestDismiss:
		return func(ctx context.Context, it Item, in ShareIntent) error {
			_, err := s.Route(ctx, it, Request{Destination: in.Destination, Repo: in.Repo, Actor: in.Actor, ShareID: in.RecordID})
			return err
		}, true
	}
	return nil, false
}

// needsTranscript reports whether an item has audio but no text yet.
func (s *Service) needsTranscript(it Item) bool {
	return it.Pending && s.hasAudio(it)
}

// ShareWhenReady shares an item to a Hive destination. An item with text is
// shared now; an audio-only item is transcribed first (the manual path, with
// its consent, budget and cap checks) and shared when the transcript
// completes. When transcription cannot run, ErrTranscribeUnavailable (with
// the reason code) is returned and nothing is stored or sent.
func (s *Service) ShareWhenReady(ctx context.Context, it Item, in ShareIntent, opts TranscribeOptions) (ShareOutcome, error) {
	run, ok := s.intentRunner(in.Destination)
	if !ok {
		return ShareOutcome{}, fmt.Errorf("%w: %q", ErrUnknownDestination, in.Destination)
	}
	if !s.needsTranscript(it) {
		st := RouteState{Status: StatusQueued, UpdatedAt: s.now()}
		if err := run(ctx, it, in); err != nil {
			return ShareOutcome{}, err
		}
		return ShareOutcome{State: st}, nil
	}
	store := s.transcriptStore()
	cfg := s.Config().Transcription
	if reason := s.transcribeBlockedReason(cfg, store); reason != "" {
		return ShareOutcome{}, fmt.Errorf("%w: %s", ErrTranscribeUnavailable, reason)
	}
	in.Status, in.Error, in.CreatedAt, in.UpdatedAt = IntentWaiting, "", s.now(), s.now()
	id, rerr := s.RecordShare(it.Key(), ShareRecord{Destination: shareDestFor(in.Destination), Actor: in.Actor, Status: ShareTranscribing})
	if rerr != nil {
		return ShareOutcome{}, rerr
	}
	in.RecordID = id
	if err := s.Store.SetIntent(it.Key(), in); err != nil {
		_ = s.FinishShare(it.Key(), id, ShareFailed, err.Error(), nil)
		return ShareOutcome{}, err
	}
	st, err := s.Transcribe(ctx, it, opts)
	switch {
	case errors.Is(err, ErrTranscriptionBusy):
		// A run is already in flight; the intent rides on it.
	case errors.Is(err, ErrAlreadyTranscribed):
		s.Store.ClearIntent(it.Key(), in.Destination)
		// Run now, continuing the record created above.
		return s.ShareWhenReady(ctx, withText(it), in, opts)
	case err != nil:
		s.Store.ClearIntent(it.Key(), in.Destination)
		_ = s.FinishShare(it.Key(), id, ShareFailed, err.Error(), nil)
		return ShareOutcome{}, err
	case st.Status == TranscriptionSkipped:
		s.Store.ClearIntent(it.Key(), in.Destination)
		_ = s.FinishShare(it.Key(), id, ShareFailed, st.Reason, nil)
		return ShareOutcome{}, fmt.Errorf("%w: %s", ErrTranscribeUnavailable, st.Reason)
	}
	return ShareOutcome{Waiting: true}, nil
}

func withText(it Item) Item { it.Pending, it.HasTranscript = false, true; return it }

// finishIntents runs (or fails) the intents waiting on a finished
// transcription. It is called after the transcript record is stored.
func (s *Service) finishIntents(it Item, rec TranscriptRecord) {
	key := it.Key()
	intents := s.Store.Intents(key)
	if len(intents) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), routeTimeout)
	defer cancel()
	for dest, in := range intents {
		if in.Status != IntentWaiting {
			continue
		}
		if rec.Status != TranscriptionDone {
			s.failIntent(key, dest, "transcription "+rec.Status+": "+rec.Reason)
			continue
		}
		run, ok := s.intentRunner(dest)
		if !ok {
			s.failIntent(key, dest, "no handler for destination "+dest)
			continue
		}
		done := withText(it)
		if err := run(ctx, done, *in); err != nil {
			s.failIntent(key, dest, err.Error())
			continue
		}
		s.Store.ClearIntent(key, dest)
		s.Logger.Info("inbox: shared after transcription", "item", key, "destination", dest, "actor", in.Actor)
	}
}

func (s *Service) failIntent(key, dest, reason string) {
	if in := s.Store.Intents(key)[dest]; in != nil && in.RecordID != "" {
		_ = s.FinishShare(key, in.RecordID, ShareFailed, reason, nil)
	}
	s.Logger.Warn("inbox: share after transcription failed", "item", key, "destination", dest, "reason", reason)
	if err := s.Store.FailIntent(key, dest, strings.TrimSpace(reason)); err != nil {
		s.Logger.Warn("inbox: persist failed share intent", "item", key, "error", err)
	}
}

// ResumeIntents re-attaches shares that were waiting on a transcript when
// the hive stopped: a finished transcript runs them, an interrupted one is
// transcribed again. Sweep calls it after every sync.
func (s *Service) ResumeIntents(ctx context.Context) {
	for _, key := range s.Store.IntentKeys() {
		source, id, _ := strings.Cut(key, "/")
		it, found, _ := s.Find(ctx, source, id)
		if !found {
			continue
		}
		store := s.transcriptStore()
		if store == nil {
			continue
		}
		rec, ok := store.Get(it.Source, it.ID)
		switch {
		case ok && rec.Status == TranscriptionDone:
			s.finishIntents(it, rec)
		case ok && (rec.Status == TranscriptionFailed || rec.Status == TranscriptionSkipped):
			s.finishIntents(it, rec)
		case ok && (rec.Status == TranscriptionQueued || rec.Status == TranscriptionRunning) && s.txIsLive(it.Source, it.ID):
			// A live job will finish the intents itself.
		default:
			opts := TranscribeOptions{Actor: AutoTranscribeActor}
			if rec.Status == TranscriptionQueued || rec.Status == TranscriptionRunning {
				// Persisted as running but no job exists: the hive restarted.
				rec.Status = TranscriptionFailed
				_ = store.Put(rec)
			}
			if _, err := s.Transcribe(ctx, it, opts); err != nil && !errors.Is(err, ErrTranscriptionBusy) {
				for dest := range s.Store.Intents(key) {
					s.failIntent(key, dest, err.Error())
				}
			}
		}
	}
}

func cloneIntents(in map[string]*ShareIntent) map[string]*ShareIntent {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*ShareIntent, len(in))
	for k, v := range in {
		c := *v
		if v.Params != nil {
			c.Params = make(map[string]string, len(v.Params))
			for pk, pv := range v.Params {
				c.Params[pk] = pv
			}
		}
		out[k] = &c
	}
	return out
}

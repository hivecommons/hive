package inbox

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/transcribe"
)

// LocalAudio is an audio (or video) file on this hive's own disk.
type LocalAudio struct {
	File *os.File
	// Name is the file name http.ServeContent uses for type fallbacks.
	Name string
	// MIME is the media type it is served and transcribed as.
	MIME    string
	Size    int64
	ModTime time.Time
}

// AudioOpener is optionally implemented by any source whose audio lives on
// this hive (dashboard recordings, uploaded audio and video files, ...).
// Playback serves the file directly with Range support, and transcription
// reads it from disk: nothing makes an HTTP call to the hive itself. A
// source that implements AudioOpener should also implement AudioSource so
// the item counts as having audio. OpenAudio returns ErrNoAudio (or an
// error wrapping it) when the item has no audio file.
type AudioOpener interface {
	OpenAudio(ctx context.Context, it Item) (LocalAudio, error)
}

// localTranscribeAudio opens a hive-local file for a transcription run. The
// first open happens here so the provider gets the file's MIME type and
// size; retries reopen it. The source checked the container when the file
// arrived; decoding is the provider's job (the sandboxed whisper subprocess
// by default). The returned cleanup closes the first handle if the provider
// never read it.
func (s *Service) localTranscribeAudio(ctx context.Context, it Item, op AudioOpener, cfg config.InboxTranscription, rec *TranscriptRecord, job *txJob) (transcribe.Audio, func(), error) {
	s.txProgress(job, transcribe.Progress{Phase: transcribe.PhaseDownloading, ProcessedSeconds: -1})
	first, err := op.OpenAudio(ctx, it)
	if err != nil {
		return transcribe.Audio{}, nil, err
	}
	maxBytes := cfg.EffectiveMaxBytes()
	rec.AudioSize = first.Size
	pending := &first
	cleanup := func() {
		if pending != nil {
			_ = pending.File.Close()
			pending = nil
		}
	}
	if maxBytes > 0 && first.Size > maxBytes {
		cleanup()
		return transcribe.Audio{}, nil, transcribe.ErrTooLarge
	}
	a := transcribe.Audio{
		MIME:     first.MIME,
		Size:     first.Size,
		Duration: time.Duration(it.DurationMS) * time.Millisecond,
		Lang:     cfg.EffectiveLanguage(),
		Progress: func(p transcribe.Progress) { s.txProgress(job, p) },
	}
	a.Open = func(ctx context.Context) (io.ReadCloser, error) {
		la := pending
		pending = nil
		if la == nil {
			s.txProgress(job, transcribe.Progress{Phase: transcribe.PhaseDownloading, ProcessedSeconds: -1})
			reopened, err := op.OpenAudio(ctx, it)
			if err != nil {
				return nil, err
			}
			la = &reopened
		}
		return readCloser{Reader: transcribe.LimitReader(la.File, maxBytes), Closer: la.File}, nil
	}
	return a, cleanup, nil
}

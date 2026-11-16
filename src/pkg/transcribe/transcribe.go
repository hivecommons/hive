// Package transcribe turns recorded audio into speaker-labelled transcript
// segments through a pluggable speech-to-text provider.
//
// Each hive owns its own transcriptions: the adapter runs on the spoke, with
// the spoke owner's own provider key, and the transcript is stored only on
// that spoke. Nothing here is shared across hives or billed centrally.
//
// Adapters: Deepgram (pre-recorded /v1/listen) and Fake (tests). Others
// plug in behind the same interface and report progress through
// Audio.Progress.
package transcribe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Transcriber is one speech-to-text provider.
type Transcriber interface {
	// Name is the provider name recorded with every transcript ("deepgram").
	Name() string
	// Transcribe uploads the audio and returns the diarized segments.
	Transcribe(ctx context.Context, a Audio) (Result, error)
}

// Audio is one recording to transcribe. Open is called once per upload
// attempt and must return a fresh stream each time, so a retry re-reads the
// audio from the start.
type Audio struct {
	Open func(ctx context.Context) (io.ReadCloser, error)
	// MIME is the audio content type; empty sends application/octet-stream.
	MIME string
	// Size is the byte length when known, else 0.
	Size int64
	// Duration is the recording length when known, else 0.
	Duration time.Duration
	// Lang is a BCP-47 language hint; "" or "auto" asks for detection.
	Lang string
	// Progress, when set, receives phase changes and measured progress
	// (see Report). It must be cheap and safe to call from any goroutine.
	Progress func(Progress)
}

// Segment is one utterance of the transcript.
type Segment struct {
	Start   time.Duration `json:"start"`
	Speaker string        `json:"speaker,omitempty"`
	Text    string        `json:"text"`
}

// Result is a finished transcript.
type Result struct {
	Segments []Segment `json:"segments"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Language string    `json:"language,omitempty"`
	// Duration is the audio length the provider reports (what it bills).
	Duration time.Duration `json:"duration,omitempty"`
}

// Text renders the result in the Inbox transcript shape.
func (r Result) Text() string { return RenderSegments(r.Segments) }

// ErrUnauthorized is returned when the provider rejects the API key.
var ErrUnauthorized = errors.New("transcription provider rejected the API key")

// ErrNoKey is returned when the API key environment variable is empty.
var ErrNoKey = errors.New("transcription API key is not set")

// ErrTooLarge is returned when the audio stream exceeds the byte limit.
var ErrTooLarge = errors.New("audio exceeds the transcription size limit")

// RetryableError marks a transient provider failure (429, 5xx, network).
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// IsRetryable reports whether err is worth another attempt.
func IsRetryable(err error) bool {
	var r *RetryableError
	return errors.As(err, &r)
}

// LimitReader returns a reader that fails with ErrTooLarge as soon as more
// than limit bytes have been read from r. A non-positive limit disables the
// cap. Unlike io.LimitReader it never truncates silently: a cut upload must
// fail, not produce a transcript of the first half.
func LimitReader(r io.Reader, limit int64) io.Reader {
	if limit <= 0 {
		return r
	}
	return &limitReader{r: r, left: limit}
}

type limitReader struct {
	r    io.Reader
	left int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, ErrTooLarge
	}
	// Read one byte past the limit so an exactly-limit stream still passes.
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return 0, ErrTooLarge
	}
	return n, err
}

// Retry wraps t so retryable failures are attempted again, up to attempts
// in total, waiting backoff(attempt) between tries (attempt starts at 1).
// Every attempt reopens the audio through Audio.Open.
func Retry(t Transcriber, attempts int, backoff func(attempt int) time.Duration) Transcriber {
	if attempts < 1 {
		attempts = 1
	}
	return &retrying{inner: t, attempts: attempts, backoff: backoff}
}

type retrying struct {
	inner    Transcriber
	attempts int
	backoff  func(int) time.Duration
}

func (r *retrying) Name() string { return r.inner.Name() }

func (r *retrying) Transcribe(ctx context.Context, a Audio) (Result, error) {
	var err error
	for i := 1; i <= r.attempts; i++ {
		var res Result
		res, err = r.inner.Transcribe(ctx, a)
		if err == nil || !IsRetryable(err) || i == r.attempts {
			return res, err
		}
		var wait time.Duration
		if r.backoff != nil {
			wait = r.backoff(i)
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	return Result{}, err
}

// ExponentialBackoff returns base, 2*base, 4*base, ... for attempts 1, 2, 3.
func ExponentialBackoff(base time.Duration) func(int) time.Duration {
	return func(attempt int) time.Duration {
		if attempt < 1 {
			attempt = 1
		}
		return base << (attempt - 1)
	}
}

// FormatOffset formats a transcript offset as mm:ss, or h:mm:ss past an hour.
func FormatOffset(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// WriteLine appends one "[stamp] Speaker: text" line. An empty stamp or
// speaker is left out. This is the one line shape every Inbox transcript
// uses: Plaud, WebVTT meeting captions and machine transcripts alike.
func WriteLine(b *strings.Builder, stamp, speaker, text string) {
	if stamp != "" {
		b.WriteString("[" + stamp + "] ")
	}
	if speaker != "" {
		b.WriteString(speaker + ": ")
	}
	b.WriteString(text)
	b.WriteByte('\n')
}

// RenderSegments renders segments as "[mm:ss] Speaker N: text" lines.
// Segments without text are skipped.
func RenderSegments(segs []Segment) string {
	var b strings.Builder
	for _, s := range segs {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		WriteLine(&b, FormatOffset(s.Start), strings.TrimSpace(s.Speaker), text)
	}
	return strings.TrimSpace(b.String())
}

// usdPerMinute is the provider's published pay-as-you-go list price per
// audio minute, used only for the estimate written to the audit log. The
// owner's own provider invoice is authoritative.
var usdPerMinute = map[string]float64{
	DeepgramName + "/" + DefaultDeepgramModel: deepgramNova3USDPerMinute,
}

// EstimateCostUSD estimates what transcribing d of audio costs on the
// owner's own provider account; 0 when the model has no known price.
func EstimateCostUSD(provider, model string, d time.Duration) float64 {
	return usdPerMinute[provider+"/"+model] * d.Minutes()
}

package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DeepgramName is the provider name recorded with Deepgram transcripts.
	DeepgramName = "deepgram"
	// DefaultDeepgramModel is the pre-recorded model used when none is set.
	DefaultDeepgramModel = "nova-3"
	// DeepgramHost is the API host audio is uploaded to (egress allowlists).
	DeepgramHost = "api.deepgram.com"
	// DefaultDeepgramBaseURL is the Deepgram API root.
	DefaultDeepgramBaseURL = "https://" + DeepgramHost
	// DefaultDeepgramKeyEnv names the environment variable holding the key.
	DefaultDeepgramKeyEnv = "DEEPGRAM_API_KEY"
	// DefaultDeepgramTimeout bounds one upload + transcription request.
	DefaultDeepgramTimeout = 20 * time.Minute
	// deepgramListenPath is the pre-recorded transcription endpoint.
	deepgramListenPath = "/v1/listen"
	// deepgramMaxResponseBytes bounds the JSON reply read into memory (word
	// timings for a two-hour recording stay well under this).
	deepgramMaxResponseBytes = 64 << 20
	// deepgramMaxErrorBytes bounds how much of an error body is quoted.
	deepgramMaxErrorBytes = 512
	// deepgramNova3USDPerMinute is Deepgram's pay-as-you-go list price for
	// nova-3 pre-recorded audio, for the audit-log estimate only.
	deepgramNova3USDPerMinute = 0.0043
	// deepgramLanguageAuto asks Deepgram to detect the spoken language.
	deepgramLanguageAuto = "auto"
)

// Deepgram transcribes through Deepgram's pre-recorded /v1/listen API with
// the hive owner's own API key. Audio is streamed as the request body (the
// hive never hands Deepgram a URL to fetch) and every request sets
// mip_opt_out=true so Deepgram does not use the audio to train models.
type Deepgram struct {
	// APIKey is the owner's Deepgram key. It is sent only in the
	// Authorization header and never logged or returned in errors.
	APIKey string
	// KeyEnv names the environment variable the key came from, so errors can
	// tell the owner where to look without ever quoting the value.
	KeyEnv string
	// Model defaults to DefaultDeepgramModel.
	Model string
	// Diarize labels speakers ("Speaker 1", "Speaker 2", ...).
	Diarize bool
	// Language is a BCP-47 code; "" or "auto" enables detection.
	Language string
	// BaseURL defaults to DefaultDeepgramBaseURL (tests point it at httptest).
	BaseURL string
	// Client defaults to a client without its own timeout; Timeout bounds
	// the request through the context instead.
	Client *http.Client
	// Timeout defaults to DefaultDeepgramTimeout.
	Timeout time.Duration
}

// Name implements Transcriber.
func (d *Deepgram) Name() string { return DeepgramName }

func (d *Deepgram) model() string {
	if m := strings.TrimSpace(d.Model); m != "" {
		return m
	}
	return DefaultDeepgramModel
}

func (d *Deepgram) keyEnv() string {
	if d.KeyEnv != "" {
		return d.KeyEnv
	}
	return DefaultDeepgramKeyEnv
}

// listenURL builds the /v1/listen request URL with the transcription options.
func (d *Deepgram) listenURL(lang string) (string, error) {
	base := strings.TrimRight(d.BaseURL, "/")
	if base == "" {
		base = DefaultDeepgramBaseURL
	}
	u, err := url.Parse(base + deepgramListenPath)
	if err != nil {
		return "", fmt.Errorf("deepgram: base URL: %w", err)
	}
	q := url.Values{}
	q.Set("model", d.model())
	q.Set("smart_format", "true")
	q.Set("utterances", "true")
	q.Set("diarize", strconv.FormatBool(d.Diarize))
	// Opt out of the Deepgram Model Improvement Program: the audio holds
	// other people's voices and must not be used for training.
	q.Set("mip_opt_out", "true")
	lang = strings.TrimSpace(lang)
	if lang == "" {
		lang = strings.TrimSpace(d.Language)
	}
	if lang == "" || strings.EqualFold(lang, deepgramLanguageAuto) {
		q.Set("detect_language", "true")
	} else {
		q.Set("language", lang)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// bodyReader remembers the first read error so an oversize stream is
// reported as ErrTooLarge whatever the transport or server made of the cut.
type bodyReader struct {
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF && b.err == nil {
		b.err = err
	}
	return n, err
}

// Transcribe implements Transcriber.
func (d *Deepgram) Transcribe(ctx context.Context, a Audio) (Result, error) {
	if strings.TrimSpace(d.APIKey) == "" {
		return Result{}, fmt.Errorf("%w: set $%s to the hive owner's Deepgram API key", ErrNoKey, d.keyEnv())
	}
	if a.Open == nil {
		return Result{}, errors.New("deepgram: no audio to upload")
	}
	endpoint, err := d.listenURL(a.Lang)
	if err != nil {
		return Result{}, err
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DefaultDeepgramTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rc, err := a.Open(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("deepgram: open audio: %w", err)
	}
	defer rc.Close()
	// The download streams straight into the upload, so from here on the
	// provider is working: one request, no measurable progress.
	a.ReportPhase(PhaseTranscribing)
	body := &bodyReader{r: rc}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return Result{}, fmt.Errorf("deepgram: build request: %w", err)
	}
	mime := strings.TrimSpace(a.MIME)
	if mime == "" {
		mime = "application/octet-stream"
	}
	req.Header.Set("Content-Type", mime)
	req.Header.Set("Authorization", "Token "+d.APIKey)
	client := d.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if errors.Is(body.err, ErrTooLarge) {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return Result{}, ErrTooLarge
	}
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, &RetryableError{Err: fmt.Errorf("deepgram: request timed out or was cancelled: %w", ctx.Err())}
		}
		return Result{}, &RetryableError{Err: fmt.Errorf("deepgram: request to %s failed: %w", DeepgramHost, unwrapURLError(err))}
	}
	defer resp.Body.Close()
	if err := d.statusError(resp); err != nil {
		return Result{}, err
	}
	var parsed deepgramResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, deepgramMaxResponseBytes)).Decode(&parsed); err != nil {
		return Result{}, fmt.Errorf("deepgram: decode response: %w", err)
	}
	return parsed.result(d.model()), nil
}

// statusError maps a non-2xx reply to an error. The key is never quoted.
func (d *Deepgram) statusError(resp *http.Response) error {
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, deepgramMaxErrorBytes))
	detail := strings.TrimSpace(strings.ReplaceAll(string(snippet), d.APIKey, "[redacted]"))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d): check the Deepgram key in $%s", ErrUnauthorized, resp.StatusCode, d.keyEnv())
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return &RetryableError{Err: fmt.Errorf("deepgram: HTTP %d: %s", resp.StatusCode, detail)}
	default:
		return fmt.Errorf("deepgram: HTTP %d: %s", resp.StatusCode, detail)
	}
}

// unwrapURLError drops the request URL from a transport error.
func unwrapURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// deepgramResponse is the part of the /v1/listen reply the hive reads.
type deepgramResponse struct {
	Metadata struct {
		Duration float64 `json:"duration"`
	} `json:"metadata"`
	Results struct {
		Channels []struct {
			DetectedLanguage string `json:"detected_language"`
			Alternatives     []struct {
				Transcript string `json:"transcript"`
			} `json:"alternatives"`
		} `json:"channels"`
		Utterances []struct {
			Start      float64 `json:"start"`
			Speaker    *int    `json:"speaker"`
			Transcript string  `json:"transcript"`
		} `json:"utterances"`
	} `json:"results"`
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// result converts the reply into segments: one per utterance, speakers
// numbered from 1. Without utterances the first channel's transcript becomes
// a single segment.
func (r deepgramResponse) result(model string) Result {
	out := Result{Provider: DeepgramName, Model: model, Duration: seconds(r.Metadata.Duration)}
	if len(r.Results.Channels) > 0 {
		out.Language = r.Results.Channels[0].DetectedLanguage
	}
	for _, u := range r.Results.Utterances {
		text := strings.TrimSpace(u.Transcript)
		if text == "" {
			continue
		}
		seg := Segment{Start: seconds(u.Start), Text: text}
		if u.Speaker != nil {
			seg.Speaker = "Speaker " + strconv.Itoa(*u.Speaker+1)
		}
		out.Segments = append(out.Segments, seg)
	}
	if len(out.Segments) == 0 && len(r.Results.Channels) > 0 && len(r.Results.Channels[0].Alternatives) > 0 {
		if text := strings.TrimSpace(r.Results.Channels[0].Alternatives[0].Transcript); text != "" {
			out.Segments = []Segment{{Text: text}}
		}
	}
	return out
}

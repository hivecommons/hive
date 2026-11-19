package transcribe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultGeminiModel is the Gemini model used when none is set.
	DefaultGeminiModel = "gemini-3.5-flash"
	// GeminiHost is the Gemini API host (egress allowlists).
	GeminiHost = "generativelanguage.googleapis.com"
	// DefaultGeminiBaseURL is the Gemini API root.
	DefaultGeminiBaseURL = "https://" + GeminiHost
	// geminiKeyEnvList names the hive's existing Gemini credential variables,
	// in the order the dashboard's model discovery reads them.
	geminiKeyEnvList = "GEMINI_API_KEY, GOOGLE_API_KEY, GOOGLE_GENAI_API_KEY"
	// geminiInlineMaxBytes: recordings up to this size are sent inline
	// (base64 grows them by a third; the request limit is 20 MB), larger
	// ones go through the Files API.
	geminiInlineMaxBytes = 14 << 20
	// GeminiMaxBytes caps what is sent to Gemini at all. The recording is
	// held in memory for the request, so the cap is far below the Files
	// API's 2 GB; the Inbox max_bytes applies first.
	GeminiMaxBytes = 256 << 20
	// GeminiMaxDuration is the longest audio one prompt may carry.
	GeminiMaxDuration = 9*time.Hour + 30*time.Minute
	// DefaultGeminiTimeout bounds one transcription (upload + generation).
	DefaultGeminiTimeout = 20 * time.Minute
	// geminiFilePollInterval / geminiFilePollMax wait for an uploaded file
	// to become ACTIVE.
	geminiFilePollInterval = 2 * time.Second
	geminiFilePollMax      = 5 * time.Minute
	// geminiMaxResponseBytes bounds a reply read into memory.
	geminiMaxResponseBytes = 32 << 20
	// geminiMaxErrorBytes bounds how much of an error body is quoted.
	geminiMaxErrorBytes = 512
	// geminiMaxRetryAfter caps a Retry-After the hive will honour.
	geminiMaxRetryAfter = 2 * time.Minute
	// geminiFileStateActive is the Files API state of a usable file.
	geminiFileStateActive = "ACTIVE"
	geminiFileStateFailed = "FAILED"
)

// geminiPrompt asks for a verbatim transcript as JSON segments. It is a chat
// model, so the prompt forbids invented speech; the result is still labelled
// a machine transcript and its timestamps are approximate.
const geminiPrompt = `Transcribe this audio recording verbatim.
Return JSON only, in exactly this shape:
{"language": "<BCP-47 code of the main spoken language>", "segments": [{"start": "MM:SS", "speaker": "Speaker 1", "text": "<what was said>"}]}
Rules:
- One segment per utterance or speaker turn, in order, with the start time as MM:SS (H:MM:SS past an hour).
- Label speakers "Speaker 1", "Speaker 2", ... by order of first appearance; use "" when you cannot tell speakers apart.
- Write only words that are actually spoken. Do not summarise, translate, correct or add anything.
- Do not invent speech during silence, music or noise; leave such parts out.
- If nothing is spoken, return {"language": "", "segments": []}.`

// Gemini transcribes through the Gemini API generateContent call with the
// hive's own Gemini API key. Audio is sent inline, or through the Files API
// (deleted again after the run) when it is large.
type Gemini struct {
	// APIKey is sent only in the x-goog-api-key header; never logged.
	APIKey string
	// Model defaults to DefaultGeminiModel.
	Model string
	// Diarize asks for speaker labels.
	Diarize bool
	// Language is a hint; "" or "auto" lets the model detect it.
	Language string
	// BaseURL defaults to DefaultGeminiBaseURL (tests point it at httptest).
	BaseURL string
	// Client defaults to http.DefaultClient.
	Client *http.Client
	// Timeout defaults to DefaultGeminiTimeout.
	Timeout time.Duration
	// PollInterval overrides geminiFilePollInterval (tests).
	PollInterval time.Duration
}

// Name implements Transcriber.
func (g *Gemini) Name() string { return GeminiName }

func (g *Gemini) model() string {
	if m := strings.TrimSpace(g.Model); m != "" {
		return m
	}
	return DefaultGeminiModel
}

func (g *Gemini) base() string {
	if b := strings.TrimRight(g.BaseURL, "/"); b != "" {
		return b
	}
	return DefaultGeminiBaseURL
}

func (g *Gemini) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return http.DefaultClient
}

// Transcribe implements Transcriber.
func (g *Gemini) Transcribe(ctx context.Context, a Audio) (Result, error) {
	if strings.TrimSpace(g.APIKey) == "" {
		return Result{}, fmt.Errorf("%w: set one of %s on this hive (the hive's own Gemini credentials)", ErrNoKey, geminiKeyEnvList)
	}
	if a.Open == nil {
		return Result{}, errors.New("gemini: no audio")
	}
	if a.Duration > GeminiMaxDuration {
		return Result{}, fmt.Errorf("gemini: the recording is longer than %s", GeminiMaxDuration)
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultGeminiTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	a.ReportPhase(PhaseDownloading)
	rc, err := a.Open(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("gemini: open audio: %w", err)
	}
	data, err := io.ReadAll(LimitReader(rc, GeminiMaxBytes))
	_ = rc.Close()
	if errors.Is(err, ErrTooLarge) {
		return Result{}, ErrTooLarge
	}
	if err != nil {
		return Result{}, fmt.Errorf("gemini: read audio: %w", err)
	}
	if len(data) == 0 {
		return Result{}, errors.New("gemini: the recording is empty")
	}
	mime := geminiMIME(a.MIME, data)
	a.ReportPhase(PhaseTranscribing)
	var media map[string]any
	if len(data) <= geminiInlineMaxBytes {
		media = map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": base64.StdEncoding.EncodeToString(data)}}
	} else {
		file, err := g.upload(ctx, data, mime)
		if err != nil {
			return Result{}, err
		}
		defer g.deleteFile(file.Name)
		media = map[string]any{"fileData": map[string]any{"mimeType": mime, "fileUri": file.URI}}
	}
	text, err := g.generate(ctx, media, a.Lang)
	if err != nil {
		return Result{}, err
	}
	a.ReportPhase(PhaseFinalizing)
	res := ParseGeminiTranscript(text, g.Diarize)
	res.Model = g.model()
	if a.Duration > 0 {
		res.Duration = a.Duration
	}
	return res, nil
}

// prompt adds the language and speaker hints to geminiPrompt.
func (g *Gemini) prompt(lang string) string {
	p := geminiPrompt
	lang = strings.TrimSpace(lang)
	if lang == "" {
		lang = strings.TrimSpace(g.Language)
	}
	if lang != "" && !strings.EqualFold(lang, "auto") {
		p += "\n- The recording is expected to be in " + lang + "."
	}
	if !g.Diarize {
		p += "\n- Do not label speakers: use \"\" for every speaker."
	}
	return p
}

// generate calls generateContent and returns the reply text.
func (g *Gemini) generate(ctx context.Context, media map[string]any, lang string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": g.prompt(lang)}, media}}},
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
			"temperature":      0,
		},
	})
	url := g.base() + "/v1beta/models/" + g.model() + ":generateContent"
	resp, err := g.do(ctx, http.MethodPost, url, bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, geminiMaxResponseBytes)).Decode(&parsed); err != nil {
		return "", fmt.Errorf("gemini: decode response: %w", err)
	}
	if parsed.PromptFeedback.BlockReason != "" {
		return "", fmt.Errorf("gemini: the request was blocked (%s)", parsed.PromptFeedback.BlockReason)
	}
	if len(parsed.Candidates) == 0 {
		return "", errors.New("gemini: the model returned no answer")
	}
	var b strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// geminiFile is the part of a Files API file the hive reads.
type geminiFile struct {
	Name  string `json:"name"`
	URI   string `json:"uri"`
	State string `json:"state"`
}

// upload sends data through the Files API resumable upload and waits for
// the file to become ACTIVE.
func (g *Gemini) upload(ctx context.Context, data []byte, mime string) (geminiFile, error) {
	start, _ := json.Marshal(map[string]any{"file": map[string]any{"displayName": "hive-inbox-recording"}})
	resp, err := g.do(ctx, http.MethodPost, g.base()+"/upload/v1beta/files", bytes.NewReader(start), map[string]string{
		"Content-Type":                        "application/json",
		"X-Goog-Upload-Protocol":              "resumable",
		"X-Goog-Upload-Command":               "start",
		"X-Goog-Upload-Header-Content-Length": strconv.Itoa(len(data)),
		"X-Goog-Upload-Header-Content-Type":   mime,
	})
	if err != nil {
		return geminiFile{}, err
	}
	uploadURL := resp.Header.Get("X-Goog-Upload-Url")
	_ = resp.Body.Close()
	if uploadURL == "" {
		return geminiFile{}, errors.New("gemini: the Files API returned no upload URL")
	}
	resp, err = g.do(ctx, http.MethodPost, uploadURL, bytes.NewReader(data), map[string]string{
		"X-Goog-Upload-Offset":  "0",
		"X-Goog-Upload-Command": "upload, finalize",
	})
	if err != nil {
		return geminiFile{}, err
	}
	var up struct {
		File geminiFile `json:"file"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, geminiMaxResponseBytes)).Decode(&up)
	_ = resp.Body.Close()
	if err != nil || up.File.Name == "" || up.File.URI == "" {
		return geminiFile{}, fmt.Errorf("gemini: unexpected Files API reply")
	}
	interval := g.PollInterval
	if interval <= 0 {
		interval = geminiFilePollInterval
	}
	deadline := time.Now().Add(geminiFilePollMax)
	f := up.File
	for f.State != "" && f.State != geminiFileStateActive {
		if f.State == geminiFileStateFailed || time.Now().After(deadline) {
			g.deleteFile(f.Name)
			return geminiFile{}, fmt.Errorf("gemini: the uploaded audio did not become usable (state %s)", f.State)
		}
		select {
		case <-ctx.Done():
			g.deleteFile(f.Name)
			return geminiFile{}, ctx.Err()
		case <-time.After(interval):
		}
		resp, err := g.do(ctx, http.MethodGet, g.base()+"/v1beta/"+f.Name, nil, nil)
		if err != nil {
			g.deleteFile(f.Name)
			return geminiFile{}, err
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, geminiMaxResponseBytes)).Decode(&f)
		_ = resp.Body.Close()
		if err != nil {
			g.deleteFile(f.Name)
			return geminiFile{}, fmt.Errorf("gemini: decode file state: %w", err)
		}
	}
	return f, nil
}

// deleteFile removes an uploaded recording right away rather than leaving
// it for the Files API's 48-hour expiry. Best effort.
func (g *Gemini) deleteFile(name string) {
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if resp, err := g.do(ctx, http.MethodDelete, g.base()+"/v1beta/"+name, nil, nil); err == nil {
		_ = resp.Body.Close()
	}
}

// do sends one authenticated request and maps errors. 429 and 5xx are
// retryable (the Retry wrapper backs off), honouring Retry-After within a
// cap; 401/403 are a bad key. The key is never quoted.
func (g *Gemini) do(ctx context.Context, method, url string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("gemini: build request: %w", err)
	}
	req.Header.Set("x-goog-api-key", g.APIKey)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := g.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &RetryableError{Err: fmt.Errorf("gemini: request timed out or was cancelled: %w", ctx.Err())}
		}
		return nil, &RetryableError{Err: fmt.Errorf("gemini: request to %s failed: %v", GeminiHost, g.redact(unwrapURLError(err).Error()))}
	}
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return resp, nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, geminiMaxErrorBytes))
	_ = resp.Body.Close()
	detail := g.redact(strings.TrimSpace(string(snippet)))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w (HTTP %d): check the hive's Gemini key (%s)", ErrUnauthorized, resp.StatusCode, geminiKeyEnvList)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		if wait := retryAfter(resp.Header.Get("Retry-After")); wait > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
		return nil, &RetryableError{Err: fmt.Errorf("gemini: HTTP %d: %s", resp.StatusCode, detail)}
	}
	return nil, fmt.Errorf("gemini: HTTP %d: %s", resp.StatusCode, detail)
}

func (g *Gemini) redact(s string) string {
	if g.APIKey == "" {
		return s
	}
	return strings.ReplaceAll(s, g.APIKey, "[redacted]")
}

// retryAfter parses a Retry-After in seconds, capped.
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return min(time.Duration(n)*time.Second, geminiMaxRetryAfter)
}

// geminiMIME picks a Gemini audio MIME type from the hint or the bytes.
func geminiMIME(hint string, data []byte) string {
	switch h := strings.ToLower(strings.TrimSpace(hint)); h {
	case "audio/mpeg", "audio/mp3", "audio/wav", "audio/ogg", "audio/flac", "audio/aac", "audio/webm", "audio/opus", "audio/aiff":
		return h
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return "audio/m4a"
	}
	if ct := http.DetectContentType(data); strings.HasPrefix(ct, "audio/") {
		return ct
	}
	return "audio/mpeg"
}

// geminiTranscript is the JSON shape the prompt asks for.
type geminiTranscript struct {
	Language string `json:"language"`
	Segments []struct {
		Start   string `json:"start"`
		Speaker string `json:"speaker"`
		Text    string `json:"text"`
	} `json:"segments"`
}

// jsonFenceRe strips a ```json fence a model may wrap the reply in.
var jsonFenceRe = regexp.MustCompile("(?s)^\\s*```(?:json)?\\s*(.*?)\\s*```\\s*$")

// ParseGeminiTranscript validates the model's JSON reply into segments. A
// reply that is not the expected JSON falls back to one plain-text segment,
// so nothing the model said is lost.
func ParseGeminiTranscript(reply string, diarize bool) Result {
	out := Result{Provider: GeminiName}
	text := strings.TrimSpace(reply)
	if m := jsonFenceRe.FindStringSubmatch(text); m != nil {
		text = m[1]
	}
	var parsed geminiTranscript
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		if t := strings.TrimSpace(reply); t != "" {
			out.Segments = []Segment{{Text: t}}
		}
		return out
	}
	out.Language = strings.TrimSpace(parsed.Language)
	for _, s := range parsed.Segments {
		t := strings.TrimSpace(s.Text)
		if t == "" {
			continue
		}
		seg := Segment{Start: parseClock(s.Start), Text: t}
		if diarize {
			seg.Speaker = strings.TrimSpace(s.Speaker)
		}
		out.Segments = append(out.Segments, seg)
	}
	return out
}

// parseClock reads MM:SS or H:MM:SS (fractions ignored); 0 when malformed.
func parseClock(v string) time.Duration {
	v, _, _ = strings.Cut(strings.TrimSpace(v), ".")
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + n
	}
	return time.Duration(total) * time.Second
}

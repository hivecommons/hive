package transcribe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const geminiTestKey = "AIza-test-secret-key"

// fakeGemini serves generateContent and the Files API, recording requests.
type fakeGemini struct {
	mu       sync.Mutex
	reply    string
	status   []int // statuses for successive generateContent calls (then 200)
	calls    int
	inline   []byte
	fileUsed string
	deleted  []string
	polls    int
	keys     []string
	prompt   string
}

// serve starts the fake and returns its base URL.
func (f *fakeGemini) serve(t *testing.T) string {
	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("POST /v1beta/models/{model}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.keys = append(f.keys, r.Header.Get("x-goog-api-key"))
		f.calls++
		if len(f.status) >= f.calls {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(f.status[f.calls-1])
			_, _ = io.WriteString(w, `{"error":{"message":"quota for key `+geminiTestKey+`"}}`)
			return
		}
		if r.PathValue("model") != "gemini-test:generateContent" {
			t.Errorf("model path = %s", r.URL.Path)
		}
		var body struct {
			Contents []struct {
				Parts []struct {
					Text       string `json:"text"`
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
					FileData *struct {
						FileURI string `json:"fileUri"`
					} `json:"fileData"`
				} `json:"parts"`
			} `json:"contents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, p := range body.Contents[0].Parts {
			if p.Text != "" {
				f.prompt = p.Text
			}
			if p.InlineData != nil {
				f.inline, _ = base64.StdEncoding.DecodeString(p.InlineData.Data)
			}
			if p.FileData != nil {
				f.fileUsed = p.FileData.FileURI
			}
		}
		reply, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": f.reply}}}}}})
		_, _ = w.Write(reply)
	})
	mux.HandleFunc("POST /upload/v1beta/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Upload-Command") != "start" || r.Header.Get("X-Goog-Upload-Header-Content-Length") == "" {
			t.Errorf("bad upload start headers: %v", r.Header)
		}
		w.Header().Set("X-Goog-Upload-Url", srvURL+"/upload-session/1")
	})
	mux.HandleFunc("POST /upload-session/1", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		if n == 0 || r.Header.Get("X-Goog-Upload-Command") != "upload, finalize" {
			t.Errorf("bad upload: %d bytes, %v", n, r.Header)
		}
		_, _ = io.WriteString(w, `{"file":{"name":"files/abc","uri":"https://files.example/abc","state":"PROCESSING"}}`)
	})
	mux.HandleFunc("GET /v1beta/files/abc", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.polls++
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"name":"files/abc","uri":"https://files.example/abc","state":"ACTIVE"}`)
	})
	mux.HandleFunc("DELETE /v1beta/files/abc", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, "files/abc")
		f.mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	srvURL = srv.URL
	return srv.URL
}

// snap copies what the fake recorded, under its lock.
type geminiSnap struct {
	inline   []byte
	fileUsed string
	deleted  []string
	polls    int
	keys     []string
	prompt   string
}

func (f *fakeGemini) snap() geminiSnap {
	f.mu.Lock()
	defer f.mu.Unlock()
	return geminiSnap{inline: f.inline, fileUsed: f.fileUsed, deleted: append([]string{}, f.deleted...), polls: f.polls, keys: append([]string{}, f.keys...), prompt: f.prompt}
}

// setStatus replaces the scripted generateContent statuses.
func (f *fakeGemini) setStatus(st ...int) {
	f.mu.Lock()
	f.status, f.calls = st, 0
	f.mu.Unlock()
}

func newFakeGemini(t *testing.T, reply string) (*fakeGemini, *Gemini) {
	t.Helper()
	f := &fakeGemini{reply: reply}
	return f, &Gemini{APIKey: geminiTestKey, Model: "gemini-test", Diarize: true, BaseURL: f.serve(t), PollInterval: time.Millisecond}
}

const geminiGoodReply = `{"language":"en","segments":[{"start":"00:03","speaker":"Speaker 1","text":"Hello hive."},{"start":"1:02:05","speaker":"Speaker 2","text":"Long meeting."},{"start":"00:09","speaker":"Speaker 1","text":"  "}]}`

func bytesAudio(b []byte, mime string) Audio {
	return Audio{Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }, MIME: mime, Duration: time.Minute, Lang: "en"}
}

func TestGeminiInlineTranscript(t *testing.T) {
	f, g := newFakeGemini(t, "```json\n"+geminiGoodReply+"\n```")
	var phases []string
	a := bytesAudio([]byte("ID3 fake mp3"), "audio/mpeg")
	a.Progress = func(p Progress) { phases = append(phases, p.Phase) }
	res, err := g.Transcribe(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[00:03] Speaker 1: Hello hive.\n[1:02:05] Speaker 2: Long meeting."; res.Text() != want {
		t.Fatalf("text = %q, want %q", res.Text(), want)
	}
	if res.Provider != GeminiName || res.Model != "gemini-test" || res.Language != "en" || res.Duration != time.Minute {
		t.Fatalf("result = %+v", res)
	}
	got := f.snap()
	if string(got.inline) != "ID3 fake mp3" || got.fileUsed != "" {
		t.Fatalf("inline = %q file = %q", got.inline, got.fileUsed)
	}
	if !strings.Contains(got.prompt, "Do not invent speech during silence") || !strings.Contains(got.prompt, "expected to be in en") {
		t.Fatalf("prompt = %q", got.prompt)
	}
	if strings.Join(phases, ",") != "downloading,transcribing,finalizing" {
		t.Fatalf("phases = %v", phases)
	}
	if got.keys[0] != geminiTestKey {
		t.Fatal("the key goes in the x-goog-api-key header")
	}
}

func TestGeminiLargeAudioUsesFilesAPIAndDeletesIt(t *testing.T) {
	f, g := newFakeGemini(t, geminiGoodReply)
	big := bytes.Repeat([]byte{1}, geminiInlineMaxBytes+1)
	if _, err := g.Transcribe(context.Background(), bytesAudio(big, "audio/mp4")); err != nil {
		t.Fatal(err)
	}
	got := f.snap()
	if got.fileUsed != "https://files.example/abc" || got.inline != nil || got.polls == 0 || len(got.deleted) != 1 {
		t.Fatalf("files api: used=%q polls=%d deleted=%v", got.fileUsed, got.polls, got.deleted)
	}
}

func TestGeminiErrorsNeverQuoteTheKey(t *testing.T) {
	f, g := newFakeGemini(t, geminiGoodReply)
	f.setStatus(http.StatusTooManyRequests)
	_, err := g.Transcribe(context.Background(), bytesAudio([]byte("a"), ""))
	if !IsRetryable(err) || strings.Contains(err.Error(), geminiTestKey) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("429 = %v", err)
	}
	// Through the Retry wrapper the second attempt succeeds.
	f.setStatus(http.StatusServiceUnavailable)
	res, err := Retry(g, 2, func(int) time.Duration { return 0 }).Transcribe(context.Background(), bytesAudio([]byte("a"), ""))
	if err != nil || len(res.Segments) != 2 {
		t.Fatalf("retry = %+v %v", res, err)
	}
	f.setStatus(http.StatusForbidden)
	_, err = g.Transcribe(context.Background(), bytesAudio([]byte("a"), ""))
	if !errors.Is(err, ErrUnauthorized) || IsRetryable(err) || strings.Contains(err.Error(), geminiTestKey) || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("403 = %v", err)
	}
	f.setStatus(http.StatusBadRequest)
	if _, err = g.Transcribe(context.Background(), bytesAudio([]byte("a"), "")); err == nil || IsRetryable(err) || strings.Contains(err.Error(), geminiTestKey) {
		t.Fatalf("400 = %v", err)
	}
	if _, err := (&Gemini{}).Transcribe(context.Background(), bytesAudio([]byte("a"), "")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key = %v", err)
	}
	long := bytesAudio([]byte("a"), "")
	long.Duration = GeminiMaxDuration + time.Minute
	if _, err := g.Transcribe(context.Background(), long); err == nil {
		t.Fatal("over-long audio must be refused before upload")
	}
	if _, err := g.Transcribe(context.Background(), bytesAudio(nil, "")); err == nil {
		t.Fatal("empty audio must fail")
	}
}

func TestParseGeminiTranscriptFallbacks(t *testing.T) {
	res := ParseGeminiTranscript("Just some words, no JSON.", true)
	if len(res.Segments) != 1 || res.Segments[0].Text != "Just some words, no JSON." || res.Segments[0].Start != 0 {
		t.Fatalf("plain text fallback = %+v", res)
	}
	res = ParseGeminiTranscript(`{"language":"de","segments":[]}`, true)
	if len(res.Segments) != 0 || res.Language != "de" {
		t.Fatalf("silence = %+v", res)
	}
	res = ParseGeminiTranscript(geminiGoodReply, false)
	if res.Segments[0].Speaker != "" {
		t.Fatal("diarize off drops speaker labels")
	}
	for in, want := range map[string]time.Duration{"00:03": 3 * time.Second, "1:02:05": time.Hour + 2*time.Minute + 5*time.Second, "02:10.5": 130 * time.Second, "bad": 0, "1:2:3:4": 0, "-1:00": 0} {
		if got := parseClock(in); got != want {
			t.Errorf("parseClock(%q) = %v, want %v", in, got, want)
		}
	}
	for hint, want := range map[string]string{"audio/mp4": "audio/m4a", "audio/ogg": "audio/ogg", "": "audio/mpeg"} {
		if got := geminiMIME(hint, []byte("xx")); got != want {
			t.Errorf("geminiMIME(%q) = %q", hint, got)
		}
	}
	if retryAfter("5") != 5*time.Second || retryAfter("9999") != geminiMaxRetryAfter || retryAfter("soon") != 0 {
		t.Fatal("retryAfter")
	}
	if (&Gemini{}).model() != DefaultGeminiModel || (&Gemini{}).Name() != GeminiName {
		t.Fatal("defaults")
	}
}

// rawGemini serves fixed bodies per path, for the error branches.
func rawGemini(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request)) *Gemini {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Gemini{APIKey: geminiTestKey, Model: "gemini-test", BaseURL: srv.URL, PollInterval: time.Millisecond}
}

func body(s string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, s) }
}

func TestGeminiReplyAndUploadFailures(t *testing.T) {
	ctx := context.Background()
	small := bytesAudio([]byte("a"), "audio/ogg")
	for name, reply := range map[string]string{
		"blocked":   `{"promptFeedback":{"blockReason":"SAFETY"}}`,
		"no answer": `{"candidates":[]}`,
		"not json":  `<html>`,
	} {
		g := rawGemini(t, map[string]func(http.ResponseWriter, *http.Request){"POST /v1beta/models/{m}": body(reply)})
		if _, err := g.Transcribe(ctx, small); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	big := bytesAudio(bytes.Repeat([]byte{2}, geminiInlineMaxBytes+1), "audio/webm")
	noURL := rawGemini(t, map[string]func(http.ResponseWriter, *http.Request){"POST /upload/v1beta/files": body("")})
	if _, err := noURL.Transcribe(ctx, big); err == nil || !strings.Contains(err.Error(), "no upload URL") {
		t.Fatalf("no upload url = %v", err)
	}

	var deleted int
	var mu sync.Mutex
	var g *Gemini
	g = rawGemini(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /upload/v1beta/files": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Goog-Upload-Url", g.BaseURL+"/session")
		},
		"POST /session":          body(`{"file":{"name":"files/x","uri":"u","state":"PROCESSING"}}`),
		"GET /v1beta/files/x":    body(`{"name":"files/x","uri":"u","state":"FAILED"}`),
		"DELETE /v1beta/files/x": func(http.ResponseWriter, *http.Request) { mu.Lock(); deleted++; mu.Unlock() },
	})
	if _, err := g.Transcribe(ctx, big); err == nil || !strings.Contains(err.Error(), "did not become usable") {
		t.Fatalf("failed file = %v", err)
	}
	mu.Lock()
	if deleted == 0 {
		t.Fatal("a failed upload must be deleted")
	}
	mu.Unlock()

	bad := rawGemini(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /upload/v1beta/files": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Goog-Upload-Url", "http://127.0.0.1:1/none")
		},
	})
	if _, err := bad.Transcribe(ctx, big); err == nil || !IsRetryable(err) || strings.Contains(err.Error(), geminiTestKey) {
		t.Fatalf("unreachable upload = %v", err)
	}
	openErr := Audio{Open: func(context.Context) (io.ReadCloser, error) { return nil, errors.New("boom") }}
	if _, err := g.Transcribe(ctx, openErr); err == nil {
		t.Fatal("open error must fail")
	}
	if _, err := g.Transcribe(ctx, Audio{}); err == nil {
		t.Fatal("no audio must fail")
	}
}

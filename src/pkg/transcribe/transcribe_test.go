package transcribe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// deepgramReply is a trimmed real-shape /v1/listen response with
// diarized utterances.
const deepgramReply = `{
  "metadata": {"duration": 3725.5, "models": ["nova-3"]},
  "results": {
    "channels": [{"detected_language": "en", "alternatives": [{"transcript": "hello hive. ship it."}]}],
    "utterances": [
      {"start": 0.4, "end": 1.2, "speaker": 0, "transcript": "Hello hive."},
      {"start": 2.0, "end": 2.5, "speaker": 1, "transcript": "  "},
      {"start": 3725.0, "end": 3726.0, "speaker": 1, "transcript": "Ship it."}
    ]
  }
}`

func audioOf(data []byte) Audio {
	return Audio{
		MIME: "audio/mpeg",
		Size: int64(len(data)),
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
}

type capture struct {
	query  map[string]string
	auth   string
	ctype  string
	body   []byte
	status int
	reply  string
	calls  atomic.Int32
}

func (c *capture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls.Add(1)
		if r.URL.Path != "/v1/listen" || r.Method != http.MethodPost {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		c.query = map[string]string{}
		for k, v := range r.URL.Query() {
			c.query[k] = v[0]
		}
		c.auth = r.Header.Get("Authorization")
		c.ctype = r.Header.Get("Content-Type")
		c.body, _ = io.ReadAll(r.Body)
		if c.status != 0 {
			w.WriteHeader(c.status)
		}
		_, _ = io.WriteString(w, c.reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDeepgramRequestParamsAndDiarizedSegments(t *testing.T) {
	c := &capture{reply: deepgramReply}
	srv := c.server(t)
	d := &Deepgram{APIKey: "dg-secret", Diarize: true, BaseURL: srv.URL + "/"}
	if d.Name() != DeepgramName {
		t.Fatalf("name = %q", d.Name())
	}
	res, err := d.Transcribe(context.Background(), audioOf([]byte("RIFFaudio")))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"model": "nova-3", "diarize": "true", "smart_format": "true", "utterances": "true", "mip_opt_out": "true", "detect_language": "true"}
	for k, v := range want {
		if c.query[k] != v {
			t.Errorf("query %s = %q, want %q (all: %v)", k, c.query[k], v, c.query)
		}
	}
	if _, ok := c.query["language"]; ok {
		t.Errorf("language must not be sent in auto mode: %v", c.query)
	}
	if c.auth != "Token dg-secret" || c.ctype != "audio/mpeg" || string(c.body) != "RIFFaudio" {
		t.Fatalf("auth=%q ctype=%q body=%q", c.auth, c.ctype, c.body)
	}
	if res.Provider != "deepgram" || res.Model != "nova-3" || res.Language != "en" || res.Duration != 3725500*time.Millisecond {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Segments) != 2 || res.Segments[0].Speaker != "Speaker 1" || res.Segments[1].Speaker != "Speaker 2" {
		t.Fatalf("segments = %+v", res.Segments)
	}
	if got, want := res.Text(), "[00:00] Speaker 1: Hello hive.\n[1:02:05] Speaker 2: Ship it."; got != want {
		t.Fatalf("rendered = %q, want %q", got, want)
	}
}

func TestDeepgramExplicitLanguageNoDiarizeAndChannelFallback(t *testing.T) {
	c := &capture{reply: `{"metadata":{"duration":2},"results":{"channels":[{"alternatives":[{"transcript":" just words "}]}]}}`}
	srv := c.server(t)
	d := &Deepgram{APIKey: "k", Model: "nova-2", Language: "auto", BaseURL: srv.URL}
	a := audioOf([]byte("x"))
	a.MIME = ""
	a.Lang = "de"
	res, err := d.Transcribe(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if c.query["language"] != "de" || c.query["diarize"] != "false" || c.query["model"] != "nova-2" || c.query["mip_opt_out"] != "true" {
		t.Fatalf("query = %v", c.query)
	}
	if _, ok := c.query["detect_language"]; ok {
		t.Fatalf("detect_language sent with an explicit language: %v", c.query)
	}
	if c.ctype != "application/octet-stream" {
		t.Fatalf("content type = %q", c.ctype)
	}
	if len(res.Segments) != 1 || res.Segments[0].Text != "just words" || res.Segments[0].Speaker != "" {
		t.Fatalf("segments = %+v", res.Segments)
	}
	if res.Text() != "[00:00] just words" {
		t.Fatalf("text = %q", res.Text())
	}
	// An empty reply yields no segments, not a crash.
	c.reply = `{}`
	res, err = d.Transcribe(context.Background(), audioOf([]byte("x")))
	if err != nil || len(res.Segments) != 0 || res.Text() != "" {
		t.Fatalf("empty reply = %+v, %v", res, err)
	}
}

func TestDeepgramUnauthorizedNamesEnvVarNeverKey(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		c := &capture{status: status, reply: `{"err_msg":"Invalid credentials. key=dg-very-secret"}`}
		srv := c.server(t)
		d := &Deepgram{APIKey: "dg-very-secret", KeyEnv: "MY_DG_KEY", BaseURL: srv.URL}
		_, err := d.Transcribe(context.Background(), audioOf([]byte("x")))
		if !errors.Is(err, ErrUnauthorized) || IsRetryable(err) {
			t.Fatalf("%d: err = %v", status, err)
		}
		if !strings.Contains(err.Error(), "$MY_DG_KEY") || strings.Contains(err.Error(), "dg-very-secret") {
			t.Fatalf("%d: error must name the env var and never the key: %v", status, err)
		}
	}
	// The default env var name is used when KeyEnv is unset.
	c := &capture{status: http.StatusUnauthorized}
	srv := c.server(t)
	_, err := (&Deepgram{APIKey: "k", BaseURL: srv.URL}).Transcribe(context.Background(), audioOf([]byte("x")))
	if err == nil || !strings.Contains(err.Error(), "$DEEPGRAM_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeepgramStatusMapping(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		c := &capture{status: tc.status, reply: "upstream says dg-k no"}
		srv := c.server(t)
		_, err := (&Deepgram{APIKey: "dg-k", BaseURL: srv.URL}).Transcribe(context.Background(), audioOf([]byte("x")))
		if err == nil || IsRetryable(err) != tc.retryable {
			t.Fatalf("%d: err = %v retryable=%v", tc.status, err, IsRetryable(err))
		}
		if strings.Contains(err.Error(), "dg-k ") || !strings.Contains(err.Error(), "[redacted]") {
			t.Fatalf("%d: key must be redacted from the quoted body: %v", tc.status, err)
		}
	}
}

func TestDeepgram429RetriedThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	opens := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "audio" {
			t.Errorf("attempt %d body = %q (each attempt must re-read from the start)", calls.Load(), body)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, deepgramReply)
	}))
	defer srv.Close()
	a := audioOf([]byte("audio"))
	inner := a.Open
	a.Open = func(ctx context.Context) (io.ReadCloser, error) { opens++; return inner(ctx) }
	var waits []int
	tr := Retry(&Deepgram{APIKey: "k", Diarize: true, BaseURL: srv.URL}, 3, func(n int) time.Duration { waits = append(waits, n); return 0 })
	if tr.Name() != DeepgramName {
		t.Fatalf("name = %q", tr.Name())
	}
	res, err := tr.Transcribe(context.Background(), a)
	if err != nil || len(res.Segments) != 2 {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if calls.Load() != 2 || opens != 2 || len(waits) != 1 || waits[0] != 1 {
		t.Fatalf("calls=%d opens=%d waits=%v", calls.Load(), opens, waits)
	}
}

func TestDeepgramTimeoutIsRetryable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	_, err := (&Deepgram{APIKey: "k", BaseURL: srv.URL, Timeout: 50 * time.Millisecond}).Transcribe(context.Background(), audioOf([]byte("x")))
	if err == nil || !IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeepgramOversizeBodyCutByLimitReader(t *testing.T) {
	c := &capture{reply: deepgramReply}
	srv := c.server(t)
	data := bytes.Repeat([]byte("a"), 1<<20)
	a := Audio{Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(LimitReader(bytes.NewReader(data), 1024)), nil
	}}
	_, err := (&Deepgram{APIKey: "k", BaseURL: srv.URL}).Transcribe(context.Background(), a)
	if !errors.Is(err, ErrTooLarge) || IsRetryable(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeepgramLocalFailures(t *testing.T) {
	ctx := context.Background()
	if _, err := (&Deepgram{KeyEnv: "DG"}).Transcribe(ctx, audioOf(nil)); !errors.Is(err, ErrNoKey) || !strings.Contains(err.Error(), "$DG") {
		t.Fatalf("no key = %v", err)
	}
	if _, err := (&Deepgram{APIKey: "k"}).Transcribe(ctx, Audio{}); err == nil {
		t.Fatal("nil Open must fail")
	}
	if _, err := (&Deepgram{APIKey: "k", BaseURL: "http://[::1"}).Transcribe(ctx, audioOf(nil)); err == nil {
		t.Fatal("bad base URL must fail")
	}
	openErr := errors.New("audio gone")
	a := Audio{Open: func(context.Context) (io.ReadCloser, error) { return nil, openErr }}
	if _, err := (&Deepgram{APIKey: "k", BaseURL: "http://127.0.0.1:1"}).Transcribe(ctx, a); !errors.Is(err, openErr) {
		t.Fatalf("open error = %v", err)
	}
	// Bad JSON in a 200 reply.
	c := &capture{reply: "not json"}
	srv := c.server(t)
	if _, err := (&Deepgram{APIKey: "k", BaseURL: srv.URL}).Transcribe(ctx, audioOf([]byte("x"))); err == nil || IsRetryable(err) {
		t.Fatalf("decode error = %v", err)
	}
	// Unreachable host: a retryable transport error that never quotes the URL.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	_, err := (&Deepgram{APIKey: "k", BaseURL: deadURL, Client: &http.Client{}}).Transcribe(ctx, audioOf([]byte("x")))
	if err == nil || !IsRetryable(err) || strings.Contains(err.Error(), deadURL) {
		t.Fatalf("transport error = %v", err)
	}
}

func TestLimitReader(t *testing.T) {
	read := func(r io.Reader) (int, error) {
		b, err := io.ReadAll(r)
		return len(b), err
	}
	if n, err := read(LimitReader(strings.NewReader("abcd"), 4)); n != 4 || err != nil {
		t.Fatalf("exact limit = %d, %v", n, err)
	}
	if n, err := read(LimitReader(strings.NewReader("abc"), 4)); n != 3 || err != nil {
		t.Fatalf("under limit = %d, %v", n, err)
	}
	if _, err := read(LimitReader(strings.NewReader("abcde"), 4)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over limit = %v", err)
	}
	if n, err := read(LimitReader(strings.NewReader("abcde"), 0)); n != 5 || err != nil {
		t.Fatalf("disabled = %d, %v", n, err)
	}
	lr := LimitReader(strings.NewReader("abcdef"), 2)
	buf := make([]byte, 8)
	if _, err := lr.Read(buf); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("first read = %v", err)
	}
	if _, err := lr.Read(buf); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("read after the cut must keep failing, got %v", err)
	}
}

func TestRetryPolicy(t *testing.T) {
	ctx := context.Background()
	transient := &RetryableError{Err: errors.New("busy")}
	if transient.Error() != "busy" || !errors.Is(transient, transient.Err) || IsRetryable(errors.New("x")) {
		t.Fatal("RetryableError basics")
	}
	f := &Fake{Errs: []error{transient, transient, transient, transient}}
	if _, err := Retry(f, 3, nil).Transcribe(ctx, Audio{}); !IsRetryable(err) || f.Calls() != 3 {
		t.Fatalf("exhausted: err=%v calls=%d", err, f.Calls())
	}
	permanent := errors.New("bad audio")
	f = &Fake{Errs: []error{permanent}}
	if _, err := Retry(f, 3, nil).Transcribe(ctx, Audio{}); !errors.Is(err, permanent) || f.Calls() != 1 {
		t.Fatalf("permanent: err=%v calls=%d", err, f.Calls())
	}
	f = &Fake{Errs: []error{transient}}
	if _, err := Retry(f, 0, nil).Transcribe(ctx, Audio{}); !IsRetryable(err) || f.Calls() != 1 {
		t.Fatalf("attempts<1 means one try: err=%v calls=%d", err, f.Calls())
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	f = &Fake{Errs: []error{transient}}
	if _, err := Retry(f, 3, func(int) time.Duration { return time.Hour }).Transcribe(cctx, Audio{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during backoff = %v", err)
	}
	b := ExponentialBackoff(time.Second)
	if b(0) != time.Second || b(1) != time.Second || b(2) != 2*time.Second || b(3) != 4*time.Second {
		t.Fatal("exponential backoff")
	}
}

func TestFakeAdapter(t *testing.T) {
	f := &Fake{Result: Result{Segments: []Segment{{Text: "hi"}}}}
	res, err := f.Transcribe(context.Background(), audioOf([]byte("12345")))
	if err != nil || res.Provider != FakeName || f.Bytes() != 5 || f.Name() != FakeName {
		t.Fatalf("fake = %+v %v bytes=%d", res, err, f.Bytes())
	}
	openErr := errors.New("no")
	if _, err := f.Transcribe(context.Background(), Audio{Open: func(context.Context) (io.ReadCloser, error) { return nil, openErr }}); !errors.Is(err, openErr) {
		t.Fatalf("open err = %v", err)
	}
	big := Audio{Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(LimitReader(strings.NewReader("123456"), 3)), nil
	}}
	if _, err := f.Transcribe(context.Background(), big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("limit err = %v", err)
	}
}

func TestRenderSegmentsAndOffsets(t *testing.T) {
	if FormatOffset(-time.Second) != "00:00" || FormatOffset(65*time.Second) != "01:05" || FormatOffset(time.Hour+2*time.Minute+3*time.Second) != "1:02:03" {
		t.Fatal("FormatOffset")
	}
	got := RenderSegments([]Segment{{Start: 1500 * time.Millisecond, Speaker: "Speaker 1", Text: " a "}, {Text: ""}, {Start: 3 * time.Second, Text: "b"}})
	if got != "[00:02] Speaker 1: a\n[00:03] b" {
		t.Fatalf("render = %q", got)
	}
	var b strings.Builder
	WriteLine(&b, "", "", "plain")
	if b.String() != "plain\n" {
		t.Fatalf("WriteLine = %q", b.String())
	}
}

func TestEstimateCostUSD(t *testing.T) {
	if got := EstimateCostUSD(DeepgramName, DefaultDeepgramModel, 10*time.Minute); math.Abs(got-0.043) > 1e-9 {
		t.Fatalf("cost = %v", got)
	}
	if EstimateCostUSD("other", "m", time.Hour) != 0 {
		t.Fatal("unknown model must estimate 0")
	}
}

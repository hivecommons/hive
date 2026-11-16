package inbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// streamSource is a Source with on-demand audio links.
type streamSource struct {
	link  AudioLink
	err   error
	mints int
}

func (f *streamSource) Type() string                         { return "plaud" }
func (f *streamSource) List(context.Context) ([]Item, error) { return nil, nil }
func (f *streamSource) Fetch(context.Context, Item) (Content, error) {
	return Content{}, nil
}
func (f *streamSource) AudioLink(context.Context, Item) (AudioLink, error) {
	f.mints++
	return f.link, f.err
}

var streamBytes = []byte("OggS-0123456789abcdef")

// newStreamService serves streamBytes behind handler-selected statuses.
func newStreamService(t *testing.T, handler http.HandlerFunc) (*Service, *streamSource) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	src := &streamSource{link: AudioLink{URL: srv.URL + "/a/rec.ogg?sig=topsecret", ExpiresAt: time.Now().Add(time.Hour)}}
	svc := NewService(config.InboxConfig{}, NewStore(filepath.Join(t.TempDir(), "s.json")), []Source{src}, nil, nil)
	svc.AudioClient = srv.Client()
	return svc, src
}

func serveRecording(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "binary/octet-stream")
	http.ServeContent(w, r, "rec.ogg", time.Time{}, bytes.NewReader(streamBytes))
}

func TestOpenAudioStreamRangeAndFallbacks(t *testing.T) {
	ctx := context.Background()
	it := Item{Source: "plaud", ID: "r1"}
	svc, src := newStreamService(t, serveRecording)
	st, err := svc.OpenAudioStream(ctx, it, "bytes=2-5")
	if err != nil || st.Status != http.StatusPartialContent || !st.Minted || st.ContentType != "audio/ogg" || !st.AcceptRanges {
		t.Fatalf("range = %+v %v", st, err)
	}
	got, _ := io.ReadAll(st.Body)
	_ = st.Body.Close()
	if string(got) != string(streamBytes[2:6]) || st.ContentRange == "" {
		t.Fatalf("range body = %q %q", got, st.ContentRange)
	}
	st, err = svc.OpenAudioStream(ctx, it, "bytes=0-1,4-5")
	if err != nil || st.Status != http.StatusOK || st.Minted || src.mints != 1 {
		t.Fatalf("multi-range dropped to a full reply from the cached link: %+v %v mints=%d", st, err, src.mints)
	}
	_ = st.Body.Close()
	st, err = svc.OpenAudioStream(ctx, it, "bytes=9999-")
	if err != nil || st.Status != http.StatusRequestedRangeNotSatisfiable || st.Body != nil {
		t.Fatalf("416 = %+v %v", st, err)
	}

	svc.AudioStreamMaxBytes = 4
	if _, err := svc.OpenAudioStream(ctx, it, ""); !errors.Is(err, ErrAudioTooLarge) {
		t.Fatalf("over the cap = %v", err)
	}
	if _, err := svc.OpenAudioStream(ctx, it, "bytes=0-1"); !errors.Is(err, ErrAudioTooLarge) {
		t.Fatalf("range of an over-cap recording = %v", err)
	}
}

func TestOpenAudioStreamUpstreamErrors(t *testing.T) {
	ctx := context.Background()
	it := Item{Source: "plaud", ID: "r1"}
	for _, tc := range []struct {
		code int
		is   error
		want string
	}{
		{http.StatusForbidden, ErrAudioLinkRefused, "HTTP 403"},
		{http.StatusGone, ErrAudioLinkRefused, "HTTP 410"},
		{http.StatusNotFound, ErrNoAudio, "HTTP 404"},
		{http.StatusInternalServerError, nil, "HTTP 500"},
	} {
		svc, src := newStreamService(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", tc.code) })
		_, err := svc.OpenAudioStream(ctx, it, "")
		if err == nil || (tc.is != nil && !errors.Is(err, tc.is)) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "topsecret") {
			t.Fatalf("HTTP %d = %v", tc.code, err)
		}
		if src.mints != 1 {
			t.Fatalf("HTTP %d: a freshly minted link is not re-minted (%d)", tc.code, src.mints)
		}
	}

	svc, src := newStreamService(t, serveRecording)
	src.link.URL = "ftp://example/rec.ogg?sig=topsecret"
	if _, err := svc.OpenAudioStream(ctx, it, ""); err == nil || !strings.Contains(err.Error(), "unsupported link") {
		t.Fatalf("ftp link = %v", err)
	}
	svc, src = newStreamService(t, serveRecording)
	src.link.URL = "http://127.0.0.1:1/rec.ogg?sig=topsecret"
	if _, err := svc.OpenAudioStream(ctx, it, ""); err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("unreachable storage = %v", err)
	}
	svc, src = newStreamService(t, serveRecording)
	src.err = ErrNoAudio
	if _, err := svc.OpenAudioStream(ctx, it, ""); !errors.Is(err, ErrNoAudio) {
		t.Fatalf("mint failure = %v", err)
	}
	if _, err := svc.OpenAudioStream(ctx, Item{Source: "zoom", ID: "x"}, ""); err == nil {
		t.Fatal("unconfigured source must fail")
	}
}

func TestOpenAudioStreamRemintsARefusedCachedLink(t *testing.T) {
	ctx := context.Background()
	it := Item{Source: "plaud", ID: "r1"}
	var refuse atomic.Bool
	svc, src := newStreamService(t, func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		serveRecording(w, r)
	})
	st, err := svc.OpenAudioStream(ctx, it, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Body.Close()
	refuse.Store(true)
	if _, err := svc.OpenAudioStream(ctx, it, ""); !errors.Is(err, ErrAudioLinkRefused) || src.mints != 2 {
		t.Fatalf("refused cached link = %v mints=%d", err, src.mints)
	}
	refuse.Store(false)
	st, err = svc.OpenAudioStream(ctx, it, "")
	if err != nil || !st.Minted || src.mints != 3 {
		t.Fatalf("after refusal the link is re-minted: %+v %v mints=%d", st, err, src.mints)
	}
	_ = st.Body.Close()
}

func TestSanitizeRange(t *testing.T) {
	for in, want := range map[string]string{
		"bytes=0-":       "bytes=0-",
		" bytes=10-20 ":  "bytes=10-20",
		"bytes=-500":     "bytes=-500",
		"bytes=-":        "",
		"bytes=0-1,5-9":  "",
		"items=0-1":      "",
		"bytes=0-\r\nX:": "",
		"":               "",
	} {
		if got := sanitizeRange(in); got != want {
			t.Errorf("sanitizeRange(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamContentType(t *testing.T) {
	for _, tc := range []struct{ upstream, link, want string }{
		{"audio/mpeg", "https://x/a.bin", "audio/mpeg"},
		{"binary/octet-stream", "https://x/a.ogg?sig=1", "audio/ogg"},
		{"", "https://x/a.m4a", "audio/mp4"},
		{"text/html", "https://x/a", "application/octet-stream"},
		{"video/mp4", "https://x/a", "video/mp4"},
	} {
		if got := streamContentType(tc.upstream, tc.link); got != tc.want {
			t.Errorf("streamContentType(%q, %q) = %q, want %q", tc.upstream, tc.link, got, tc.want)
		}
	}
}

func TestContentRangeTotal(t *testing.T) {
	if got := contentRangeTotal("bytes 0-9/1234"); got != 1234 {
		t.Fatalf("total = %d", got)
	}
	if got := contentRangeTotal("bytes 0-9/*"); got != -1 {
		t.Fatalf("unknown total = %d", got)
	}
}

func TestCappedBodyFailsPastTheCap(t *testing.T) {
	b := &cappedBody{r: io.NopCloser(strings.NewReader("0123456789")), left: 4}
	got, err := io.ReadAll(b)
	if !errors.Is(err, ErrAudioTooLarge) || string(got) != "0123" {
		t.Fatalf("capped read = %q %v", got, err)
	}
	b = &cappedBody{r: io.NopCloser(strings.NewReader("0123")), left: 4}
	if got, err := io.ReadAll(b); err != nil || string(got) != "0123" {
		t.Fatalf("exact read = %q %v", got, err)
	}
}

func TestAudioLinkCacheReuseAndExpiry(t *testing.T) {
	var c audioLinkCache
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	c.put("plaud/a", AudioLink{URL: "u", ExpiresAt: now.Add(24 * time.Hour)}, now)
	if _, ok := c.get("plaud/a", now.Add(time.Minute)); !ok {
		t.Fatal("fresh link not reused")
	}
	if _, ok := c.get("plaud/a", now.Add(audioStreamLinkReuse)); ok {
		t.Fatal("link reused past the reuse window")
	}
	c.put("plaud/b", AudioLink{URL: "u", ExpiresAt: now.Add(30 * time.Second)}, now)
	if _, ok := c.get("plaud/b", now); ok {
		t.Fatal("link reused inside its expiry margin")
	}
	for i := 0; i < audioStreamCacheEntries+1; i++ {
		c.put("k"+strings.Repeat("x", i), AudioLink{URL: "u"}, now)
	}
	if len(c.links) > audioStreamCacheEntries {
		t.Fatalf("cache grew past its bound: %d", len(c.links))
	}
	c.put("plaud/c", AudioLink{URL: "u"}, now)
	c.drop("plaud/c")
	if _, ok := c.get("plaud/c", now); ok {
		t.Fatal("dropped link still cached")
	}
}

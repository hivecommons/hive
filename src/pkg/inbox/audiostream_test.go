package inbox

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

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
	c.put("plaud/c", AudioLink{URL: "u"}, now)
	c.drop("plaud/c")
	if _, ok := c.get("plaud/c", now); ok {
		t.Fatal("dropped link still cached")
	}
}

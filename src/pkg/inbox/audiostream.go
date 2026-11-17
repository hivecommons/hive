package inbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Same-origin playback. A source's presigned audio link points at its cloud
// storage (Plaud: S3), which the dashboard CSP does not allow as a media
// source, and must not: the browser would also learn a bearer link it can
// leak. The hive fetches the link itself and streams the bytes back on its
// own origin instead. Audio is never written to disk and the link is held
// only in memory, never logged (errors name the storage host only).
const (
	// DefaultAudioStreamMaxBytes caps one proxied response (2 GiB: hours of
	// meeting audio, or a long video recording).
	DefaultAudioStreamMaxBytes int64 = 2 << 30
	// audioStreamMintTimeout bounds minting a link (may spawn an MCP process).
	audioStreamMintTimeout = 90 * time.Second
	// audioStreamLinkReuse is how long one minted link serves the Range
	// requests of a playback (seeking) before a fresh one is minted.
	audioStreamLinkReuse = 10 * time.Minute
	// audioStreamLinkMargin retires a link this long before it expires.
	audioStreamLinkMargin = time.Minute
	// audioStreamCacheEntries bounds the in-memory link cache.
	audioStreamCacheEntries = 256
	// bytesPerMiBShift turns a byte count into MiB for messages.
	bytesPerMiBShift = 20
)

var (
	// ErrAudioLinkRefused is the storage refusing the link (expired, revoked
	// or not authorised: HTTP 401/403/410).
	ErrAudioLinkRefused = errors.New("the recording's storage refused the audio link (expired or revoked); press ▶ Play recording again")
	// ErrAudioTooLarge is a recording over the stream size cap.
	ErrAudioTooLarge = errors.New("the recording is larger than this hive streams")
)

// AudioStream is one upstream audio response, ready to copy to the client.
type AudioStream struct {
	// Status is 200 (whole file), 206 (the requested range) or 416.
	Status        int
	ContentType   string
	ContentLength int64
	ContentRange  string
	AcceptRanges  bool
	ETag          string
	LastModified  string
	// Body is nil for a 416. The caller closes it.
	Body io.ReadCloser
	// Minted is true when a fresh link was minted for this response.
	Minted bool
}

// singleByteRange accepts the one Range form media elements send.
var singleByteRange = regexp.MustCompile(`^bytes=\d*-\d*$`)

// sanitizeRange passes a single byte range through and drops anything else
// (multi-range, other units), which upstream then answers with 200.
func sanitizeRange(h string) string {
	h = strings.TrimSpace(h)
	if h == "bytes=-" || !singleByteRange.MatchString(h) {
		return ""
	}
	return h
}

// OpenAudioStream fetches an item's audio through a minted link, passing a
// single byte range through. When upstream ignores the range the whole file
// comes back as 200. A link the storage refuses is re-minted once.
func (s *Service) OpenAudioStream(ctx context.Context, it Item, rangeHeader string) (AudioStream, error) {
	rng := sanitizeRange(rangeHeader)
	link, minted, err := s.streamLink(ctx, it, false)
	if err != nil {
		return AudioStream{}, err
	}
	st, err := s.fetchAudio(ctx, link.URL, rng)
	if errors.Is(err, ErrAudioLinkRefused) && !minted {
		if link, minted, err = s.streamLink(ctx, it, true); err != nil {
			return AudioStream{}, err
		}
		st, err = s.fetchAudio(ctx, link.URL, rng)
	}
	if errors.Is(err, ErrAudioLinkRefused) {
		s.streamLinks.drop(it.Key())
	}
	st.Minted = minted
	return st, err
}

// streamLink returns a cached link for the item or mints a fresh one.
func (s *Service) streamLink(ctx context.Context, it Item, fresh bool) (AudioLink, bool, error) {
	key := it.Key()
	if !fresh {
		if link, ok := s.streamLinks.get(key, s.now()); ok {
			return link, false, nil
		}
	}
	mctx, cancel := context.WithTimeout(ctx, audioStreamMintTimeout)
	defer cancel()
	link, err := s.AudioLink(mctx, it)
	if err != nil {
		s.streamLinks.drop(key)
		return AudioLink{}, false, err
	}
	s.streamLinks.put(key, link, s.now())
	return link, true, nil
}

func (s *Service) audioStreamMaxBytes() int64 {
	if s.AudioStreamMaxBytes > 0 {
		return s.AudioStreamMaxBytes
	}
	return DefaultAudioStreamMaxBytes
}

// fetchAudio GETs the link. Errors name the storage host only, so the signed
// query string never reaches logs, the audit trail or the client.
func (s *Service) fetchAudio(ctx context.Context, link, rng string) (AudioStream, error) {
	host := linkHost(link)
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return AudioStream{}, fmt.Errorf("audio from %s: unsupported link", host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return AudioStream{}, fmt.Errorf("audio from %s: invalid link", host)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	client := s.AudioClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return AudioStream{}, fmt.Errorf("audio from %s: %s", host, redactLink(err.Error(), link))
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusRequestedRangeNotSatisfiable:
		_ = resp.Body.Close()
		return AudioStream{Status: resp.StatusCode, ContentRange: resp.Header.Get("Content-Range")}, nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusGone:
		_ = resp.Body.Close()
		return AudioStream{}, fmt.Errorf("%w (%s: HTTP %d)", ErrAudioLinkRefused, host, resp.StatusCode)
	case http.StatusNotFound:
		_ = resp.Body.Close()
		return AudioStream{}, fmt.Errorf("%w: %s has no such recording (HTTP 404)", ErrNoAudio, host)
	default:
		_ = resp.Body.Close()
		return AudioStream{}, fmt.Errorf("audio from %s: HTTP %d", host, resp.StatusCode)
	}
	maxBytes := s.audioStreamMaxBytes()
	if resp.ContentLength > maxBytes || contentRangeTotal(resp.Header.Get("Content-Range")) > maxBytes {
		_ = resp.Body.Close()
		return AudioStream{}, fmt.Errorf("%w (%d MiB cap)", ErrAudioTooLarge, maxBytes>>bytesPerMiBShift)
	}
	st := AudioStream{
		Status:        resp.StatusCode,
		ContentType:   streamContentType(resp.Header.Get("Content-Type"), link),
		ContentLength: resp.ContentLength,
		AcceptRanges:  resp.StatusCode == http.StatusPartialContent || strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes"),
		ETag:          resp.Header.Get("ETag"),
		LastModified:  resp.Header.Get("Last-Modified"),
		Body:          &cappedBody{r: resp.Body, left: maxBytes},
	}
	if resp.StatusCode == http.StatusPartialContent {
		st.ContentRange = resp.Header.Get("Content-Range")
	}
	return st, nil
}

// contentRangeTotal reads the complete length from "bytes a-b/total"; -1 when
// absent or unknown ("*").
func contentRangeTotal(cr string) int64 {
	i := strings.LastIndexByte(cr, '/')
	if i < 0 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// streamContentType keeps an upstream media type and otherwise derives one
// from the link's extension: object stores often answer binary/octet-stream,
// which a media element may refuse.
func streamContentType(upstream, link string) string {
	if mt, _, err := mime.ParseMediaType(upstream); err == nil &&
		(strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/") || mt == "application/ogg") {
		return upstream
	}
	if m := mimeFromURL(link); m != "" {
		return m
	}
	return "application/octet-stream"
}

// cappedBody fails a read once more than left bytes arrive (an upstream that
// sends more than it declared, or no length at all).
type cappedBody struct {
	r    io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		n, err := b.r.Read(one[:])
		if n > 0 {
			return 0, ErrAudioTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.r.Close() }

// audioLinkCache holds minted links in memory for one playback's Range
// requests. It is never persisted.
type audioLinkCache struct {
	mu    sync.Mutex
	links map[string]cachedAudioLink
}

type cachedAudioLink struct {
	link     AudioLink
	mintedAt time.Time
}

func (c *audioLinkCache) usable(e cachedAudioLink, now time.Time) bool {
	if now.Sub(e.mintedAt) >= audioStreamLinkReuse {
		return false
	}
	return e.link.ExpiresAt.IsZero() || now.Before(e.link.ExpiresAt.Add(-audioStreamLinkMargin))
}

func (c *audioLinkCache) get(key string, now time.Time) (AudioLink, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.links[key]
	if !ok || !c.usable(e, now) {
		delete(c.links, key)
		return AudioLink{}, false
	}
	return e.link, true
}

func (c *audioLinkCache) put(key string, link AudioLink, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.links == nil {
		c.links = map[string]cachedAudioLink{}
	}
	for k, e := range c.links {
		if !c.usable(e, now) {
			delete(c.links, k)
		}
	}
	if len(c.links) >= audioStreamCacheEntries {
		c.links = map[string]cachedAudioLink{}
	}
	c.links[key] = cachedAudioLink{link: link, mintedAt: now}
}

func (c *audioLinkCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.links, key)
}

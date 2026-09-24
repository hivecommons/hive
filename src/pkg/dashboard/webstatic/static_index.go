package webstatic

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// IndexDocument serves the embedded SPA document (static/index.html) with
// compression and revalidation, replacing the bare http.FileServer for the
// root document only.
//
// WHY THIS EXISTS (perf, measured 2026-08-14): the SPA is a single ~1.3 MB
// inline HTML document. http.FileServer over embed.FS served it with
//   - no Content-Encoding (Go's file server never compresses),
//   - no ETag, and
//   - no Last-Modified (embed.FS files have a zero ModTime),
//
// so every dashboard visit re-downloaded the full 1.3 MB uncompressed. None of
// the fleet's edges compress on our behalf (ingress-nginx ships with gzip off,
// the OpenShift HAProxy router never compresses), so the spoke process is the
// only place this can be fixed for every cluster at once. Gzip cuts the
// transfer to ~330 KB (≈4x), and the strong ETag turns repeat visits into a
// 304 with no body at all.
//
// The document is embedded in the binary, so the ETag and gzipped form are
// computed exactly once and are immutable for the life of the process. The
// ETag is cheap and is ready when the handler is constructed; the gzipped body
// can be precomputed after the listener is bound (or lazily by the first gzip
// client) so health/readiness probes are not held behind BestCompression work
// on a saturated runner. A new image ⇒ new bytes ⇒ new ETag, which is precisely
// the invalidation we want; Cache-Control: no-cache forces revalidation on
// every load, so a rolled spoke can never serve a stale UI from browser cache.
type IndexDocument struct {
	raw      []byte
	gzipOnce sync.Once
	gzipped  []byte // nil when gzip compression failed; raw is then always served
	etag     string
}

// BrandingLinkTag is injected into the served index document so an operator
// can restyle the dashboard without forking the embedded SPA.
//
// It is injected UNCONDITIONALLY, not "only when the file exists": the index
// document is built once with a strong ETag and immutable gzip cache, so making
// its content depend on a file that can appear later would mean either
// rebuilding it per request or serving a stale page forever. An absent override
// simply 404s, and a 404'd stylesheet is inert.
const BrandingLinkTag = `<link rel="stylesheet" href="/branding/custom.css">`

// InjectBranding places the override link immediately before </head> so it
// wins the cascade against everything the embedded document defines. Falls
// back to returning the document untouched if there is no </head> to anchor
// to, rather than corrupting the markup.
func InjectBranding(raw []byte) []byte {
	marker := []byte("</head>")
	i := bytes.Index(raw, marker)
	if i < 0 {
		return raw
	}
	out := make([]byte, 0, len(raw)+len(BrandingLinkTag))
	out = append(out, raw[:i]...)
	out = append(out, []byte(BrandingLinkTag)...)
	out = append(out, raw[i:]...)
	return out
}

func NewIndexDocument(raw []byte) *IndexDocument {
	sum := sha256.Sum256(raw)
	// 16 hex bytes of the digest is plenty for cache validation and keeps the
	// header short; the quotes are part of the ETag grammar (RFC 9110 §8.8.3).
	return &IndexDocument{
		raw:  raw,
		etag: `"` + hex.EncodeToString(sum[:])[:16] + `"`,
	}
}

// Precompress computes the gzip representation ahead of traffic. It is safe to
// call from a goroutine after the dashboard listener is already bound; ServeHTTP
// will share the same sync.Once and wait only if a gzip-capable client arrives
// before this finishes.
func (d *IndexDocument) Precompress() {
	d.compressedBody()
}

func (d *IndexDocument) compressedBody() []byte {
	d.gzipOnce.Do(func() {
		d.gzipped = gzipBytes(d.raw)
	})
	return d.gzipped
}

func gzipBytes(raw []byte) []byte {
	var buf bytes.Buffer
	// BestCompression: this runs once per process for a highly compressible
	// document — spend the extra CPU outside the readiness path, not per request.
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err == nil {
		if _, err = zw.Write(raw); err == nil {
			if err = zw.Close(); err == nil {
				return buf.Bytes()
			}
		}
	}
	return nil
}

// acceptsGzip reports whether the request's Accept-Encoding allows gzip.
// A bare substring check would treat "gzip;q=0" (an explicit refusal) as
// acceptance, so each listed coding is inspected for a zero q-value.
func acceptsGzip(acceptEncoding string) bool {
	for _, part := range strings.Split(acceptEncoding, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		coding := strings.ToLower(strings.TrimSpace(fields[0]))
		if coding != "gzip" && coding != "*" {
			continue
		}
		for _, p := range fields[1:] {
			p = strings.TrimSpace(p)
			if q, ok := strings.CutPrefix(p, "q="); ok {
				if f, err := strconv.ParseFloat(q, 64); err == nil && f == 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

// ifNoneMatchHits reports whether the request's If-None-Match header matches
// etag. A weak validator prefix on the client's copy still matches: byte-range
// requests are not served here, so weak comparison (RFC 9110 §8.8.3.2) is the
// correct and safe interpretation for a full-document 304.
func ifNoneMatchHits(header, etag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

func (d *IndexDocument) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("ETag", d.etag)
	// no-cache = "store it, but revalidate before every use". Combined with the
	// strong ETag this makes repeat loads a 304 (no body) while guaranteeing a
	// freshly rolled spoke's new UI is picked up immediately.
	h.Set("Cache-Control", "no-cache")
	// The body varies with Accept-Encoding, so any intermediary cache must key
	// on it or it could hand gzip bytes to a client that cannot decode them.
	h.Set("Vary", "Accept-Encoding")

	if ifNoneMatchHits(r.Header.Get("If-None-Match"), d.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := d.raw
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		gzipped := d.compressedBody()
		if gzipped != nil {
			h.Set("Content-Encoding", "gzip")
			body = gzipped
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

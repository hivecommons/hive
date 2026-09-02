package dashboard

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
)

// indexDocument serves the embedded SPA document (static/index.html) with
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
// The document is embedded in the binary, so both the gzipped form and the
// ETag are computed exactly once at startup and are immutable for the life of
// the process. A new image ⇒ new bytes ⇒ new ETag, which is precisely the
// invalidation we want; Cache-Control: no-cache forces revalidation on every
// load, so a rolled spoke can never serve a stale UI from browser cache.
type indexDocument struct {
	raw     []byte
	gzipped []byte // nil when gzip compression failed; raw is then always served
	etag    string
}

// brandingLinkTag is injected into the served index document so an operator
// can restyle the dashboard without forking the embedded SPA.
//
// It is injected UNCONDITIONALLY, not "only when the file exists": the index
// document is built once at startup with a precomputed gzip body and a strong
// ETag, so making its content depend on a file that can appear later would
// mean either rebuilding it per request or serving a stale page forever. An
// absent override simply 404s, and a 404'd stylesheet is inert.
const brandingLinkTag = `<link rel="stylesheet" href="/branding/custom.css">`

// injectBranding places the override link immediately before </head> so it
// wins the cascade against everything the embedded document defines. Falls
// back to returning the document untouched if there is no </head> to anchor
// to, rather than corrupting the markup.
func injectBranding(raw []byte) []byte {
	marker := []byte("</head>")
	i := bytes.Index(raw, marker)
	if i < 0 {
		return raw
	}
	out := make([]byte, 0, len(raw)+len(brandingLinkTag))
	out = append(out, raw[:i]...)
	out = append(out, []byte(brandingLinkTag)...)
	out = append(out, raw[i:]...)
	return out
}

func newIndexDocument(raw []byte) *indexDocument {
	raw = injectBranding(raw)
	sum := sha256.Sum256(raw)
	// 16 hex bytes of the digest is plenty for cache validation and keeps the
	// header short; the quotes are part of the ETag grammar (RFC 9110 §8.8.3).
	d := &indexDocument{
		raw:  raw,
		etag: `"` + hex.EncodeToString(sum[:])[:16] + `"`,
	}
	var buf bytes.Buffer
	// BestCompression: this runs once per process for a highly compressible
	// document — spend the extra CPU at startup, not per request.
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err == nil {
		if _, err = zw.Write(raw); err == nil {
			if err = zw.Close(); err == nil {
				d.gzipped = buf.Bytes()
			}
		}
	}
	return d
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

func (d *indexDocument) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if d.gzipped != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		h.Set("Content-Encoding", "gzip")
		body = d.gzipped
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

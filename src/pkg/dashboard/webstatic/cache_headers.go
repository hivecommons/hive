package webstatic

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// etagDigestLen is the number of hex characters (from the SHA-256 digest)
// kept in a computed ETag. 16 hex chars (8 bytes) is far more collision
// resistant than this cache-validation use needs while keeping the header
// short - see NewIndexDocument for the original precedent.
const etagDigestLen = 16

// ETagFor computes a strong ETag from the exact bytes a handler is about to
// serve. Embedded documents (go:embed) carry a zero ModTime, so there is no
// Last-Modified to fall back on; hashing the content itself gives a
// validator that changes if and only if the served bytes change, which is
// exactly the invalidation an upgrade needs.
func ETagFor(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:])[:etagDigestLen] + `"`
}

// WriteCacheHeaders sets Cache-Control: no-cache and the given ETag on w,
// then answers a matching If-None-Match with a 304 and no body. It reports
// whether it already wrote the response (true) so the caller can return
// immediately instead of writing the document a second time.
//
// no-cache means "store it, but revalidate before every use" - the served
// page is never used without asking first, so a freshly upgraded hub/spoke
// is picked up on the very next load instead of waiting out a browser's
// heuristic freshness window (hivecommons/hive#9674).
func WriteCacheHeaders(w http.ResponseWriter, r *http.Request, etag string) bool {
	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if ifNoneMatchHits(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

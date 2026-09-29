package webstatic

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestETagForDiffersWhenContentDiffers(t *testing.T) {
	a := ETagFor([]byte("<html>one</html>"))
	b := ETagFor([]byte("<html>two</html>"))
	if a == b {
		t.Fatalf("ETagFor returned the same tag %q for different content", a)
	}
	if a == "" || b == "" {
		t.Fatal("ETagFor returned an empty tag")
	}
}

func TestETagForStableForSameContent(t *testing.T) {
	body := []byte("<html>same</html>")
	a := ETagFor(body)
	b := ETagFor(append([]byte(nil), body...)) // distinct slice, same bytes
	if a != b {
		t.Fatalf("ETagFor(%q) = %q, want %q (stable for identical content)", body, a, b)
	}
}

func TestWriteCacheHeadersSetsNoCacheAndETag(t *testing.T) {
	etag := ETagFor([]byte("hello"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	done := WriteCacheHeaders(rec, req, etag)
	if done {
		t.Fatal("WriteCacheHeaders reported done=true with no If-None-Match sent")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want %q", got, "no-cache")
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("ETag = %q, want %q", got, etag)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want default 200 (no WriteHeader call)", rec.Code)
	}
}

func TestWriteCacheHeadersAnswersMatchingIfNoneMatchWith304(t *testing.T) {
	etag := ETagFor([]byte("hello"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()

	done := WriteCacheHeaders(rec, req, etag)
	if !done {
		t.Fatal("WriteCacheHeaders reported done=false for a matching If-None-Match")
	}
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("body = %q, want empty on 304", body)
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("ETag = %q, want %q even on 304", got, etag)
	}
}

func TestWriteCacheHeadersIgnoresNonMatchingIfNoneMatch(t *testing.T) {
	etag := ETagFor([]byte("hello"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", `"some-other-etag"`)
	rec := httptest.NewRecorder()

	done := WriteCacheHeaders(rec, req, etag)
	if done {
		t.Fatal("WriteCacheHeaders reported done=true for a non-matching If-None-Match")
	}
}

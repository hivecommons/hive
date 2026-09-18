package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// The oversized arm of enforceIssueShape is a deliberate policy choice: a body
// past issueShapeBodyLimit is forwarded UNCHECKED rather than truncated, and
// the prefix already consumed for the size probe is stitched back in front of
// the unread remainder. Both halves of that promise were untested: that an
// oversized body containing a violation still passes (no deny), and that the
// stitched body reaching GitHub is byte-for-byte the original (no corruption
// of a large-but-legitimate issue write).
func TestEnforceIssueShapeOversizedBodyForwardedUncheckedAndIntact(t *testing.T) {
	// A body that WOULD be denied if shape-checked, padded past the limit so
	// the check is skipped. The padding lives in a JSON field so the payload
	// stays a plausible issue write.
	violating := `{"title":"<specific description of the documentation gap>","body":"`
	padding := strings.Repeat("x", issueShapeBodyLimit)
	body := violating + padding + `"}`
	if int64(len(body)) <= issueShapeBodyLimit {
		t.Fatalf("test body is not oversized: %d <= %d", len(body), issueShapeBodyLimit)
	}

	p := &GitHubProxy{logger: slog.Default()}
	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	wantLen := req.ContentLength

	reason, deny, readErr := p.enforceIssueShape(req)
	if readErr != nil {
		t.Fatalf("unexpected read error: %v", readErr)
	}
	if deny || reason != "" {
		t.Fatalf("oversized body must be forwarded unchecked, got deny=%v reason=%q", deny, reason)
	}
	if req.ContentLength != wantLen {
		t.Fatalf("ContentLength changed on the oversized path: got %d want %d", req.ContentLength, wantLen)
	}

	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("reading stitched body: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("stitched body corrupted: got %d bytes, want %d, prefix match=%v",
			len(restored), len(body), strings.HasPrefix(string(restored), body[:64]))
	}
	if err := req.Body.Close(); err != nil {
		t.Fatalf("closing stitched body: %v", err)
	}
}

// errAfterReader yields its content, then fails with err instead of io.EOF —
// the shape of a client connection that dies mid-body.
type errAfterReader struct {
	content string
	err     error
	off     int
	closed  bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off < len(r.content) {
		n := copy(p, r.content[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

func (r *errAfterReader) Close() error {
	r.closed = true
	return nil
}

// A body that cannot be read cannot be shape-checked, and the caller in
// proxyHTTP fails CLOSED on that error (the request is dropped, not
// forwarded). enforceIssueShape's contract for that arm — surface the read
// error, no deny verdict, body closed — was untested; a regression that
// swallowed the error would forward a half-read body to GitHub.
func TestEnforceIssueShapeReadErrorFailsClosed(t *testing.T) {
	wantErr := errors.New("client hung up mid-body")
	r := &errAfterReader{content: `{"title":"trunc`, err: wantErr}

	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Body = r

	p := &GitHubProxy{logger: slog.Default()}
	reason, deny, readErr := p.enforceIssueShape(req)
	if !errors.Is(readErr, wantErr) {
		t.Fatalf("read error not surfaced: got %v want %v", readErr, wantErr)
	}
	if deny || reason != "" {
		t.Fatalf("read-error arm must not produce a deny verdict, got deny=%v reason=%q", deny, reason)
	}
	if !r.closed {
		t.Fatal("unreadable body was not closed")
	}
}

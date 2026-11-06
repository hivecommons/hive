package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// /api/version reports the Go toolchain the binary was built with, not a
// literal that goes stale on the next toolchain bump.
func TestVersionReportsBuildGoVersion(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	s.handleVersion(rr, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := strings.TrimPrefix(runtime.Version(), "go")
	if got := body["go"]; got != want {
		t.Fatalf(`/api/version "go" = %v, want %q (runtime.Version() %q)`, got, want, runtime.Version())
	}
}

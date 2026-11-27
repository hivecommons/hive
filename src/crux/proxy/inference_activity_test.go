package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInferenceActivityRecordsIdentifiedMessagesOnly(t *testing.T) {
	p := &GitHubProxy{logger: slog.Default(), inference: newInferenceRouter()}
	h := p.inferenceTranslatorHandlerFor(func(r *http.Request) (string, bool) {
		return r.Header.Get("agent"), r.Header.Get("agent") != ""
	}, func(*http.Request) []any { return nil })
	request := func(method, path, agent string) {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Header.Set("agent", agent)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	request(http.MethodPost, "/v1/messages", "")
	request(http.MethodGet, "/v1/messages", "alice")
	request(http.MethodPost, "/v1/messages/count_tokens", "alice")
	if got := p.InferenceActivity(); len(got) != 0 {
		t.Fatalf("non-inference or unidentified requests counted: %v", got)
	}
	request(http.MethodPost, "/v1/messages", "alice")
	got := p.InferenceActivity()
	if got["alice"].IsZero() || len(got) != 1 {
		t.Fatalf("activity = %v", got)
	}
	delete(got, "alice")
	if p.InferenceActivity()["alice"].IsZero() {
		t.Fatal("snapshot mutation changed proxy state")
	}
	request(http.MethodPost, "/v1/messages", "bob")
	if len(p.InferenceActivity()) != 2 {
		t.Fatal("activity not isolated per agent")
	}
}

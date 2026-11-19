package askai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testProjectID     = "11111111-2222-3333-4444-555555555555"
	testIntegrationID = "66666666-7777-8888-9999-000000000000"
	testAPIKey        = "kapa-test-key-value"
)

// TestKapaClientContract pins the vendor contract: the path, the key header,
// a body with exactly integration_id and query, and the QueryResponse fields.
func TestKapaClientContract(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotKey, gotType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s, want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath, gotKey, gotType = r.URL.Path, r.Header.Get("X-API-KEY"), r.Header.Get("Content-Type")
		gotBody = nil
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		mu.Unlock()
		_, _ = io.WriteString(w, `{"answer":"Open Settings, Features.","thread_id":"aaaa-bbbb","question_answer_id":"qa-1","is_uncertain":true,
			"relevant_sources":[{"source_url":"https://docs.hivecommons.dev/settings/","title":"Settings","contains_internal_data":false},
			{"source_url":"https://intranet.example/x","title":"Internal","contains_internal_data":true}]}`)
	}))
	defer srv.Close()

	c, err := NewKapaClient(srv.URL, testProjectID, testIntegrationID, testAPIKey, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Chat(context.Background(), "", "where is the pause button")
	if err != nil {
		t.Fatal(err)
	}
	type capture struct {
		path, key, ctype string
		body             map[string]any
	}
	snap := func() capture {
		mu.Lock()
		defer mu.Unlock()
		return capture{gotPath, gotKey, gotType, gotBody}
	}
	first := snap()
	if first.path != "/query/v1/projects/"+testProjectID+"/chat/" {
		t.Fatalf("path %q", first.path)
	}
	if first.key != testAPIKey || first.ctype != "application/json" {
		t.Fatalf("headers key=%q type=%q", first.key, first.ctype)
	}
	if len(first.body) != 2 || first.body["integration_id"] != testIntegrationID || first.body["query"] != "where is the pause button" {
		t.Fatalf("body must hold exactly integration_id and query, got %v", first.body)
	}
	if got.Answer != "Open Settings, Features." || got.ThreadID != "aaaa-bbbb" || !got.Uncertain || len(got.Sources) != 2 ||
		got.Sources[0].URL != "https://docs.hivecommons.dev/settings/" || got.Sources[0].Title != "Settings" || !got.Sources[1].Internal {
		t.Fatalf("decoded %+v", got)
	}

	if _, err := c.Chat(context.Background(), "aaaa-bbbb", "and then?"); err != nil {
		t.Fatal(err)
	}
	if followUp := snap().path; followUp != "/query/v1/threads/aaaa-bbbb/chat/" {
		t.Fatalf("follow-up path %q", followUp)
	}
	if _, err := c.Chat(context.Background(), "../../etc", "x"); err == nil {
		t.Fatal("a thread id that is not a vendor id must be refused before any call")
	}
}

func TestKapaClientErrorsCarryNoCredentials(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, `{"detail":"echo `+testAPIKey+`"}`)
	}))
	c, err := NewKapaClient(srv.URL, testProjectID, testIntegrationID, testAPIKey, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Chat(context.Background(), "", "q")
	var se *UpstreamStatusError
	if !errors.As(err, &se) || !se.RateLimited() || se.RetryAfter != 7*time.Second {
		t.Fatalf("429: %v", err)
	}
	status.Store(http.StatusBadGateway)
	_, err = c.Chat(context.Background(), "", "q")
	if !errors.As(err, &se) || se.Status != http.StatusBadGateway || se.RateLimited() {
		t.Fatalf("502: %v", err)
	}
	srv.Close()
	_, err = c.Chat(context.Background(), "", "q")
	if err == nil {
		t.Fatal("closed server must fail")
	}
	for _, s := range []string{err.Error(), se.Error(), c.String()} {
		for _, secret := range []string{testAPIKey, testProjectID, testIntegrationID} {
			if strings.Contains(s, secret) {
				t.Fatalf("%q leaks a credential", s)
			}
		}
	}
	if _, err := NewKapaClient("", "bad/id", testIntegrationID, testAPIKey, nil); err == nil {
		t.Fatal("a project id with a slash must be refused")
	}
	if _, err := NewKapaClient("", testProjectID, testIntegrationID, " ", nil); err == nil {
		t.Fatal("an empty key must be refused")
	}
}

func TestParseRetryAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":            defaultRetryAfter,
		"soon":        defaultRetryAfter,
		"-3":          defaultRetryAfter,
		"12":          12 * time.Second,
		"99999999999": maxRetryAfter,
	} {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

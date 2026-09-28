package escalate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPushSinks(t *testing.T) {
	ev := Event{Severity: SeverityPage, Title: "Page", Body: "body", Link: "https://link"}
	var ntfyOK, pushoverOK, pdOK bool
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok" && r.Header.Get("Priority") == "high" && r.Header.Get("Title") == "Page" {
			ntfyOK = true
		}
	}))
	defer ntfy.Close()
	if err := (&NtfySink{URL: ntfy.URL, Token: "tok"}).Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	po := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("token") == "app" && r.Form.Get("user") == "user" && r.Form.Get("priority") == "1" {
			pushoverOK = true
		}
	}))
	defer po.Close()
	if err := (&PushoverSink{URL: po.URL, AppToken: "app", UserKey: "user"}).Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	pd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		payload := got["payload"].(map[string]any)
		if got["routing_key"] == "rk" && payload["severity"] == "critical" {
			pdOK = true
		}
	}))
	defer pd.Close()
	if err := (&PagerDutySink{URL: pd.URL, RoutingKey: "rk"}).Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if !ntfyOK || !pushoverOK || !pdOK {
		t.Fatalf("providers ok: ntfy=%v pushover=%v pd=%v", ntfyOK, pushoverOK, pdOK)
	}
}

func TestPushErrorsAndHelpers(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad", http.StatusBadGateway) }))
	defer bad.Close()
	if err := (&NtfySink{URL: bad.URL}).Deliver(context.Background(), Event{Severity: SeverityDecision, Title: "a\nb", Body: "b"}); err == nil {
		t.Fatal("expected status error")
	}
	if cleanHeader("a\nb") != "a b" {
		t.Fatal("header not cleaned")
	}
	if p := ntfyPriority(SeverityDecision); p != "default" {
		t.Fatalf("priority=%s", p)
	}
	if !strings.Contains((&PushoverSink{}).Name(), "pushover") {
		t.Fatal("name")
	}
}

func TestPushSinksTruncateToProviderLimits(t *testing.T) {
	longTitle := strings.Repeat("t", 2000)
	longBody := strings.Repeat("b", 8000)
	ev := Event{Severity: SeverityPage, Title: longTitle, Body: longBody}

	var gotNtfyTitle string
	var gotNtfyBodyLen int
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotNtfyTitle = r.Header.Get("Title")
		body, _ := io.ReadAll(r.Body)
		gotNtfyBodyLen = len(body)
	}))
	defer ntfy.Close()
	if err := (&NtfySink{URL: ntfy.URL}).Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if n := len(gotNtfyTitle); n > ntfyTitleLimit {
		t.Fatalf("ntfy title bytes=%d, want <= %d", n, ntfyTitleLimit)
	}
	if gotNtfyBodyLen > ntfyBodyLimit {
		t.Fatalf("ntfy body bytes=%d, want <= %d", gotNtfyBodyLen, ntfyBodyLimit)
	}

	var gotPOTitle, gotPOMessage string
	po := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotPOTitle = r.Form.Get("title")
		gotPOMessage = r.Form.Get("message")
	}))
	defer po.Close()
	if err := (&PushoverSink{URL: po.URL, AppToken: "app", UserKey: "user"}).Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(gotPOTitle)); n != pushoverTitleLimit {
		t.Fatalf("pushover title runes=%d, want %d", n, pushoverTitleLimit)
	}
	if n := len([]rune(gotPOMessage)); n != pushoverMessageLimit {
		t.Fatalf("pushover message runes=%d, want %d", n, pushoverMessageLimit)
	}

	var gotSummary string
	pd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		gotSummary, _ = got["payload"].(map[string]any)["summary"].(string)
	}))
	defer pd.Close()
	if err := (&PagerDutySink{URL: pd.URL, RoutingKey: "rk"}).Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(gotSummary)); n != pagerDutySummaryLimit {
		t.Fatalf("pagerduty summary runes=%d, want %d", n, pagerDutySummaryLimit)
	}
}

func TestTruncateHelpers(t *testing.T) {
	if got := truncate("hello", 3); got != "hel" {
		t.Fatalf("truncate=%q", got)
	}
	if got := truncate("hi", 10); got != "hi" {
		t.Fatalf("truncate short string changed: %q", got)
	}
	// Multi-byte runes must not be split in the middle.
	s := strings.Repeat("é", 10) // each 'é' is 2 bytes in UTF-8
	got := truncateBytes(s, 5)
	if len(got) > 5 {
		t.Fatalf("truncateBytes exceeded limit: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncateBytes produced invalid UTF-8: %q", got)
	}
}

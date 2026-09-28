package escalate

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// oversizeEvent is a governor page whose detail blows through every
// provider's limits, with a multi-byte rune straddling each cut point.
func oversizeEvent() Event {
	return Event{
		Severity: SeverityPage,
		Title:    "Governor budget exhausted — " + strings.Repeat("é", 2000),
		Body:     strings.Repeat("detail — ", 1000),
		Link:     "https://hive.example/runs/42",
	}
}

func TestPushoverTruncatesToProviderLimits(t *testing.T) {
	var got map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.Form
		// Pushover's documented limits, in characters.
		if utf8.RuneCountInString(r.Form.Get("message")) > 1024 || utf8.RuneCountInString(r.Form.Get("title")) > 250 || utf8.RuneCountInString(r.Form.Get("url")) > 512 {
			http.Error(w, `{"status":0}`, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	if err := (&PushoverSink{URL: srv.URL, AppToken: "app", UserKey: "user"}).Deliver(context.Background(), oversizeEvent()); err != nil {
		t.Fatalf("oversize page rejected: %v", err)
	}
	title, message := got["title"][0], got["message"][0]
	if n := utf8.RuneCountInString(title); n != 250 || !strings.HasSuffix(title, "…") || !utf8.ValidString(title) {
		t.Errorf("title = %d runes (suffix %q), want 250 ending in an ellipsis", n, title[len(title)-3:])
	}
	if n := utf8.RuneCountInString(message); n != 1024 || !strings.HasSuffix(message, "…") || !utf8.ValidString(message) {
		t.Errorf("message = %d runes, want 1024 ending in an ellipsis", n)
	}
	if got["url"][0] != "https://hive.example/runs/42" {
		t.Errorf("url = %q, want the link intact", got["url"][0])
	}

	// A link over the url limit is left off rather than cut into a dead URL.
	ev := oversizeEvent()
	ev.Link = "https://hive.example/" + strings.Repeat("x", 600)
	if err := (&PushoverSink{URL: srv.URL, AppToken: "app", UserKey: "user"}).Deliver(context.Background(), ev); err != nil {
		t.Fatalf("oversize link rejected: %v", err)
	}
	if _, ok := got["url"]; ok {
		t.Errorf("url = %q, want an oversize link omitted", got["url"][0])
	}
}

func TestPagerDutyTruncatesSummary(t *testing.T) {
	var summary string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Payload struct {
				Summary string `json:"summary"`
			} `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		summary = body.Payload.Summary
		if utf8.RuneCountInString(summary) > 1024 {
			http.Error(w, `{"status":"invalid event"}`, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	if err := (&PagerDutySink{URL: srv.URL, RoutingKey: "rk"}).Deliver(context.Background(), oversizeEvent()); err != nil {
		t.Fatalf("oversize page rejected: %v", err)
	}
	if n := utf8.RuneCountInString(summary); n != 1024 || !strings.HasSuffix(summary, "…") {
		t.Errorf("summary = %d runes, want 1024 ending in an ellipsis", n)
	}
}

func TestNtfyFitsMessageLimitAndKeepsLink(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	if err := (&NtfySink{URL: srv.URL}).Deliver(context.Background(), oversizeEvent()); err != nil {
		t.Fatal(err)
	}
	if len(body) > 4096 || !utf8.ValidString(body) {
		t.Fatalf("body = %d bytes (valid UTF-8 %v), want ≤4096 valid", len(body), utf8.ValidString(body))
	}
	if !strings.HasSuffix(body, "…\nhttps://hive.example/runs/42") {
		t.Fatalf("body tail = %q, want the cut body followed by the intact link", body[len(body)-40:])
	}
}

func TestNtfyTitleIsRFC2047Encoded(t *testing.T) {
	const title = "Governor budget exhausted — agent paused"
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = r.Header.Get("Title")
	}))
	defer srv.Close()
	if err := (&NtfySink{URL: srv.URL}).Deliver(context.Background(), Event{Severity: SeverityPage, Title: title}); err != nil {
		t.Fatal(err)
	}
	for i := range len(raw) {
		if raw[i] >= utf8.RuneSelf {
			t.Fatalf("Title header carries raw non-ASCII bytes: %q", raw)
		}
	}
	// ntfy decodes the header with RFC 2047 word decoding.
	decoded, err := new(mime.WordDecoder).DecodeHeader(raw)
	if err != nil || decoded != title {
		t.Fatalf("Title %q decodes to %q (err %v), want %q", raw, decoded, err, title)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"5":                             5 * time.Second,
		"0":                             0,
		"-3":                            0,
		"soon":                          0,
		"Mon, 28 Sep 2026 12:00:10 GMT": 10 * time.Second,
		"Mon, 28 Sep 2026 11:59:00 GMT": 0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

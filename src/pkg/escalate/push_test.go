package escalate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

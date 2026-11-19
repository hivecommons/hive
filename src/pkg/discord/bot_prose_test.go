package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every bot write goes through Bot.do, which applies the shared outbound dash
// rule (hivecommons/hive#19103) without touching the other payload fields.
func TestBotDoFiltersDashes(t *testing.T) {
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "chan")
	if err := b.SendMessageTo("rfc-channel", "RFC opened — please review"); err != nil {
		t.Fatalf("SendMessageTo: %v", err)
	}
	var got struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Content != "RFC opened - please review" {
		t.Errorf("content = %q", got.Content)
	}
	if !strings.Contains(string(body), `"allowed_mentions":{"parse":[]}`) {
		t.Errorf("allowed_mentions changed: %s", body)
	}
}

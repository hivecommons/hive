package commands

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestHivesActivityUnavailableShowsRetention(t *testing.T) {
	h := newHivesHarness(t)
	if err := h.run(t, "", "hives", "activity"); err != nil {
		t.Fatal(err)
	}
	if got := h.out.String(); !strings.Contains(got, "unavailable (retention: last 90 days") {
		t.Fatalf("missing history must read as unavailable with retention, got %q", got)
	}
	if err := h.run(t, "", "hives", "activity", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Available bool   `json:"available"`
		Retention string `json:"retention"`
		Completed int    `json:"completed"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Available || payload.Retention == "" {
		t.Fatalf("json must report unavailable with retention: %+v", payload)
	}
}

func TestHivesActivityRollupAndClearLeavesProfiles(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	body := `{"key":"k1","hub":"wss://a/contribute","repository":"github/github.com/org/repo","task_id":"t1","outcome":"success","started_at":"2026-10-01T10:00:00Z","ended_at":"2026-10-01T10:05:00Z"}
{"key":"k1","hub":"wss://a/contribute","repository":"github/github.com/org/repo","task_id":"t1","outcome":"success","started_at":"2026-10-01T10:00:00Z","ended_at":"2026-10-01T10:05:00Z"}
{"key":"k2","hub":"wss://b/contribute","repository":"github/github.com/org/repo","task_id":"t2","outcome":"failure","started_at":"2026-10-02T10:00:00Z","ended_at":"2026-10-02T10:05:00Z"}
garbage
`
	if err := os.WriteFile(h.store.RelayActivityPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "activity"); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"retained history", "1 completed of 2 attempts", "github/github.com/org/repo\tcompleted=1\tfailure=1 success=1\tlast=2026-10-02T10:05:00Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(h.errs.String(), "Skipped 1 unreadable line") {
		t.Fatalf("malformed line not reported: %q", h.errs.String())
	}
	before, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "activity", "--clear"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.store.RelayActivityPath()); !os.IsNotExist(err) {
		t.Fatalf("--clear left the activity file: %v", err)
	}
	after, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || h.signalCalls != 0 {
		t.Fatal("--clear must not touch profiles, consent or the relay")
	}
}

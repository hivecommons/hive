package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func attributedClosedPR(number int, updatedAt time.Time) map[string]any {
	return map[string]any{
		"number":     number,
		"title":      fmt.Sprintf("attributed %d", number),
		"state":      "closed",
		"body":       "done\n\n— hive: agent=scanner backend=bob model=auto",
		"user":       map[string]any{"login": "hive-bot[bot]"},
		"html_url":   fmt.Sprintf("https://github.com/o/r/pull/%d", number),
		"updated_at": updatedAt.Format(time.RFC3339),
		"closed_at":  updatedAt.Format(time.RFC3339),
	}
}

func openPR(number int) map[string]any {
	return map[string]any{
		"number":     number,
		"title":      fmt.Sprintf("open %d", number),
		"state":      "open",
		"body":       "work in progress",
		"user":       map[string]any{"login": "contributor"},
		"html_url":   fmt.Sprintf("https://github.com/o/r/pull/%d", number),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
		"created_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encoding response: %v", err)
	}
}

func TestFetchAttributedClosedPRsStopsAtLookback(t *testing.T) {
	t.Setenv(attributedClosedPRLookbackEnv, "24h")
	now := time.Now().UTC()
	var closedCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls" || r.URL.Query().Get("state") != "closed" {
			t.Fatalf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.URL.Query().Get("sort"); got != "updated" {
			t.Fatalf("sort = %q, want updated", got)
		}
		if got := r.URL.Query().Get("direction"); got != "desc" {
			t.Fatalf("direction = %q, want desc", got)
		}
		closedCalls++
		switch closedCalls {
		case 1:
			w.Header().Set("Link", `<`+srvURL(r)+`/repos/o/r/pulls?state=closed&sort=updated&direction=desc&page=2>; rel="next"`)
			writeJSON(t, w, []map[string]any{attributedClosedPR(1, now.Add(-2*time.Hour))})
		case 2:
			writeJSON(t, w, []map[string]any{
				attributedClosedPR(2, now.Add(-48*time.Hour)),
				attributedClosedPR(3, now.Add(-3*time.Hour)),
			})
		default:
			t.Fatalf("closed scan continued past lookback; call %d", closedCalls)
		}
	}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	prs, err := c.fetchAttributedClosedPRs(context.Background(), "o", "r", "r")
	if err != nil {
		t.Fatalf("fetchAttributedClosedPRs: %v", err)
	}
	if closedCalls != 2 {
		t.Fatalf("closed calls = %d, want 2", closedCalls)
	}
	if len(prs) != 1 || prs[0].Number != 1 {
		t.Fatalf("attributed PRs = %+v, want only #1", prs)
	}
}

func TestFetchAttributedClosedPRsStopsAtPageCap(t *testing.T) {
	t.Setenv(attributedClosedPRLookbackEnv, "14d")
	var closedCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls" || r.URL.Query().Get("state") != "closed" {
			t.Fatalf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		closedCalls++
		if closedCalls < 10 {
			w.Header().Set("Link", `<`+srvURL(r)+`/repos/o/r/pulls?state=closed&sort=updated&direction=desc&page=`+strconv.Itoa(closedCalls+1)+`>; rel="next"`)
		}
		writeJSON(t, w, []map[string]any{attributedClosedPR(closedCalls, time.Now().Add(-time.Hour))})
	}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	prs, err := c.fetchAttributedClosedPRs(context.Background(), "o", "r", "r")
	if err != nil {
		t.Fatalf("fetchAttributedClosedPRs: %v", err)
	}
	if closedCalls != attributedClosedPRMaxPages {
		t.Fatalf("closed calls = %d, want page cap %d", closedCalls, attributedClosedPRMaxPages)
	}
	if len(prs) != attributedClosedPRMaxPages {
		t.Fatalf("attributed PR count = %d, want %d", len(prs), attributedClosedPRMaxPages)
	}
}

func TestFetchPRsClosedScanErrorReturnsOpenPRsAndCachedAttributions(t *testing.T) {
	var closedCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/o/r/pulls/") || strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/") {
			writeJSON(t, w, []map[string]any{})
			return
		}
		if r.URL.Path != "/repos/o/r/pulls" {
			t.Fatalf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		switch r.URL.Query().Get("state") {
		case "open":
			writeJSON(t, w, []map[string]any{openPR(10)})
		case "closed":
			closedCalls++
			if closedCalls == 1 {
				writeJSON(t, w, []map[string]any{attributedClosedPR(7, time.Now().Add(-time.Hour))})
				return
			}
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected state query: %q", r.URL.Query().Get("state"))
		}
	}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, _, _, _, firstAttributed, _, _, err := c.fetchPRs(context.Background(), "r", nil)
	if err != nil {
		t.Fatalf("first fetchPRs: %v", err)
	}
	if len(firstAttributed) != 1 || firstAttributed[0].Number != 7 {
		t.Fatalf("first attributed = %+v, want cached closed #7", firstAttributed)
	}

	actionable, _, _, _, attributed, _, _, err := c.fetchPRs(context.Background(), "r", nil)
	if err != nil {
		t.Fatalf("closed scan failure must not fail fetchPRs: %v", err)
	}
	if len(actionable) != 1 || actionable[0].Number != 10 {
		t.Fatalf("actionable = %+v, want open PR #10", actionable)
	}
	if len(attributed) != 1 || attributed[0].Number != 7 {
		t.Fatalf("attributed = %+v, want last-good closed #7", attributed)
	}
}

func srvURL(r *http.Request) string {
	return "http://" + r.Host
}

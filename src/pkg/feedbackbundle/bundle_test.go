package feedbackbundle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewIDIsSortableAndPathSafe(t *testing.T) {
	id := NewID(time.Date(2026, 10, 19, 15, 30, 0, 0, time.UTC))
	if !strings.HasPrefix(id, "20261019T153000Z-") || strings.ContainsAny(id, "/\\ ") {
		t.Fatalf("id = %q", id)
	}
}

func TestPrefillBodyKeepsBundleLineAndCapsLength(t *testing.T) {
	long := strings.Repeat("ünïcödé text ", 2000) // ~26 KB
	p := Prefill{
		OpenedBy:    "Opened by @alice from hive demo (hub-less)",
		Description: long,
		Footer:      "Submitted from the Hive dashboard feedback form.\nTarget: Hive",
		BundleURL:   "https://github.com/o/r/blob/hive-feedback/feedback-bundles/x/report.md",
	}
	body := PrefillBody(p)
	if len(body) > MaxPrefillBodyBytes {
		t.Fatalf("body is %d bytes, cap is %d", len(body), MaxPrefillBodyBytes)
	}
	if !strings.Contains(body, "Diagnostic bundle: "+p.BundleURL) {
		t.Fatalf("bundle line dropped:\n%s", body)
	}
	if !strings.Contains(body, "truncated") {
		t.Fatalf("expected truncation marker:\n%s", body)
	}
	if !strings.HasPrefix(body, p.OpenedBy) {
		t.Fatalf("opened-by line missing:\n%s", body)
	}
	if strings.Contains(body, "\uFFFD") {
		t.Fatalf("truncation split a rune")
	}

	short := PrefillBody(Prefill{OpenedBy: "Opened by @a", Description: "hi", Footer: "f", BundleURL: ""})
	if !strings.Contains(short, "Diagnostic bundle: not stored") || strings.Contains(short, "truncated") {
		t.Fatalf("short body = %q", short)
	}
}

func TestPrefillURLEncodesEverything(t *testing.T) {
	u := PrefillURL(Prefill{
		Base:        "https://github.com/hivecommons/hive/issues/new",
		Title:       "Bug: a & b = c?",
		OpenedBy:    "Opened by @alice",
		Description: "line1\nline2 #1927",
		Footer:      "Target: Hive",
		BundleURL:   "https://github.com/o/r/blob/hive-feedback/feedback-bundles/x/report.md",
		Labels:      []string{"feedback", " ", "bug"},
	})
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "github.com" || parsed.Path != "/hivecommons/hive/issues/new" {
		t.Fatalf("url = %s", u)
	}
	q := parsed.Query()
	if q.Get("title") != "Bug: a & b = c?" {
		t.Fatalf("title = %q", q.Get("title"))
	}
	if q.Get("labels") != "feedback,bug" {
		t.Fatalf("labels = %q", q.Get("labels"))
	}
	if !strings.Contains(q.Get("body"), "line1\nline2 #1927") || !strings.Contains(q.Get("body"), "Diagnostic bundle: https://github.com/o/r/blob") {
		t.Fatalf("body = %q", q.Get("body"))
	}
	if len(u) > MaxPrefillBodyBytes*3+1024 {
		t.Fatalf("url unexpectedly long: %d", len(u))
	}
}

// Store must write only to the dedicated branch (creating it from the default
// branch head when missing), use the given token, and return the report URL.
func TestStoreCommitsToDedicatedBranchWithAppToken(t *testing.T) {
	var puts []map[string]string
	var paths []string
	branchExists := false
	var refCreated bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghs_install" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/"):
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if !branchExists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			puts = append(puts, payload)
			p := strings.TrimPrefix(r.URL.Path, "/repos/o/r/contents/")
			paths = append(paths, p)
			_ = json.NewEncoder(w).Encode(map[string]any{"content": map[string]string{
				"html_url":     "https://github.com/o/r/blob/" + Branch + "/" + p,
				"download_url": "https://raw.githubusercontent.com/o/r/" + Branch + "/" + p,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/git/ref/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "abc"}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/git/refs":
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["ref"] != "refs/heads/"+Branch || payload["sha"] != "abc" {
				t.Errorf("ref payload = %v", payload)
			}
			refCreated = true
			branchExists = true
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()

	png := []byte("\x89PNG\r\n\x1a\n0000")
	got, err := Store(context.Background(), srv.Client(), srv.URL, "ghs_install", "o", "r", "20261019T000000Z-aa", "# report\n", []Screenshot{{Content: png, Ext: "png"}})
	if err != nil {
		t.Fatal(err)
	}
	if !refCreated {
		t.Fatal("branch was not created from the default branch head")
	}
	if len(puts) != 2 {
		t.Fatalf("expected 2 content puts, got %d (%v)", len(puts), paths)
	}
	for _, p := range puts {
		if p["branch"] != Branch {
			t.Fatalf("commit went to branch %q, want %q", p["branch"], Branch)
		}
	}
	if paths[0] != Dir+"/20261019T000000Z-aa/screenshot-1.png" || paths[1] != Dir+"/20261019T000000Z-aa/"+ReportFile {
		t.Fatalf("paths = %v", paths)
	}
	report, _ := base64.StdEncoding.DecodeString(puts[1]["content"])
	if !strings.Contains(string(report), "![screenshot 1](https://raw.githubusercontent.com/o/r/"+Branch+"/"+Dir+"/20261019T000000Z-aa/screenshot-1.png)") {
		t.Fatalf("report does not embed the screenshot:\n%s", report)
	}
	if got.ReportURL != "https://github.com/o/r/blob/"+Branch+"/"+Dir+"/20261019T000000Z-aa/"+ReportFile {
		t.Fatalf("report url = %q", got.ReportURL)
	}
}

func TestStoreRefusesWithoutToken(t *testing.T) {
	if _, err := Store(context.Background(), nil, "https://api.github.com", "", "o", "r", "id", "x", nil); err == nil {
		t.Fatal("expected error without an installation token")
	}
	if _, err := Store(context.Background(), nil, "https://api.github.com", "ghs_x", "o", "r", "../evil", "x", nil); err == nil {
		t.Fatal("expected error for a path-unsafe id")
	}
}

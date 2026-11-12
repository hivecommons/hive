package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const plaudFact = `---
title: RFC: Voice inbox
type: reference
layer: personal
status: active
tags: [connector, plaud, plaud]
source: plaud
connector: plaud
source_id: rec-1
source_path: Plaud
attr_duration_ms: 125000
attr_plaud_file_id: rec-1
attr_recorded_at: 2026-11-10T08:58:00Z
attr_tags: voice, ideas
synthesized: 2026-11-10T08:58:00Z
synced_at: 2026-11-11T20:00:00Z
---

# RFC: Voice inbox

**Recorded:** 2026-11-10T08:58:00Z

## Summary (Plaud)

- ship the inbox

## Transcript

[00:02] Andy: Hello hive
[01:05] Andy: Second line
`

func writeFact(t *testing.T, dir, layer, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, layer, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFactSourceListsAndFetchesConnectorFacts(t *testing.T) {
	dir := t.TempDir()
	writeFact(t, dir, "personal", "plaud-plaud-rec-1.md", plaudFact)
	writeFact(t, dir, "personal", "plaud-plaud-rec-0.md", strings.Replace(strings.Replace(plaudFact, "rec-1", "rec-0", -1), "status: active", "status: deprecated", 1))
	writeFact(t, dir, "personal", "notion-docs-x.md", strings.Replace(plaudFact, "source: plaud", "source: notion", 1))
	src := &FactSource{ConnectorsDir: dir, SourceType: "plaud"}
	items, err := src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	it := items[0]
	if it.ID != "rec-1" || it.Source != "plaud" || it.Connector != "plaud" || it.Title != "RFC: Voice inbox" || it.FactSlug != "plaud-plaud-rec-1" || it.Layer != "personal" {
		t.Fatalf("item = %+v", it)
	}
	if it.DurationMS != 125000 || !it.RecordedAt.Equal(time.Date(2026, 11, 10, 8, 58, 0, 0, time.UTC)) || strings.Join(it.Tags, ",") != "voice,ideas" {
		t.Fatalf("attrs = %+v", it)
	}
	if !strings.Contains(it.Excerpt, "ship the inbox") || strings.Contains(it.Excerpt, "Recorded:") {
		t.Fatalf("excerpt = %q", it.Excerpt)
	}
	c, err := src.Fetch(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if c.Summary != "- ship the inbox" || !strings.HasPrefix(c.Transcript, "[00:02] Andy: Hello hive") || c.Provenance != "plaud://recordings/rec-1" {
		t.Fatalf("content = %+v", c)
	}
}

func TestRuleMatching(t *testing.T) {
	it := Item{Source: "plaud", Title: "rfc: Build the thing", Tags: []string{"Ideas"}}
	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"prefix case-insensitive", Rule{TitlePrefix: "RFC:"}, true},
		{"prefix mismatch", Rule{TitlePrefix: "Hive:"}, false},
		{"tag", Rule{Tag: "ideas"}, true},
		{"source mismatch", Rule{Source: "wisprflow", TitlePrefix: "RFC:"}, false},
		{"source match", Rule{Source: "plaud", TitlePrefix: "RFC:"}, true},
		{"no filter never matches", Rule{Destination: DestDibs}, false},
	}
	for _, tc := range cases {
		if got := RuleMatches(tc.rule, it); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
	if got := StripPrefix("RFC: Build the thing", DefaultTitlePrefixes); got != "Build the thing" {
		t.Fatalf("StripPrefix = %q", got)
	}
	if got := StripPrefix("Plain", DefaultTitlePrefixes); got != "Plain" {
		t.Fatalf("StripPrefix = %q", got)
	}
}

func TestStoreTransitions(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), "inbox.json"))
	key := Key("plaud", "rec-1")
	if _, err := st.Begin(key, DestSpekRFC, "o/r"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Begin(key, DestSpekRFC, "o/r"); !errors.Is(err, ErrRouteBusy) {
		t.Fatalf("second Begin while queued: %v", err)
	}
	if _, err := st.Set(key, DestSpekRFC, StatusRunning, Result{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Begin(key, DestSpekRFC, "o/r"); !errors.Is(err, ErrRouteBusy) {
		t.Fatalf("Begin while running: %v", err)
	}
	// dibs route on the same item is independent.
	if _, err := st.Begin(key, DestDibs, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Set(key, DestSpekRFC, StatusFiled, Result{URL: "https://github.com/o/r/issues/1", Children: []string{"https://github.com/o/r/issues/2"}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Set(key, DestDibs, StatusFailed, Result{}, "boom"); err != nil {
		t.Fatal(err)
	}
	// Reload from disk.
	st2 := NewStore(st.Path)
	items := []Item{{Source: "plaud", ID: "rec-1"}, {Source: "plaud", ID: "rec-9"}}
	st2.Apply(items)
	r := items[0].Routes[DestSpekRFC]
	if r == nil || r.Status != StatusFiled || r.URL != "https://github.com/o/r/issues/1" || len(r.Children) != 1 || r.Repo != "o/r" {
		t.Fatalf("spek route = %+v", r)
	}
	if d := items[0].Routes[DestDibs]; d == nil || d.Status != StatusFailed || d.Error != "boom" {
		t.Fatalf("dibs route = %+v", d)
	}
	if !items[0].Processed || items[1].Processed {
		t.Fatalf("processed flags = %v %v", items[0].Processed, items[1].Processed)
	}
	// A failed route may be retried; a filed one may be re-filed.
	if _, err := st2.Begin(key, DestDibs, ""); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if err := st2.Dismiss(Key("plaud", "rec-9"), true); err != nil {
		t.Fatal(err)
	}
	st2.Apply(items)
	if !items[1].Processed || !items[1].Dismissed {
		t.Fatalf("dismissed item must be processed: %+v", items[1])
	}
}

func TestParsePublishOutput(t *testing.T) {
	jsonOut := `Publishing...
{"target":"github","repo":"o/r","rfc_url":"https://github.com/o/r/issues/10","rfc_number":10,"children":[{"id":"a","url":"https://github.com/o/r/issues/11","number":11},{"id":"b","url":"https://github.com/o/r/issues/12","number":12}],"created":3}`
	res, err := ParsePublishOutput(jsonOut)
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://github.com/o/r/issues/10" || len(res.Children) != 2 || res.Children[1] != "https://github.com/o/r/issues/12" {
		t.Fatalf("json result = %+v", res)
	}
	textOut := "Draft written to .spektacular/specs/voice.md\nRFC: https://github.com/o/r/issues/20\n  child: https://github.com/o/r/issues/21\n  child: https://github.com/o/r/issues/22 (dup) https://github.com/o/r/issues/22\n"
	res, err = ParsePublishOutput(textOut)
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://github.com/o/r/issues/20" || len(res.Children) != 2 {
		t.Fatalf("text result = %+v", res)
	}
	if _, err := ParsePublishOutput("error: nothing happened"); err == nil {
		t.Fatal("no URL must be an error")
	}
}

func TestRenderAgentPrompt(t *testing.T) {
	it := Item{Source: "plaud", ID: "rec-1"}
	c := Content{Summary: "- ship it", Transcript: "Hello hive", Provenance: "plaud://recordings/rec-1"}
	p := RenderAgentPrompt(it, c, "o/r", "Voice inbox v2")
	for _, want := range []string{"spektacular publish voice-inbox-v2 --repo o/r --allow-draft --format json", "spektacular init", "Open questions", "plaud://recordings/rec-1", "## Summary\n- ship it", "## Transcript\nHello hive"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

type fakeSource struct {
	items []Item
	body  Content
}

func (f *fakeSource) Type() string                                 { return "plaud" }
func (f *fakeSource) List(context.Context) ([]Item, error)         { return f.items, nil }
func (f *fakeSource) Fetch(context.Context, Item) (Content, error) { return f.body, nil }

func newTestService(t *testing.T, cfg config.InboxConfig, src Source, dests ...Destination) *Service {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "inbox.json"))
	svc := NewService(cfg, store, []Source{src}, dests, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	svc.Run = func(fn func()) { fn() }
	return svc
}

func TestSpekDestinationRunsVoicePublish(t *testing.T) {
	var gotArgs []string
	var gotStdin string
	var gotEnv []string
	calls := 0
	d := &SpekDestination{
		Binary:      "spektacular",
		DefaultRepo: "o/r",
		Token:       func(context.Context, string) (string, error) { return "tok", nil },
		LookPath:    func(string) error { return nil },
		Exec: func(_ context.Context, _ string, args, env []string, stdin string) (string, error) {
			calls++
			if calls == 1 {
				return "Error: unknown flag: --format", errors.New("exit status 1")
			}
			gotArgs, gotEnv, gotStdin = args, env, stdin
			return "RFC: https://github.com/o/r/issues/5\nhttps://github.com/o/r/issues/6\n", nil
		},
	}
	it := Item{Source: "plaud", ID: "rec-1", Title: "RFC: Voice inbox"}
	c := Content{Title: "RFC: Voice inbox", Transcript: "Hello hive", Summary: "- ship it", Provenance: "plaud://recordings/rec-1"}
	res, err := d.Deliver(context.Background(), it, c, Request{Destination: DestSpekRFC})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected retry without --format json, calls=%d", calls)
	}
	want := []string{"voice", "--from", "stdin", "--title", "Voice inbox", "--publish", "--repo", "o/r"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v", gotArgs)
	}
	if !strings.Contains(gotStdin, "Hello hive") || !strings.Contains(gotStdin, "plaud://recordings/rec-1") {
		t.Fatalf("stdin = %q", gotStdin)
	}
	hasTok := false
	for _, e := range gotEnv {
		if e == "GITHUB_TOKEN=tok" {
			hasTok = true
		}
	}
	if !hasTok {
		t.Fatal("GITHUB_TOKEN not injected")
	}
	if res.URL != "https://github.com/o/r/issues/5" || len(res.Children) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestSpekDestinationFallsBackToAgentKick(t *testing.T) {
	var kicked, prompt string
	d := &SpekDestination{
		LookPath: func(string) error { return errors.New("not found") },
		Agent:    "builder",
		Kick:     func(agent, p string) error { kicked, prompt = agent, p; return nil },
	}
	res, err := d.Deliver(context.Background(), Item{Source: "plaud", ID: "x", Title: "Idea"}, Content{Markdown: "stuff"}, Request{Repo: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	if kicked != "builder" || !strings.Contains(prompt, "--repo o/r") || res.Note == "" || res.URL != "" {
		t.Fatalf("kick=%q note=%q prompt=%q", kicked, res.Note, prompt)
	}
	d.Agent = ""
	if _, err := d.Deliver(context.Background(), Item{Source: "plaud", ID: "x"}, Content{Markdown: "stuff"}, Request{Repo: "o/r"}); err == nil {
		t.Fatal("missing binary without agent must fail")
	}
	if _, err := (&SpekDestination{LookPath: func(string) error { return nil }}).Deliver(context.Background(), Item{}, Content{Markdown: "x"}, Request{}); !errors.Is(err, ErrNoRepo) {
		t.Fatalf("no repo: %v", err)
	}
}

type recordingDest struct {
	name string
	mu   sync.Mutex
	got  []Request
	res  Result
	err  error
}

func (r *recordingDest) Name() string { return r.name }
func (r *recordingDest) Deliver(_ context.Context, _ Item, _ Content, req Request) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, req)
	return r.res, r.err
}

func TestServiceRouteAndSweep(t *testing.T) {
	src := &fakeSource{items: []Item{
		{Source: "plaud", ID: "rec-1", Title: "RFC: one", RecordedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
		{Source: "plaud", ID: "rec-2", Title: "note", RecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}, body: Content{Markdown: "x"}}
	spek := &recordingDest{name: DestSpekRFC, res: Result{URL: "https://github.com/o/r/issues/1"}}
	dibs := &recordingDest{name: DestDibs, err: errors.New("dibs down")}
	cfg := config.InboxConfig{
		Rules: []config.InboxRule{{TitlePrefix: "RFC:", Destination: DestSpekRFC}},
		Spek:  config.InboxSpek{Repo: "o/r"},
	}
	svc := newTestService(t, cfg, src, spek, dibs)

	routed, err := svc.Sweep(context.Background())
	if err != nil || routed != 1 {
		t.Fatalf("sweep routed=%d err=%v", routed, err)
	}
	if len(spek.got) != 1 || spek.got[0].Repo != "o/r" {
		t.Fatalf("spek requests = %+v", spek.got)
	}
	items, _ := svc.Items(context.Background())
	if items[0].ID != "rec-1" || items[0].Routes[DestSpekRFC].Status != StatusFiled || !items[0].Processed {
		t.Fatalf("rec-1 = %+v", items[0])
	}
	if items[1].Processed || Unprocessed(items) != 1 {
		t.Fatalf("rec-2 should be unprocessed: %+v", items[1])
	}
	// A second sweep must not re-fire on seen items.
	if routed, _ := svc.Sweep(context.Background()); routed != 0 {
		t.Fatalf("second sweep routed %d", routed)
	}
	// Manual dibs route fails and records the error.
	if _, err := svc.Route(context.Background(), items[1], Request{Destination: DestDibs}); err != nil {
		t.Fatal(err)
	}
	items, _ = svc.Items(context.Background())
	if r := items[1].Routes[DestDibs]; r == nil || r.Status != StatusFailed || r.Error != "dibs down" {
		t.Fatalf("dibs route = %+v", r)
	}
	if _, err := svc.Route(context.Background(), items[1], Request{Destination: "nope"}); !errors.Is(err, ErrUnknownDestination) {
		t.Fatalf("unknown destination: %v", err)
	}
	// Dismiss = knowledge only.
	if _, err := svc.Route(context.Background(), items[1], Request{Destination: DestDismiss}); err != nil {
		t.Fatal(err)
	}
	items, _ = svc.Items(context.Background())
	if !items[1].Dismissed || Unprocessed(items) != 0 {
		t.Fatalf("dismiss failed: %+v", items[1])
	}
}

func TestDibsDestinationPostsIdea(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ideas" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"idea-7","title":"x"}`))
	}))
	defer srv.Close()
	d := &DibsDestination{URL: srv.URL + "/", Token: "hub-tok"}
	it := Item{Source: "plaud", ID: "rec-1", Title: "Idea", RecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	res, err := d.Deliver(context.Background(), it, Content{Title: "Idea", Summary: "sum", Transcript: "words", Provenance: "plaud://recordings/rec-1"}, Request{})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer hub-tok" || gotBody["title"] != "Idea" || gotBody["visibility"] != "private" || gotBody["status"] != "draft" {
		t.Fatalf("auth=%q body=%v", gotAuth, gotBody)
	}
	body, _ := gotBody["body"].(string)
	if !strings.Contains(body, "plaud://recordings/rec-1") || !strings.Contains(body, "## Transcript") || !strings.HasPrefix(body, "sum") {
		t.Fatalf("body = %q", body)
	}
	if res.URL != srv.URL+"/api/ideas/idea-7" {
		t.Fatalf("url = %q", res.URL)
	}
	if _, err := (&DibsDestination{}).Deliver(context.Background(), it, Content{}, Request{}); !errors.Is(err, ErrDibsUnconfigured) {
		t.Fatalf("unconfigured: %v", err)
	}
}

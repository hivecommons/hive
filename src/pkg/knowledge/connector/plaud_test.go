package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func plaudTestFiles() []map[string]any {
	return []map[string]any{
		{"id": "rec-1", "name": "Standup ideas", "created_at": "2026-11-10T09:00:00Z", "start_at": "2026-11-10T08:58:00Z", "duration": 125000.0, "serial_number": "PN123"},
		{"id": "rec-2", "name": "Drive home", "created_at": "2026-11-11T18:00:00Z", "duration": 61000.0},
		{"id": "rec-old", "name": "Ancient", "created_at": "2020-01-01T00:00:00Z"},
	}
}

func newPlaudTestServer() *fakeMCPServer {
	srv := &fakeMCPServer{tools: map[string]func(map[string]any) (string, bool){}}
	srv.tools["list_files"] = func(a map[string]any) (string, bool) {
		if p, _ := a["page"].(float64); p > 1 {
			return "[]", false
		}
		b, _ := json.Marshal(map[string]any{"files": plaudTestFiles()})
		return string(b), false
	}
	srv.tools["get_note"] = func(a map[string]any) (string, bool) {
		if a["file_id"] == "rec-2" {
			return "", true // still processing
		}
		b, _ := json.Marshal(map[string]any{"note_list": []string{"## Key points\n- ship plaud connector"}})
		return string(b), false
	}
	srv.tools["get_transcript"] = func(a map[string]any) (string, bool) {
		if a["file_id"] == "rec-2" {
			return `{"source_list":[]}`, false
		}
		return `{"source_list":[{"start_time":1500,"speaker":"Andy","text":"Hello hive"},{"start_time":65000,"speaker":"Andy","text":"Second line"}]}`, false
	}
	srv.tools["get_current_user"] = func(map[string]any) (string, bool) {
		return `{"email":"andy@example.com"}`, false
	}
	return srv
}

func newTestPlaud(t *testing.T, srv *fakeMCPServer, scope map[string]string) *plaudConnector {
	t.Helper()
	cfg := ConnectorConfig{Name: "plaud", Type: TypePlaud, Enabled: true, Layer: "personal", Scope: scope}
	c, err := DefaultRegistry().New(cfg, Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	pc := c.(*plaudConnector)
	pc.dial = pipeDialer(srv)
	pc.now = func() time.Time { return time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC) }
	return pc
}

func TestPlaudSyncEmitsPagesAndAdvancesCursor(t *testing.T) {
	srv := newPlaudTestServer()
	pc := newTestPlaud(t, srv, map[string]string{"tags": "voice, ideas"})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var pages []Page
	next, err := pc.Sync(ctx, "", func(p Page) error { pages = append(pages, p); return nil })
	if err == nil || !IsPartial(err) || !strings.Contains(err.Error(), "rec-2") {
		t.Fatalf("want partial error naming rec-2, got %v", err)
	}
	if len(pages) != 1 || pages[0].ID != "rec-1" {
		t.Fatalf("pages = %+v", pages)
	}
	p := pages[0]
	for _, want := range []string{"# Standup ideas", "**Recorded:** 2026-11-10T08:58:00Z", "**Duration:** 2m5s", "## Summary (Plaud)", "ship plaud connector", "## Transcript", "[00:02] Andy: Hello hive", "[01:05] Andy: Second line"} {
		if !strings.Contains(p.Markdown, want) {
			t.Errorf("markdown missing %q:\n%s", want, p.Markdown)
		}
	}
	if p.Attrs["plaud_file_id"] != "rec-1" || p.Attrs["tags"] != "voice, ideas" || p.Attrs["device_serial"] != "PN123" || p.Attrs["duration_ms"] != "125000" {
		t.Fatalf("attrs = %v", p.Attrs)
	}
	if next != Cursor("2026-11-10T09:00:00Z") {
		t.Fatalf("cursor = %q", next)
	}
	// The sync refreshes the sign-in state for the dashboard card.
	if st := pc.AuthStatus(ctx, false); !st.Connected || st.Account != "andy@example.com" {
		t.Fatalf("auth after sync = %+v", st)
	}
	calls := strings.Join(srv.callNames(), ",")
	if strings.Contains(calls, "get_file") {
		t.Fatalf("audio must not be fetched by default: %s", calls)
	}
}

func TestPlaudSyncUsesCursorWindowAndAudio(t *testing.T) {
	srv := newPlaudTestServer()
	var dateFrom string
	list := srv.tools["list_files"]
	srv.tools["list_files"] = func(a map[string]any) (string, bool) {
		dateFrom, _ = a["date_from"].(string)
		return list(a)
	}
	srv.tools["get_file"] = func(map[string]any) (string, bool) {
		return `{"id":"rec-1","presigned_url":"https://cdn.plaud.example/rec-1.mp3?sig=1"}`, false
	}
	pc := newTestPlaud(t, srv, map[string]string{"include_audio": "true"})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var pages []Page
	next, err := pc.Sync(ctx, "2026-11-11T00:00:00Z", func(p Page) error { pages = append(pages, p); return nil })
	if err == nil || !IsPartial(err) {
		t.Fatalf("err = %v", err)
	}
	// Cursor minus the 24h overlap → 2026-11-10; rec-1 (Nov 10) is re-listed.
	if dateFrom != "2026-11-10" {
		t.Fatalf("date_from = %q", dateFrom)
	}
	if len(pages) != 1 || !strings.Contains(pages[0].Markdown, "cdn.plaud.example/rec-1.mp3") {
		t.Fatalf("pages = %+v", pages)
	}
	// The cursor never moves backwards.
	if next != Cursor("2026-11-11T00:00:00Z") {
		t.Fatalf("cursor = %q", next)
	}
}

func TestPlaudSyncNotAuthenticated(t *testing.T) {
	srv := newPlaudTestServer()
	srv.tools["list_files"] = func(map[string]any) (string, bool) { return "401 Not authenticated", true }
	pc := newTestPlaud(t, srv, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := pc.Sync(ctx, "", func(Page) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Connect") {
		t.Fatalf("want connect hint, got %v", err)
	}
}

func TestPlaudValidate(t *testing.T) {
	base := ConnectorConfig{Name: "plaud", Type: TypePlaud, Layer: "personal"}
	reg := DefaultRegistry()
	deps := Deps{StateDir: t.TempDir()}
	if _, err := reg.New(base, deps); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
	bad := []ConnectorConfig{
		{Name: "p", Type: TypePlaud, Layer: "personal", Auth: Auth{Env: "PLAUD_TOKEN"}},
		{Name: "p", Type: TypePlaud, Layer: "personal", Scope: map[string]string{"since_days": "zero"}},
		{Name: "p", Type: TypePlaud, Layer: "personal", Scope: map[string]string{"include_audio": "maybe"}},
		{Name: "p", Type: TypePlaud, Layer: "personal", Scope: map[string]string{"home": "relative/dir"}},
		{Name: "p", Type: TypePlaud, Layer: "personal", Scope: map[string]string{"command": `npx "oops`}},
	}
	for i, cfg := range bad {
		if _, err := reg.New(cfg, deps); err == nil {
			t.Errorf("config %d must be rejected: %+v", i, cfg.Scope)
		}
	}
}

func TestPlaudConnectCompletesLogin(t *testing.T) {
	srv := newPlaudTestServer()
	srv.tools["login"] = func(map[string]any) (string, bool) {
		return "Open https://auth.plaud.example/authorize?state=abc to sign in", false
	}
	srv.tools["logout"] = func(map[string]any) (string, bool) { return "Signed out", false }
	pc := newTestPlaud(t, srv, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := pc.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// get_current_user succeeds right away in the fake, so the login
	// finishes inside the reply window and the state is already connected.
	if !st.Connected || st.Account != "andy@example.com" || st.Pending {
		t.Fatalf("state after connect = %+v", st)
	}
	st, err = pc.Disconnect(ctx)
	if err != nil || st.Connected || st.Message != "Signed out" {
		t.Fatalf("disconnect = %+v, %v", st, err)
	}
}

func TestPlaudConnectLoginError(t *testing.T) {
	srv := newPlaudTestServer()
	srv.tools["login"] = func(map[string]any) (string, bool) { return "browser could not be opened", true }
	pc := newTestPlaud(t, srv, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := pc.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Connected || st.Pending || !strings.Contains(st.Message, "browser could not be opened") {
		t.Fatalf("state = %+v", st)
	}
}

func TestPlaudRenderHelpers(t *testing.T) {
	if got := renderPlaudNote("plain **markdown**"); got != "plain **markdown**" {
		t.Fatalf("plain note = %q", got)
	}
	if got := renderPlaudNote(`{"note_list":[{"title":"Action items","content":"- do it"}]}`); got != "- do it" {
		t.Fatalf("nested note = %q", got)
	}
	if got := renderPlaudTranscript("raw transcript"); got != "raw transcript" {
		t.Fatalf("plain transcript = %q", got)
	}
	if got := renderPlaudTranscript(`{"transcript":"as string"}`); got != "as string" {
		t.Fatalf("string transcript = %q", got)
	}
	if got := renderPlaudTranscript(`[{"start":3725000,"text":"late"}]`); !strings.HasPrefix(got, "[1:02:05] late") {
		t.Fatalf("hour timestamp = %q", got)
	}
	files, err := decodePlaudFiles("Here are your files:\n" + `[{"id": 7, "name": "n", "created_at": 1762848000}]`)
	if err != nil || len(files) != 1 || files[0].ID != "7" || files[0].CreatedAt.Year() != 2025 {
		t.Fatalf("decode with prose prefix = %+v, %v", files, err)
	}
	if _, err := decodePlaudFiles("no json here"); err == nil {
		t.Fatal("non-JSON listing must error")
	}
	if got := plaudAccountName(`{"user":{"name":"Andy"}}`); got != "Andy" {
		t.Fatalf("account = %q", got)
	}
	if got := fmt.Sprint(plaudAuthError(fmt.Errorf("mcp: list_files: Unauthorized"))); !strings.Contains(got, "not connected") {
		t.Fatalf("auth error = %q", got)
	}
}

func TestPlaudListAndReadRecording(t *testing.T) {
	pc := newTestPlaud(t, newPlaudTestServer(), map[string]string{"tags": "voice, ideas"})
	recs, err := pc.ListRecordings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].ID != "rec-2" || recs[1].ID != "rec-1" {
		t.Fatalf("recs = %+v", recs)
	}
	if recs[1].Title != "Standup ideas" || recs[1].DurationMS != 125000 || len(recs[1].Tags) != 2 {
		t.Fatalf("rec-1 = %+v", recs[1])
	}
	if want := time.Date(2026, 11, 10, 8, 58, 0, 0, time.UTC); !recs[1].RecordedAt.Equal(want) {
		t.Fatalf("recorded at = %v", recs[1].RecordedAt)
	}
	c, err := pc.ReadRecording(context.Background(), "rec-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Summary, "ship plaud connector") || !strings.Contains(c.Transcript, "Hello hive") {
		t.Fatalf("content = %+v", c)
	}
	if _, err := pc.ReadRecording(context.Background(), "rec-2"); err == nil {
		t.Fatal("recording with no note or transcript should error")
	}
}

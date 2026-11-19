package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func wisprTestNow() time.Time { return time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC) }

func newTestWispr(t *testing.T, scope map[string]string) *wisprConnector {
	t.Helper()
	cfg := ConnectorConfig{Name: "wispr", Type: TypeWisprFlow, Enabled: true, Layer: "personal", Scope: scope}
	c, err := DefaultRegistry().New(cfg, Deps{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	wc := c.(*wisprConnector)
	wc.now = wisprTestNow
	return wc
}

// wisprFixtureDB builds flow.sqlite from testdata/wisprflow/history.sql with
// the sqlite3 CLI the connector itself shells out to; callers skip when the
// CLI is absent (CI runners), so the sync tests use wisprFakeQuery instead.
func wisprFixtureDB(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not installed")
	}
	sql, err := os.ReadFile(filepath.Join("testdata", "wisprflow", "history.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "flow.sqlite")
	cmd := exec.Command("sqlite3", db)
	cmd.Stdin = strings.NewReader(string(sql))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fixture db: %v: %s", err, out)
	}
	return db
}

var wisprSinceRE = regexp.MustCompile(`datetime\('([^']+)'\)`)

// wisprFakeQuery stands in for the sqlite3 CLI: it serves the rows of
// testdata/wisprflow/history.json (the same data as history.sql) and applies
// the WHERE clause of wisprHistorySQL — status, isArchived and the since
// bound — so the connector sees exactly what sqlite3 -json would return.
func wisprFakeQuery(t *testing.T) func(ctx context.Context, dbPath, sql string) ([]byte, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "wisprflow", "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	return func(_ context.Context, dbPath, sql string) ([]byte, error) {
		if _, err := os.Stat(dbPath); err != nil {
			return nil, fmt.Errorf("wisprflow: sqlite3 failed: unable to open database")
		}
		m := wisprSinceRE.FindStringSubmatch(sql)
		if m == nil || !strings.Contains(sql, "status='formatted'") {
			return nil, fmt.Errorf("unexpected sql: %s", sql)
		}
		since, err := time.Parse("2006-01-02 15:04:05", m[1])
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		for _, r := range rows {
			at := wisprParseTime(r["at"].(string))
			if r["status"] != "formatted" || r["isArchived"] != float64(0) || !at.After(since) {
				continue
			}
			out = append(out, r)
		}
		return json.Marshal(out)
	}
}

// newFakeWispr returns a connector whose sqlite3 runner is wisprFakeQuery
// and whose path points at an existing (empty) flow.sqlite file.
func newFakeWispr(t *testing.T, scope map[string]string) *wisprConnector {
	t.Helper()
	db := filepath.Join(t.TempDir(), "flow.sqlite")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if scope == nil {
		scope = map[string]string{}
	}
	scope["path"] = db
	c := newTestWispr(t, scope)
	c.query = wisprFakeQuery(t)
	return c
}

func collectWispr(t *testing.T, c *wisprConnector, cur Cursor) ([]Page, Cursor, error) {
	t.Helper()
	var pages []Page
	next, err := c.Sync(context.Background(), cur, func(p Page) error {
		pages = append(pages, p)
		return nil
	})
	return pages, next, err
}

func wisprIDs(pages []Page) []string {
	out := make([]string, 0, len(pages))
	for _, p := range pages {
		out = append(out, p.ID)
	}
	return out
}

func TestWisprValidate(t *testing.T) {
	c := newTestWispr(t, map[string]string{"path": "/x/flow.sqlite"})
	cases := []struct {
		name string
		cfg  ConnectorConfig
		want string
	}{
		{"auth rejected", ConnectorConfig{Auth: Auth{Env: "X"}, Scope: map[string]string{"path": "/x/flow.sqlite"}}, "not used"},
		{"relative path", ConnectorConfig{Scope: map[string]string{"path": "flow.sqlite"}}, "absolute"},
		{"bad group", ConnectorConfig{Scope: map[string]string{"path": "/x", "group": "week"}}, "scope.group"},
		{"bad since", ConnectorConfig{Scope: map[string]string{"path": "/x", "since_days": "0"}}, "since_days"},
		{"bad include_raw", ConnectorConfig{Scope: map[string]string{"path": "/x", "include_raw": "maybe"}}, "include_raw"},
	}
	for _, tc := range cases {
		err := c.Validate(tc.cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
	ok := ConnectorConfig{Scope: map[string]string{"path": "/x/flow.sqlite", "group": "day", "since_days": "7", "min_words": "0", "include_raw": "true"}}
	if err := c.Validate(ok); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestWisprSyncSQLite(t *testing.T) {
	c := newFakeWispr(t, map[string]string{"tags": "voice", "include_raw": "true"})

	pages, cur, err := collectWispr(t, c, "")
	if err != nil {
		t.Fatal(err)
	}
	// d-short (1 word), d-raw (not formatted), d-archived and d-old (before
	// since_days) are all excluded.
	if got := wisprIDs(pages); strings.Join(got, ",") != "d-1,d-2" {
		t.Fatalf("pages = %v", got)
	}
	if cur != "2026-11-11T17:55:30Z" {
		t.Fatalf("cursor = %q", cur)
	}
	p := pages[1]
	if p.Title != "Running late — start without me." {
		t.Errorf("title = %q (editedText should win)", p.Title)
	}
	for _, want := range []string{"**App:** Slack", "**Dictated:** 2026-11-11T17:55:30Z", "## Raw transcript", "running late start without me"} {
		if !strings.Contains(p.Markdown, want) {
			t.Errorf("markdown missing %q:\n%s", want, p.Markdown)
		}
	}
	if p.Attrs["wispr_transcript_id"] != "d-2" || p.Attrs["app"] != "com.tinyspeck.slackmacgap" || p.Attrs["tags"] != "voice" || p.Attrs["num_words"] != "5" {
		t.Errorf("attrs = %v", p.Attrs)
	}
	if strings.Join(p.Path, "/") != "Wispr Flow/2026-11-11" {
		t.Errorf("path = %v", p.Path)
	}
	if _, ok := pages[0].Attrs["tags"]; !ok || strings.Contains(pages[0].Markdown, "## Raw transcript") == false {
		t.Errorf("first page attrs/raw = %v", pages[0].Attrs)
	}

	// Incremental: the overlap window re-reads d-2 only.
	pages, cur2, err := collectWispr(t, c, cur)
	if err != nil {
		t.Fatal(err)
	}
	if got := wisprIDs(pages); strings.Join(got, ",") != "d-2" {
		t.Fatalf("incremental pages = %v", got)
	}
	if cur2 != cur {
		t.Fatalf("cursor moved to %q", cur2)
	}
}

func TestWisprFactInboxMetadata(t *testing.T) {
	c := newFakeWispr(t, map[string]string{"include_raw": "true"})
	pages, _, err := collectWispr(t, c, "")
	if err != nil {
		t.Fatal(err)
	}
	w := testWriter(t)
	slug, _, err := w.Write(c.cfg, pages[1])
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(w.Dir, slug+".md"))
	if err != nil {
		t.Fatal(err)
	}
	fact := string(data)
	for _, want := range []string{
		"source: wisprflow\n",
		"source_id: d-2\n",
		"attr_recorded_at: 2026-11-11T17:55:30Z\n",
		"## Transcript\n\nRunning late — start without me.\n",
	} {
		if !strings.Contains(fact, want) {
			t.Errorf("fact missing %q:\n%s", want, fact)
		}
	}
}

func TestWisprSyncAppsFilterAndDayGroup(t *testing.T) {
	c := newFakeWispr(t, map[string]string{"apps": "slack, vscode", "group": "day", "since_days": "3650"})
	pages, _, err := collectWispr(t, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := wisprIDs(pages); strings.Join(got, ",") != "day-2026-11-10,day-2026-11-11" {
		t.Fatalf("pages = %v", got)
	}
	day := pages[1]
	if day.Attrs["dictations"] != "1" || day.Attrs["recorded_at"] != "2026-11-11T17:55:30Z" || !strings.Contains(day.Markdown, "## 17:55 · Slack") {
		t.Errorf("day page = %v\n%s", day.Attrs, day.Markdown)
	}
	if strings.Contains(day.Markdown, "Ok.") {
		t.Error("one-word dictation leaked into the day page")
	}
	// d-old is Chrome, so even with a 10-year window the apps filter drops it.
	for _, p := range pages {
		if strings.Contains(p.Markdown, "Ancient") {
			t.Error("apps filter did not drop the Chrome dictation")
		}
	}
}

// TestWisprSQLite3CLI runs the real sqlite3 CLI against the fixture database
// and checks it agrees with the fake used by the other tests.
func TestWisprSQLite3CLI(t *testing.T) {
	db := wisprFixtureDB(t)
	c := newTestWispr(t, map[string]string{"path": db, "since_days": "3650"})
	pages, cur, err := collectWispr(t, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := wisprIDs(pages); strings.Join(got, ",") != "d-old,d-1,d-2" {
		t.Fatalf("pages = %v", got)
	}
	if cur != "2026-11-11T17:55:30Z" {
		t.Fatalf("cursor = %q", cur)
	}
}

func TestWisprSyncMissingPath(t *testing.T) {
	c := newTestWispr(t, map[string]string{"path": filepath.Join(t.TempDir(), "nope", "flow.sqlite")})
	_, cur, err := collectWispr(t, c, "2026-01-01T00:00:00Z")
	if err == nil || !strings.Contains(err.Error(), "Full Disk Access") {
		t.Fatalf("err = %v", err)
	}
	if cur != "2026-01-01T00:00:00Z" {
		t.Fatalf("cursor changed on error: %q", cur)
	}
}

func TestWisprSyncSQLiteError(t *testing.T) {
	abs, _ := filepath.Abs(filepath.Join("testdata", "wisprflow", "history.sql"))
	c := newTestWispr(t, map[string]string{"path": abs})
	c.query = func(context.Context, string, string) ([]byte, error) {
		return nil, &PartialError{Err: os.ErrPermission}
	}
	if _, _, err := collectWispr(t, c, ""); err == nil {
		t.Fatal("expected sqlite error to surface")
	}
}

func TestWisprHistorySQL(t *testing.T) {
	sql := wisprHistorySQL(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), 7)
	for _, want := range []string{"status='formatted'", "datetime('2026-11-01 12:00:00')", "LIMIT 7", "COALESCE(isArchived,0)=0", "NULLIF(editedText,'')"} {
		if !strings.Contains(sql, want) {
			t.Errorf("sql missing %q: %s", want, sql)
		}
	}
	if strings.Contains(sql, "audio") || strings.Contains(sql, "screenshot") {
		t.Error("sql must not select BLOB columns")
	}
}

func TestWisprSyncExportDir(t *testing.T) {
	dir, _ := filepath.Abs(filepath.Join("testdata", "wisprflow", "export"))
	c := newTestWispr(t, map[string]string{"path": dir})
	pages, cur, err := collectWispr(t, c, "")
	if err != nil {
		t.Fatal(err)
	}
	// j-old is before since_days; n-2 is one word; ignored.bin is skipped.
	if got := wisprIDs(pages); strings.Join(got, ",") != "j-1,m-fm,n-1,c-1,2026-11-11-1430-cursor" {
		t.Fatalf("pages = %v", got)
	}
	if cur != "2026-11-11T14:30:00Z" {
		t.Fatalf("cursor = %q", cur)
	}
	byID := map[string]Page{}
	for _, p := range pages {
		byID[p.ID] = p
	}
	if p := byID["2026-11-11-1430-cursor"]; !strings.HasPrefix(p.Title, "Refactor the syncer") || strings.Contains(p.Markdown, "# Dictated into Cursor") {
		t.Errorf("markdown export page = %q\n%s", p.Title, p.Markdown)
	}
	if p := byID["m-fm"]; p.Attrs["app"] != "com.apple.Notes" || !p.UpdatedAt.Equal(time.Date(2026, 11, 10, 16, 20, 0, 0, time.UTC)) {
		t.Errorf("front-matter page = %v %v", p.Attrs, p.UpdatedAt)
	}
	if p := byID["c-1"]; p.Attrs["num_words"] != "13" || !strings.Contains(p.Markdown, "**App:** Slack") {
		t.Errorf("csv page = %v\n%s", p.Attrs, p.Markdown)
	}
	if p := byID["j-1"]; !strings.Contains(p.Markdown, "**App:** Wispr Flow") {
		t.Errorf("json page:\n%s", p.Markdown)
	}
}

func TestWisprExportDirPartial(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good.ndjson"), []byte(`{"id":"g","text":"three words here","at":"2026-11-11T10:00:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestWispr(t, map[string]string{"path": dir})
	pages, cur, err := collectWispr(t, c, "")
	if !IsPartial(err) || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("err = %v, want partial naming bad.json", err)
	}
	if len(pages) != 1 || pages[0].ID != "g" || cur != "2026-11-11T10:00:00Z" {
		t.Fatalf("pages = %v cur = %q", wisprIDs(pages), cur)
	}
}

func TestWisprParseTime(t *testing.T) {
	cases := map[string]time.Time{
		"2026-11-11 17:55:30.000 +00:00": time.Date(2026, 11, 11, 17, 55, 30, 0, time.UTC),
		"2026-11-11 19:55:30.250 +02:00": time.Date(2026, 11, 11, 17, 55, 30, 250e6, time.UTC),
		"2026-11-11T17:55:30Z":           time.Date(2026, 11, 11, 17, 55, 30, 0, time.UTC),
		"2026-11-11":                     time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC),
		"garbage":                        {},
	}
	for in, want := range cases {
		if got := wisprParseTime(in); !got.Equal(want) {
			t.Errorf("wisprParseTime(%q) = %v, want %v", in, got, want)
		}
	}
	if got, ok := wisprTimeFromName("2026-11-11T14-05-09.md"); !ok || !got.Equal(time.Date(2026, 11, 11, 14, 5, 9, 0, time.UTC)) {
		t.Errorf("wisprTimeFromName = %v %v", got, ok)
	}
	if _, ok := wisprTimeFromName("notes.md"); ok {
		t.Error("wisprTimeFromName matched a name without a date")
	}
}

func TestWisprAppName(t *testing.T) {
	for in, want := range map[string]string{"com.tinyspeck.slackmacgap": "Slack", "com.example.foo": "foo", "": "", "Cursor": "Cursor"} {
		if got := wisprAppName(in); got != want {
			t.Errorf("wisprAppName(%q) = %q, want %q", in, got, want)
		}
	}
}

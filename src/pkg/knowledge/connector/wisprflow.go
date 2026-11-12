package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TypeWisprFlow is the connector type for Wispr Flow dictation history,
// read from the desktop app's local SQLite file or from an export folder.
const TypeWisprFlow = "wisprflow"

const (
	// wisprDefaultSQLite3 is the sqlite3 CLI used to read flow.sqlite; it
	// ships with macOS and the hive image, so no Go SQLite driver is needed.
	wisprDefaultSQLite3 = "sqlite3"
	// wisprDefaultSinceDays bounds the first sync.
	wisprDefaultSinceDays = 30
	// wisprDefaultMaxItems caps dictations read per sync.
	wisprDefaultMaxItems = 500
	// wisprDefaultMinWords drops one- and two-word dictations ("yes", "ok").
	wisprDefaultMinWords = 3
	// wisprRelistOverlap re-reads this far behind the cursor: Flow updates a
	// row in place (raw_transcript → formatted), so a late finalisation is
	// still picked up.
	wisprRelistOverlap = 2 * time.Hour
	// wisprGroupDictation emits one fact per dictation; wisprGroupDay one
	// fact per calendar day.
	wisprGroupDictation = "dictation"
	wisprGroupDay       = "day"
	// wisprTitleWords is how many words of the text make the fact title.
	wisprTitleWords = 10
	// wisprSQLTimeout bounds one sqlite3 invocation.
	wisprSQLTimeout = 60 * time.Second
	// wisprStatusFormatted is the terminal row status in Flow's History table.
	wisprStatusFormatted = "formatted"
)

// wisprDictation is one Wispr Flow history row, normalised from SQLite or
// an export file.
type wisprDictation struct {
	ID       string
	Text     string
	RawText  string
	At       time.Time
	App      string
	URL      string
	Duration float64
	Words    int
	Language string
}

// wisprConnector reads Wispr Flow's local dictation history. Scope keys:
// path (flow.sqlite, a backup copy, or an export folder), sqlite3, since_days,
// max_items, min_words, group (dictation|day), apps, include_raw, tags.
// No auth: the data is already on local disk.
type wisprConnector struct {
	cfg    ConnectorConfig
	logger *slog.Logger
	now    func() time.Time
	// query runs the sqlite3 CLI; tests replace it.
	query func(ctx context.Context, dbPath, sql string) ([]byte, error)
}

func newWisprConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	c := &wisprConnector{cfg: cfg, logger: deps.logger(), now: time.Now}
	bin := cfg.ScopeValue("sqlite3")
	if bin == "" {
		bin = wisprDefaultSQLite3
	}
	c.query = wisprSQLite3Runner(bin)
	return c, nil
}

func (c *wisprConnector) Type() string { return TypeWisprFlow }

// wisprDefaultPath is where the desktop app keeps its history on this OS.
func wisprDefaultPath() string {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, "Library", "Application Support", "Wispr Flow", "flow.sqlite")
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return filepath.Join(appdata, "Wispr Flow", "flow.sqlite")
		}
	}
	return ""
}

func (c *wisprConnector) path() string {
	if p := c.cfg.ScopeValue("path"); p != "" {
		return p
	}
	return wisprDefaultPath()
}

// Validate checks the scope. The path may not exist yet (the app may be
// installed later); only its shape is checked.
func (c *wisprConnector) Validate(cfg ConnectorConfig) error {
	if cfg.Auth.Configured() {
		return fmt.Errorf("wisprflow: auth.env/auth.file are not used; the connector reads Wispr Flow's local history file")
	}
	p := cfg.ScopeValue("path")
	if p == "" && wisprDefaultPath() == "" {
		return fmt.Errorf("wisprflow: scope.path is required on %s (no default Wispr Flow location)", runtime.GOOS)
	}
	if p != "" && !filepath.IsAbs(p) {
		return fmt.Errorf("wisprflow: scope.path must be an absolute path to flow.sqlite or an export folder")
	}
	for _, key := range []string{"since_days", "max_items", "min_words"} {
		if v := cfg.ScopeValue(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || (key != "min_words" && n == 0) {
				return fmt.Errorf("wisprflow: scope.%s must be a positive integer", key)
			}
		}
	}
	if g := cfg.ScopeValue("group"); g != "" && g != wisprGroupDictation && g != wisprGroupDay {
		return fmt.Errorf("wisprflow: scope.group must be %q or %q", wisprGroupDictation, wisprGroupDay)
	}
	if v := cfg.ScopeValue("include_raw"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return fmt.Errorf("wisprflow: scope.include_raw must be true or false")
		}
	}
	if bin := cfg.ScopeValue("sqlite3"); bin != "" {
		if _, err := splitCommandLine(bin); err != nil {
			return err
		}
	}
	return nil
}

func (c *wisprConnector) scopeInt(key string, def int) int {
	v := c.cfg.ScopeValue(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func (c *wisprConnector) group() string {
	if c.cfg.ScopeValue("group") == wisprGroupDay {
		return wisprGroupDay
	}
	return wisprGroupDictation
}

// Sync reads dictations newer than the cursor (minus an overlap) and emits
// one page per dictation or per day. The listing is incremental, so unlisted
// pages are never tombstoned.
func (c *wisprConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	p := c.path()
	if p == "" {
		return cur, fmt.Errorf("wisprflow: no path configured")
	}
	info, err := os.Stat(p)
	if err != nil {
		return cur, fmt.Errorf("wisprflow: %w (install Wispr Flow, point scope.path at flow.sqlite or an export folder, or grant the hive process Full Disk Access on macOS)", err)
	}
	since := parsePlaudCursor(cur)
	if since.IsZero() {
		since = c.now().Add(-time.Duration(c.scopeInt("since_days", wisprDefaultSinceDays)) * 24 * time.Hour)
	} else {
		since = since.Add(-wisprRelistOverlap)
	}
	if c.group() == wisprGroupDay {
		// A day page is rebuilt whole, so re-read from the start of that day.
		y, m, d := since.UTC().Date()
		since = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	maxItems := c.scopeInt("max_items", wisprDefaultMaxItems)

	var rows []wisprDictation
	var partial error
	if info.IsDir() {
		rows, partial = readWisprExportDir(p, since, maxItems)
	} else {
		rows, err = c.readSQLite(ctx, p, since, maxItems)
		if err != nil {
			return cur, err
		}
	}
	rows = c.filter(rows)
	sort.Slice(rows, func(i, j int) bool { return rows[i].At.Before(rows[j].At) })

	newest := parsePlaudCursor(cur)
	for _, r := range rows {
		if r.At.After(newest) {
			newest = r.At
		}
	}
	pages := c.pages(rows)
	for _, pg := range pages {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		if err := emit(pg); err != nil {
			return cur, err
		}
	}
	next := cur
	if !newest.IsZero() {
		next = Cursor(newest.UTC().Format(time.RFC3339))
	}
	if partial != nil {
		return next, &PartialError{Err: partial}
	}
	return next, nil
}

// filter applies min_words and the apps allow-list.
func (c *wisprConnector) filter(rows []wisprDictation) []wisprDictation {
	minWords := c.scopeInt("min_words", wisprDefaultMinWords)
	var apps []string
	for _, a := range strings.Split(c.cfg.ScopeValue("apps"), ",") {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			apps = append(apps, a)
		}
	}
	out := rows[:0]
	for _, r := range rows {
		r.Text = strings.TrimSpace(r.Text)
		if r.Text == "" || r.ID == "" {
			continue
		}
		words := r.Words
		if words <= 0 {
			words = len(strings.Fields(r.Text))
		}
		if words < minWords {
			continue
		}
		if len(apps) > 0 && !wisprAppMatches(r.App, apps) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func wisprAppMatches(app string, allow []string) bool {
	app = strings.ToLower(app)
	for _, a := range allow {
		if strings.Contains(app, a) {
			return true
		}
	}
	return false
}

// pages renders one page per dictation, or one per UTC day.
func (c *wisprConnector) pages(rows []wisprDictation) []Page {
	includeRaw, _ := strconv.ParseBool(c.cfg.ScopeValue("include_raw"))
	tags := c.cfg.ScopeValue("tags")
	if c.group() == wisprGroupDictation {
		out := make([]Page, 0, len(rows))
		for _, r := range rows {
			out = append(out, c.dictationPage(r, includeRaw, tags))
		}
		return out
	}
	byDay := map[string][]wisprDictation{}
	var days []string
	for _, r := range rows {
		day := r.At.UTC().Format("2006-01-02")
		if _, ok := byDay[day]; !ok {
			days = append(days, day)
		}
		byDay[day] = append(byDay[day], r)
	}
	sort.Strings(days)
	out := make([]Page, 0, len(days))
	for _, day := range days {
		out = append(out, c.dayPage(day, byDay[day], includeRaw, tags))
	}
	return out
}

func wisprTitle(text string) string {
	words := strings.Fields(text)
	if len(words) > wisprTitleWords {
		return strings.Join(words[:wisprTitleWords], " ") + "…"
	}
	return strings.Join(words, " ")
}

// wisprAppName turns a bundle id like com.tinyspeck.slackmacgap into a
// readable label; unknown ids keep their last segment.
func wisprAppName(app string) string {
	app = strings.TrimSpace(app)
	if app == "" {
		return ""
	}
	known := map[string]string{
		"com.tinyspeck.slackmacgap":      "Slack",
		"md.obsidian":                    "Obsidian",
		"com.apple.MobileSMS":            "Messages",
		"com.microsoft.VSCode":           "VS Code",
		"com.google.Chrome":              "Chrome",
		"com.apple.Safari":               "Safari",
		"com.electron.wispr-flow":        "Wispr Flow",
		"com.openai.chat":                "ChatGPT",
		"com.anthropic.claudefordesktop": "Claude",
		"com.apple.Notes":                "Notes",
		"com.apple.mail":                 "Mail",
		"com.apple.Terminal":             "Terminal",
		"com.googlecode.iterm2":          "iTerm2",
		"com.todesktop.230313mzl4w4u92":  "Cursor",
	}
	if n, ok := known[app]; ok {
		return n
	}
	if i := strings.LastIndex(app, "."); i >= 0 && i < len(app)-1 {
		return app[i+1:]
	}
	return app
}

func (c *wisprConnector) dictationPage(r wisprDictation, includeRaw bool, tags string) Page {
	appName := wisprAppName(r.App)
	title := wisprTitle(r.Text)
	var md strings.Builder
	fmt.Fprintf(&md, "# %s\n\n", title)
	fmt.Fprintf(&md, "- **Dictated:** %s\n", r.At.UTC().Format(time.RFC3339))
	if appName != "" {
		fmt.Fprintf(&md, "- **App:** %s\n", appName)
	}
	if r.URL != "" {
		fmt.Fprintf(&md, "- **URL:** %s\n", r.URL)
	}
	if r.Duration > 0 {
		fmt.Fprintf(&md, "- **Duration:** %s\n", (time.Duration(r.Duration * float64(time.Second))).Round(time.Second))
	}
	md.WriteString("\n## Transcript\n\n")
	md.WriteString(r.Text)
	md.WriteString("\n")
	if includeRaw && strings.TrimSpace(r.RawText) != "" && strings.TrimSpace(r.RawText) != r.Text {
		md.WriteString("\n## Raw transcript\n\n")
		md.WriteString(strings.TrimSpace(r.RawText))
		md.WriteString("\n")
	}
	attrs := map[string]string{
		"wispr_transcript_id": r.ID,
		"dictated_at":         r.At.UTC().Format(time.RFC3339),
		"recorded_at":         r.At.UTC().Format(time.RFC3339),
	}
	if r.App != "" {
		attrs["app"] = r.App
	}
	if r.Words > 0 {
		attrs["num_words"] = strconv.Itoa(r.Words)
	}
	if r.Language != "" {
		attrs["language"] = r.Language
	}
	if tags != "" {
		attrs["tags"] = tags
	}
	return Page{
		ID:        r.ID,
		Title:     title,
		URL:       r.URL,
		Markdown:  md.String(),
		UpdatedAt: r.At,
		Path:      []string{"Wispr Flow", r.At.UTC().Format("2006-01-02")},
		Attrs:     attrs,
	}
}

func (c *wisprConnector) dayPage(day string, rows []wisprDictation, includeRaw bool, tags string) Page {
	title := "Wispr Flow dictations " + day
	var md strings.Builder
	fmt.Fprintf(&md, "# %s\n\n", title)
	fmt.Fprintf(&md, "%d dictation(s).\n\n", len(rows))
	var newest time.Time
	words := 0
	for _, r := range rows {
		if r.At.After(newest) {
			newest = r.At
		}
		words += r.Words
		label := r.At.UTC().Format("15:04")
		if n := wisprAppName(r.App); n != "" {
			label += " · " + n
		}
		fmt.Fprintf(&md, "## %s\n\n%s\n\n", label, r.Text)
		if includeRaw && strings.TrimSpace(r.RawText) != "" && strings.TrimSpace(r.RawText) != r.Text {
			fmt.Fprintf(&md, "<details><summary>Raw transcript</summary>\n\n%s\n\n</details>\n\n", strings.TrimSpace(r.RawText))
		}
	}
	attrs := map[string]string{"day": day, "dictations": strconv.Itoa(len(rows)), "recorded_at": newest.UTC().Format(time.RFC3339)}
	if words > 0 {
		attrs["num_words"] = strconv.Itoa(words)
	}
	if tags != "" {
		attrs["tags"] = tags
	}
	return Page{
		ID:        "day-" + day,
		Title:     title,
		Markdown:  md.String(),
		UpdatedAt: newest,
		Path:      []string{"Wispr Flow"},
		Attrs:     attrs,
	}
}

// wisprSQLite3Runner executes the sqlite3 CLI read-only with JSON output.
// The History table holds audio and screenshot BLOBs, so only text columns
// are ever selected.
func wisprSQLite3Runner(bin string) func(ctx context.Context, dbPath, sql string) ([]byte, error) {
	return func(ctx context.Context, dbPath, sql string) ([]byte, error) {
		argv, err := splitCommandLine(bin)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, wisprSQLTimeout)
		defer cancel()
		args := append(argv[1:], "-readonly", "-json", "-bail", dbPath, sql)
		cmd := exec.CommandContext(ctx, argv[0], args...)
		out, err := cmd.Output()
		if err != nil {
			var stderr string
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = firstLine(string(ee.Stderr))
			}
			if stderr == "" {
				stderr = err.Error()
			}
			return nil, fmt.Errorf("wisprflow: sqlite3 failed: %s", stderr)
		}
		return out, nil
	}
}

// wisprHistorySQL selects finalised, unarchived dictations newer than since.
// Flow stores Sequelize DATETIME strings; datetime() normalises them so the
// comparison does not depend on the stored format.
func wisprHistorySQL(since time.Time, limit int) string {
	s := strings.ReplaceAll(since.UTC().Format("2006-01-02 15:04:05"), "'", "''")
	return fmt.Sprintf(`SELECT transcriptEntityId AS id, `+
		`COALESCE(NULLIF(editedText,''), NULLIF(formattedText,''), asrText) AS text, `+
		`asrText AS raw, timestamp AS at, COALESCE(app,'') AS app, COALESCE(url,'') AS url, `+
		`COALESCE(duration,0) AS duration, COALESCE(numWords,0) AS numWords, COALESCE(language,'') AS language `+
		`FROM History WHERE COALESCE(isArchived,0)=0 AND status='%s' AND datetime(timestamp) > datetime('%s') `+
		`ORDER BY datetime(timestamp) ASC LIMIT %d;`, wisprStatusFormatted, s, limit)
}

func (c *wisprConnector) readSQLite(ctx context.Context, dbPath string, since time.Time, limit int) ([]wisprDictation, error) {
	out, err := c.query(ctx, dbPath, wisprHistorySQL(since, limit))
	if err != nil {
		return nil, err
	}
	return decodeWisprJSON(out)
}

// decodeWisprJSON accepts the sqlite3 -json array (or any JSON array/object
// list of dictations, as written by community exporters).
func decodeWisprJSON(data []byte) ([]wisprDictation, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, nil
	}
	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("wisprflow: invalid JSON: %s", firstLine(text))
	}
	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case map[string]any:
		for _, key := range []string{"dictations", "history", "items", "data", "results", "transcripts"} {
			if arr, ok := v[key].([]any); ok {
				items = arr
				break
			}
		}
		if items == nil {
			items = []any{v}
		}
	}
	out := make([]wisprDictation, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if d, ok := wisprFromMap(m); ok {
			out = append(out, d)
		}
	}
	return out, nil
}

// wisprFromMap normalises one dictation object from SQLite JSON or an
// exporter's JSON, which name the same fields differently.
func wisprFromMap(m map[string]any) (wisprDictation, bool) {
	d := wisprDictation{
		ID:       plaudString(m, "id", "transcriptEntityId", "transcript_id", "uuid"),
		Text:     plaudString(m, "text", "editedText", "edited_text", "formattedText", "formatted_text", "content", "asrText"),
		RawText:  plaudString(m, "raw", "asrText", "asr_text", "raw_text"),
		At:       plaudTime(m, "at", "timestamp", "created_at", "createdAt", "date"),
		App:      plaudString(m, "app", "application", "bundle_id"),
		URL:      plaudString(m, "url"),
		Language: plaudString(m, "language", "lang"),
	}
	d.Duration = wisprFloat(m, "duration", "duration_s", "seconds")
	d.Words = int(wisprFloat(m, "numWords", "num_words", "words"))
	if d.At.IsZero() {
		d.At = wisprParseTime(plaudString(m, "at", "timestamp", "created_at", "createdAt", "date"))
	}
	if d.ID == "" || strings.TrimSpace(d.Text) == "" || d.At.IsZero() {
		return d, false
	}
	return d, true
}

// wisprParseTime handles Sequelize's "2026-11-11 14:05:09.123 +00:00" and
// the common ISO shapes; plaudTime covers RFC3339 and epoch numbers.
func wisprParseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.000 -07:00",
		"2006-01-02 15:04:05 -07:00",
		"2006-01-02 15:04:05.000Z07:00",
		"2006-01-02 15:04:05.000",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.000Z07:00",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// wisprFloat reads the first numeric value among keys, accepting JSON
// numbers and numeric strings (CSV exports).
func wisprFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return v
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return f
			}
		}
	}
	return 0
}

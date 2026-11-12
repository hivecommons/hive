package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypePlaud is the connector type for Plaud voice-note recordings, read
// through the official (free) Plaud MCP server over stdio.
const TypePlaud = "plaud"

const (
	// plaudDefaultCommand launches the official Plaud MCP server.
	plaudDefaultCommand = "npx -y @plaud-ai/mcp@latest"
	// plaudDefaultSinceDays bounds the first sync.
	plaudDefaultSinceDays = 30
	// plaudDefaultMaxFiles caps recordings fetched per sync.
	plaudDefaultMaxFiles = 200
	// plaudListPageSize is the list_files page size requested.
	plaudListPageSize = 50
	// plaudRelistOverlap re-lists this far behind the cursor so late
	// transcripts and edited summaries are picked up.
	plaudRelistOverlap = 24 * time.Hour
	// plaudLoginWindow is how long a login session stays alive to receive
	// the OAuth callback.
	plaudLoginWindow = 10 * time.Minute
	// plaudLoginReplyWait bounds how long Connect waits for the login tool
	// to answer before returning whatever URL has surfaced so far.
	plaudLoginReplyWait = 8 * time.Second
	// plaudHomeInherit keeps the hive process HOME for the MCP server.
	plaudHomeInherit = "inherit"
)

// plaudConnector syncs Plaud recordings: list_files → get_transcript +
// get_note → one page per recording. Scope keys: command, home, since_days,
// max_files, include_audio, query, tags. No auth.env/auth.file: Plaud's
// OAuth token lives in the MCP server's own store ($HOME/.plaud).
type plaudConnector struct {
	cfg      ConnectorConfig
	logger   *slog.Logger
	dial     mcpDialer
	now      func() time.Time
	stateDir string
	// home is the MCP server's HOME (token store); empty inherits the
	// process HOME.
	home string
	// callbackBase is where a pasted OAuth redirect is forwarded while a
	// login session is alive (the MCP server's fixed localhost listener).
	callbackBase string

	mu    sync.Mutex
	login *plaudLogin
	auth  AuthState
}

// plaudLogin is an in-flight OAuth login kept alive for the callback.
type plaudLogin struct {
	client mcpClient
	cancel context.CancelFunc
	result string
	done   chan struct{}
}

func newPlaudConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	c := &plaudConnector{cfg: cfg, logger: deps.logger(), now: time.Now, stateDir: deps.StateDir}
	home := cfg.ScopeValue("home")
	switch {
	case home == plaudHomeInherit:
		home = ""
	case home == "" && deps.StateDir != "":
		home = filepath.Join(deps.StateDir, "home")
	}
	if _, err := ensureDir(home); err != nil {
		return nil, fmt.Errorf("plaud: %w", err)
	}
	command := cfg.ScopeValue("command")
	if command == "" {
		command = plaudDefaultCommand
	}
	c.dial = stdioMCPDialer(command, home)
	c.home = home
	c.callbackBase = plaudCallbackBase
	return c, nil
}

func (c *plaudConnector) Type() string { return TypePlaud }

// Validate checks the scope. Plaud never takes a hive-managed credential.
func (c *plaudConnector) Validate(cfg ConnectorConfig) error {
	if cfg.Auth.Configured() {
		return fmt.Errorf("plaud: auth.env/auth.file are not used; Plaud signs in through its own OAuth flow (Connect in the dashboard)")
	}
	if cmd := cfg.ScopeValue("command"); cmd != "" {
		if _, err := splitCommandLine(cmd); err != nil {
			return err
		}
	}
	if h := cfg.ScopeValue("home"); h != "" && h != plaudHomeInherit && !filepath.IsAbs(h) {
		return fmt.Errorf("plaud: scope.home must be an absolute path or %q", plaudHomeInherit)
	}
	for _, key := range []string{"since_days", "max_files"} {
		if v := cfg.ScopeValue(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return fmt.Errorf("plaud: scope.%s must be a positive integer", key)
			}
		}
	}
	if v := cfg.ScopeValue("include_audio"); v != "" {
		if _, err := strconv.ParseBool(v); err != nil {
			return fmt.Errorf("plaud: scope.include_audio must be true or false")
		}
	}
	return nil
}

func (c *plaudConnector) scopeInt(key string, def int) int {
	n, err := strconv.Atoi(c.cfg.ScopeValue(key))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func (c *plaudConnector) includeAudio() bool {
	b, _ := strconv.ParseBool(c.cfg.ScopeValue("include_audio"))
	return b
}

// plaudCursor is the newest recording time seen, RFC3339.
func parsePlaudCursor(cur Cursor) time.Time {
	t, err := time.Parse(time.RFC3339, string(cur))
	if err != nil {
		return time.Time{}
	}
	return t
}

// Sync lists recordings created since the cursor (minus an overlap window)
// and emits one page per recording. The listing is incremental, so unlisted
// pages are never tombstoned.
func (c *plaudConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	client, err := c.dial(ctx)
	if err != nil {
		return cur, err
	}
	defer func() { _ = client.Close() }()

	since := parsePlaudCursor(cur)
	if since.IsZero() {
		since = c.now().Add(-time.Duration(c.scopeInt("since_days", plaudDefaultSinceDays)) * 24 * time.Hour)
	} else {
		since = since.Add(-plaudRelistOverlap)
	}
	files, err := c.listFiles(ctx, client, since, c.scopeInt("max_files", plaudDefaultMaxFiles))
	if err != nil {
		return cur, err
	}
	newest := parsePlaudCursor(cur)
	var skipped []string
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		page, err := c.fetchPage(ctx, client, f)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", f.ID, err))
			continue
		}
		if err := emit(page); err != nil {
			return cur, err
		}
		if f.CreatedAt.After(newest) {
			newest = f.CreatedAt
		}
	}
	c.refreshAuth(ctx, client)
	next := cur
	if !newest.IsZero() {
		next = Cursor(newest.UTC().Format(time.RFC3339))
	}
	if len(skipped) > 0 {
		return next, &PartialError{Err: fmt.Errorf("plaud: %d recording(s) skipped: %s", len(skipped), strings.Join(skipped, "; "))}
	}
	return next, nil
}

// listFiles pages through list_files until a page is empty, repeats, or the
// cap is hit. Recordings older than since are dropped client-side as well,
// in case the server ignores date_from.
func (c *plaudConnector) listFiles(ctx context.Context, client mcpClient, since time.Time, maxFiles int) ([]plaudFile, error) {
	seen := map[string]bool{}
	var out []plaudFile
	for page := 1; len(out) < maxFiles; page++ {
		args := map[string]any{"page": page, "page_size": plaudListPageSize}
		if !since.IsZero() {
			args["date_from"] = since.UTC().Format("2006-01-02")
		}
		if q := c.cfg.ScopeValue("query"); q != "" {
			args["query"] = q
		}
		text, err := client.Call(ctx, "list_files", args)
		if err != nil {
			return nil, plaudAuthError(err)
		}
		files, err := decodePlaudFiles(text)
		if err != nil {
			return nil, err
		}
		added := 0
		for _, f := range files {
			if f.ID == "" || seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			if !since.IsZero() && !f.CreatedAt.IsZero() && f.CreatedAt.Before(since) {
				continue
			}
			out = append(out, f)
			added++
			if len(out) >= maxFiles {
				break
			}
		}
		if added == 0 || len(files) < plaudListPageSize {
			break
		}
	}
	return out, nil
}

// fetchPage builds the knowledge page for one recording.
func (c *plaudConnector) fetchPage(ctx context.Context, client mcpClient, f plaudFile) (Page, error) {
	note, transcript, err := c.fetchNoteAndTranscript(ctx, client, f.ID)
	if err != nil {
		return Page{}, err
	}
	audioURL := ""
	if c.includeAudio() {
		if text, err := client.Call(ctx, "get_file", map[string]any{"file_id": f.ID}); err == nil {
			audioURL = plaudPresignedURL(text)
		}
	}
	title := strings.TrimSpace(f.Name)
	if title == "" {
		title = "Plaud recording " + f.ID
	}
	var md strings.Builder
	fmt.Fprintf(&md, "# %s\n\n", title)
	if !f.StartAt.IsZero() {
		fmt.Fprintf(&md, "- **Recorded:** %s\n", f.StartAt.UTC().Format(time.RFC3339))
	} else if !f.CreatedAt.IsZero() {
		fmt.Fprintf(&md, "- **Recorded:** %s\n", f.CreatedAt.UTC().Format(time.RFC3339))
	}
	if f.DurationMS > 0 {
		fmt.Fprintf(&md, "- **Duration:** %s\n", (time.Duration(f.DurationMS) * time.Millisecond).Round(time.Second))
	}
	if audioURL != "" {
		fmt.Fprintf(&md, "- **Audio (24h link):** %s\n", audioURL)
	}
	md.WriteString("\n")
	if strings.TrimSpace(note) != "" {
		md.WriteString("## Summary (Plaud)\n\n")
		md.WriteString(strings.TrimSpace(note))
		md.WriteString("\n\n")
	}
	if strings.TrimSpace(transcript) != "" {
		md.WriteString("## Transcript\n\n")
		md.WriteString(strings.TrimSpace(transcript))
		md.WriteString("\n")
	}
	attrs := map[string]string{"plaud_file_id": f.ID}
	if f.DurationMS > 0 {
		attrs["duration_ms"] = strconv.FormatInt(f.DurationMS, 10)
	}
	if !f.StartAt.IsZero() {
		attrs["recorded_at"] = f.StartAt.UTC().Format(time.RFC3339)
	}
	if tags := c.cfg.ScopeValue("tags"); tags != "" {
		attrs["tags"] = tags
	}
	if f.Serial != "" {
		attrs["device_serial"] = f.Serial
	}
	updated := f.CreatedAt
	if f.StartAt.After(updated) {
		updated = f.StartAt
	}
	return Page{
		ID:        f.ID,
		Title:     title,
		Markdown:  md.String(),
		UpdatedAt: updated,
		Path:      []string{"Plaud"},
		Attrs:     attrs,
	}, nil
}

// plaudAuthError rewrites "not authenticated" tool errors into a hint that
// points at the dashboard Connect button.
func plaudAuthError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "401") || strings.Contains(msg, "not authenticated") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "login") {
		return fmt.Errorf("plaud: not connected — use Connect in the Knowledge → Integrations → Plaud card (%w)", err)
	}
	return err
}

// Attrs returned by the MCP server for one recording.
type plaudFile struct {
	ID         string
	Name       string
	CreatedAt  time.Time
	StartAt    time.Time
	DurationMS int64
	Serial     string
}

// decodePlaudFiles accepts a JSON array of recordings, or an object wrapping
// one under files/items/data/results/recordings.
func decodePlaudFiles(text string) ([]plaudFile, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		if i := strings.IndexAny(text, "[{"); i > 0 {
			return decodePlaudFiles(text[i:])
		}
		return nil, fmt.Errorf("plaud: list_files returned non-JSON output: %s", firstLine(text))
	}
	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case map[string]any:
		for _, key := range []string{"files", "items", "data", "results", "recordings"} {
			if arr, ok := v[key].([]any); ok {
				items = arr
				break
			}
		}
		if items == nil {
			if _, ok := v["id"]; ok {
				items = []any{v}
			}
		}
	}
	out := make([]plaudFile, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		f := plaudFile{
			ID:        plaudString(m, "id", "file_id"),
			Name:      plaudString(m, "name", "title", "filename"),
			Serial:    plaudString(m, "serial_number"),
			CreatedAt: plaudTime(m, "created_at", "create_time"),
			StartAt:   plaudTime(m, "start_at", "start_time"),
		}
		if d, ok := m["duration"].(float64); ok {
			f.DurationMS = int64(d)
		}
		if f.ID != "" {
			out = append(out, f)
		}
	}
	return out, nil
}

func plaudString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

func plaudTime(m map[string]any, keys ...string) time.Time {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
				if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
					return t
				}
			}
		case float64:
			// Epoch seconds or milliseconds.
			if v > 1e12 {
				return time.UnixMilli(int64(v)).UTC()
			}
			if v > 0 {
				return time.Unix(int64(v), 0).UTC()
			}
		}
	}
	return time.Time{}
}

// fetchNoteAndTranscript renders the note and transcript of one recording.
// It fails only when neither is available.
func (c *plaudConnector) fetchNoteAndTranscript(ctx context.Context, client mcpClient, id string) (note, transcript string, err error) {
	noteText, noteErr := client.Call(ctx, "get_note", map[string]any{"file_id": id})
	transcriptText, trErr := client.Call(ctx, "get_transcript", map[string]any{"file_id": id})
	if noteErr != nil && trErr != nil {
		return "", "", plaudAuthError(trErr)
	}
	if noteErr != nil {
		noteText = ""
	}
	if trErr != nil {
		transcriptText = ""
	}
	note = renderPlaudNote(noteText)
	transcript = renderPlaudTranscript(transcriptText)
	if strings.TrimSpace(note) == "" && strings.TrimSpace(transcript) == "" {
		return "", "", fmt.Errorf("no note or transcript available yet")
	}
	return note, transcript, nil
}

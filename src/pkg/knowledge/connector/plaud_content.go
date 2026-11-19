package connector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Transcript blocks the Plaud MCP server (@plaud-ai/mcp 0.3.14) exposes
// through get_transcript's `block` argument. A recording only carries the
// blocks Plaud has produced for it: a fresh upload that was never transcribed
// in the Plaud app has none (or only mark_memo when the device button was
// pressed), and asking for a missing block returns the plain-text reply
// `Block "transaction" not available for this recording. Available blocks:
// mark_memo.` rather than an error.
const (
	// plaudBlockTranscript is the raw transcript (speaker + timestamps).
	plaudBlockTranscript = "transaction"
	// plaudBlockPolished is the AI-cleaned transcript, same shape.
	plaudBlockPolished = "transaction_polish"
	// plaudBlockOutline is the outline block.
	plaudBlockOutline = "outline"
	// plaudBlockMarks holds the moments flagged with the device's highlight
	// button, returned under `marks`.
	plaudBlockMarks = "mark_memo"
)

const (
	// plaudTranscriptPageLimit is the utterances requested per get_transcript
	// page (the schema's maximum).
	plaudTranscriptPageLimit = 500
	// plaudTranscriptMaxPages bounds how many next_cursor pages one
	// recording's transcript may span.
	plaudTranscriptMaxPages = 40
	// plaudPendingRefetchCap bounds how many recordings whose fact still
	// lacks a summary and transcript are re-fetched per sync, on top of the
	// listing, so a backlog never turns every sync into a full refetch.
	plaudPendingRefetchCap = 25
	// plaudAudioLinkTTL is how long Plaud's presigned audio URL stays valid.
	plaudAudioLinkTTL = 24 * time.Hour
	// plaudContentAttr is the fact attr recording whether the page carries
	// text: plaudContentReady or plaudContentPending.
	plaudContentAttr    = "plaud_content"
	plaudContentReady   = FactContentReady
	plaudContentPending = FactContentPending
	// PlaudPendingText is written into a fact (and shown by the Inbox) when
	// Plaud has produced neither a summary nor a transcript for a recording.
	PlaudPendingText = "Plaud has not transcribed this recording yet. Open it in the Plaud app to run transcription and the AI summary; the hive re-checks it on every sync until text arrives."
)

// plaudTranscriptBlockOrder is the preference order for the transcript
// section: the cleaned transcript, the raw one, then the outline.
var plaudTranscriptBlockOrder = []string{plaudBlockPolished, plaudBlockTranscript, plaudBlockOutline}

// plaudContent is everything one recording offers as text.
type plaudContent struct {
	Summary    string
	Transcript string
	// Highlights lists the flagged moments (mark_memo) as "- mm:ss" lines.
	Highlights string
	// TranscriptBlock names the block Transcript was read from.
	TranscriptBlock string
	// Blocks are the transcript blocks the server reports for the recording.
	Blocks []string
	// AudioURL is the presigned link get_file returned (24h). It is never
	// stored unless scope.include_audio is true.
	AudioURL string
}

// Pending reports that neither a summary nor a transcript exists yet.
func (c plaudContent) Pending() bool {
	return strings.TrimSpace(c.Summary) == "" && strings.TrimSpace(c.Transcript) == ""
}

// plaudInventory is the block inventory get_file returns.
type plaudInventory struct {
	PresignedURL string
	Blocks       []string
	Notes        int
}

// decodePlaudInventory reads get_file's source_list/note_list/presigned_url.
func decodePlaudInventory(text string) (plaudInventory, bool) {
	raw, ok := plaudJSONPayload(text)
	if !ok {
		return plaudInventory{}, false
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return plaudInventory{}, false
	}
	inv := plaudInventory{PresignedURL: plaudString(m, "presigned_url", "audio_url", "url")}
	if src, ok := m["source_list"].([]any); ok {
		for _, it := range src {
			if sm, ok := it.(map[string]any); ok {
				if t := plaudString(sm, "data_type", "type", "block"); t != "" {
					inv.Blocks = append(inv.Blocks, t)
				}
			}
		}
	}
	if notes, ok := m["note_list"].([]any); ok {
		inv.Notes = len(notes)
	}
	return inv, true
}

// plaudUnavailableRE matches the MCP's reply for a block the recording does
// not have. Capture 2 is the comma-separated list of blocks it does have.
var plaudUnavailableRE = regexp.MustCompile(`Block "([^"]+)" not available for this recording\.?\s*Available blocks:\s*([^.\n]*)`)

// plaudBlockUnavailable reports whether text is the "block not available"
// reply and returns the blocks the server says are available (may be empty).
func plaudBlockUnavailable(text string) (available []string, ok bool) {
	m := plaudUnavailableRE.FindStringSubmatch(text)
	if m == nil {
		return nil, false
	}
	for _, b := range strings.Split(m[2], ",") {
		if b = strings.TrimSpace(b); b != "" && !strings.EqualFold(b, "none") {
			available = append(available, b)
		}
	}
	return available, true
}

// pickPlaudBlock returns the best transcript block among available, or ""
// when none of them carries a transcript.
func pickPlaudBlock(available []string) string {
	for _, want := range plaudTranscriptBlockOrder {
		for _, have := range available {
			if strings.EqualFold(have, want) {
				return want
			}
		}
	}
	return ""
}

func hasPlaudBlock(blocks []string, name string) bool {
	for _, b := range blocks {
		if strings.EqualFold(b, name) {
			return true
		}
	}
	return false
}

// fetchContent reads everything Plaud offers for one recording. It asks
// get_file for the block inventory first, reads notes only when some exist,
// reads the best available transcript block (following the "available
// blocks" hint when the inventory was not readable) and renders highlights.
// It fails only when every call failed, so a recording without text still
// becomes a page that later syncs refresh.
func (c *plaudConnector) fetchContent(ctx context.Context, client mcpClient, id string) (plaudContent, error) {
	var out plaudContent
	var lastErr error
	calls, failed := 0, 0

	invText, err := client.Call(ctx, "get_file", map[string]any{"file_id": id})
	calls++
	inv, haveInv := plaudInventory{}, false
	if err != nil {
		failed++
		lastErr = err
	} else {
		inv, haveInv = decodePlaudInventory(invText)
		out.AudioURL = inv.PresignedURL
		if out.AudioURL == "" {
			out.AudioURL = firstHTTPSURL(invText)
		}
	}
	if haveInv {
		out.Blocks = append(out.Blocks, inv.Blocks...)
	}

	if !haveInv || inv.Notes > 0 {
		noteText, err := client.Call(ctx, "get_note", map[string]any{"file_id": id})
		calls++
		if err != nil {
			failed++
			lastErr = err
		} else {
			out.Summary = renderPlaudNote(noteText)
		}
	}

	block := plaudBlockTranscript
	if haveInv {
		block = pickPlaudBlock(inv.Blocks)
	}
	if block != "" {
		text, available, err := c.fetchTranscriptBlock(ctx, client, id, block)
		calls++
		switch {
		case err != nil:
			failed++
			lastErr = err
		case available != nil:
			// The server named what the recording does have: use that.
			out.Blocks = mergeBlocks(out.Blocks, available)
			if retry := pickPlaudBlock(available); retry != "" && retry != block {
				if text, _, err := c.fetchTranscriptBlock(ctx, client, id, retry); err == nil && strings.TrimSpace(text) != "" {
					out.Transcript, out.TranscriptBlock = text, retry
				}
			}
		default:
			out.Transcript, out.TranscriptBlock = text, block
		}
	}

	if hasPlaudBlock(out.Blocks, plaudBlockMarks) {
		if text, _, err := c.fetchTranscriptBlock(ctx, client, id, plaudBlockMarks); err == nil {
			out.Highlights = renderPlaudHighlights(text)
		}
	}
	if calls > 0 && failed == calls {
		return plaudContent{}, plaudAuthError(lastErr)
	}
	return out, nil
}

func mergeBlocks(have, more []string) []string {
	for _, b := range more {
		if !hasPlaudBlock(have, b) {
			have = append(have, b)
		}
	}
	return have
}

// fetchTranscriptBlock reads one block, following next_cursor pages, and
// renders it. When the server answers "block not available" it returns the
// available blocks instead of text (available is non-nil, possibly empty).
func (c *plaudConnector) fetchTranscriptBlock(ctx context.Context, client mcpClient, id, block string) (text string, available []string, err error) {
	var b strings.Builder
	cursor := ""
	for page := 0; page < plaudTranscriptMaxPages; page++ {
		args := map[string]any{"file_id": id, "block": block, "limit": plaudTranscriptPageLimit}
		if cursor != "" {
			args["cursor"] = cursor
		}
		reply, err := client.Call(ctx, "get_transcript", args)
		if err != nil {
			return "", nil, err
		}
		if avail, ok := plaudBlockUnavailable(reply); ok {
			if avail == nil {
				avail = []string{}
			}
			return "", avail, nil
		}
		if block == plaudBlockMarks {
			// Marks are rendered by the caller from the raw JSON.
			b.WriteString(reply)
		} else {
			b.WriteString(renderPlaudTranscript(reply))
			b.WriteString("\n")
		}
		cursor = plaudNextCursor(reply)
		if cursor == "" {
			break
		}
	}
	return strings.TrimSpace(b.String()), nil, nil
}

// plaudNextCursor returns a non-empty next_cursor from a paged reply.
func plaudNextCursor(text string) string {
	raw, ok := plaudStructuredReply(text)
	if !ok {
		return ""
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	return plaudString(m, "next_cursor")
}

// renderPlaudHighlights turns a mark_memo reply into "- mm:ss" lines, one per
// flagged moment, in time order.
func renderPlaudHighlights(text string) string {
	raw, ok := plaudStructuredReply(text)
	if !ok {
		return ""
	}
	var marks []any
	switch t := raw.(type) {
	case []any:
		marks = t
	case map[string]any:
		marks, _ = t["marks"].([]any)
	}
	var stamps []string
	for _, mk := range marks {
		m, ok := mk.(map[string]any)
		if !ok {
			continue
		}
		if ts := plaudTimestamp(m); ts != "" {
			stamps = append(stamps, ts)
		} else if v, ok := m["timestamp"].(float64); ok {
			stamps = append(stamps, plaudTimestamp(map[string]any{"start_time": v}))
		}
	}
	sort.Strings(stamps)
	var b strings.Builder
	for _, s := range stamps {
		b.WriteString("- " + s + "\n")
	}
	return strings.TrimSpace(b.String())
}

// PlaudAudioLink is a fresh presigned link to a recording's audio.
type PlaudAudioLink struct {
	URL       string
	ExpiresAt time.Time
}

// PlaudAudioReader fetches a short-lived audio link for one recording. The
// link is minted on demand and never persisted by the hive.
type PlaudAudioReader interface {
	AudioLink(ctx context.Context, id string) (PlaudAudioLink, error)
}

var _ PlaudAudioReader = (*plaudConnector)(nil)

// ErrPlaudNoAudio is returned when Plaud has no audio file for a recording.
var ErrPlaudNoAudio = fmt.Errorf("plaud: no audio available for this recording")

// AudioLink implements PlaudAudioReader through get_file.
func (c *plaudConnector) AudioLink(ctx context.Context, id string) (PlaudAudioLink, error) {
	client, err := c.dial(ctx)
	if err != nil {
		return PlaudAudioLink{}, err
	}
	defer func() { _ = client.Close() }()
	text, err := client.Call(ctx, "get_file", map[string]any{"file_id": id})
	if err != nil {
		return PlaudAudioLink{}, plaudAuthError(err)
	}
	url := plaudPresignedURL(text)
	if url == "" {
		return PlaudAudioLink{}, ErrPlaudNoAudio
	}
	return PlaudAudioLink{URL: url, ExpiresAt: c.now().UTC().Add(plaudAudioLinkTTL)}, nil
}

// pendingRefetch returns recordings whose fact in the vault still lacks text
// (attr plaud_content: pending, or a pre-attr fact with no Summary/Transcript
// section, such as one that stored the MCP's "block not available" line) and
// that the current listing did not include, newest first, at most
// plaudPendingRefetchCap. The fact's front matter supplies the metadata the
// listing would have.
func (c *plaudConnector) pendingRefetch(listed map[string]bool) []plaudFile {
	if c.vaultDir == "" {
		return nil
	}
	entries, err := os.ReadDir(c.vaultDir)
	if err != nil {
		return nil
	}
	prefix := slugPrefix(c.cfg.Type, c.cfg.Name)
	var out []plaudFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.vaultDir, name))
		if err != nil {
			continue
		}
		fm, body := splitFactFrontMatter(string(data))
		if fm["connector"] != c.cfg.Name || fm["source"] != c.cfg.Type || fm["status"] == StatusDeprecated {
			continue
		}
		id := fm["source_id"]
		if id == "" || listed[id] || !plaudFactNeedsRefetch(fm, body) {
			continue
		}
		f := plaudFile{ID: id, Name: fm["title"], Serial: fm["attr_device_serial"]}
		if t, err := time.Parse(time.RFC3339, fm["attr_recorded_at"]); err == nil {
			f.StartAt, f.CreatedAt = t, t
		} else if t, err := time.Parse(time.RFC3339, fm["synthesized"]); err == nil {
			f.CreatedAt = t
		}
		if ms, err := strconv.ParseInt(fm["attr_duration_ms"], 10, 64); err == nil {
			f.DurationMS = ms
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > plaudPendingRefetchCap {
		out = out[:plaudPendingRefetchCap]
	}
	return out
}

// plaudFactNeedsRefetch reports whether a stored fact carries no text from
// Plaud yet: pending, or holding only the hive's machine transcript (so
// Plaud's own text replaces it once it exists).
func plaudFactNeedsRefetch(fm map[string]string, body string) bool {
	switch fm["attr_"+plaudContentAttr] {
	case plaudContentPending, FactContentMachine:
		return true
	case plaudContentReady:
		return false
	}
	return !strings.Contains(body, "\n## Summary") && !strings.Contains(body, "\n## Transcript")
}

// splitFactFrontMatter returns the front matter and the body of a fact.
func splitFactFrontMatter(content string) (map[string]string, string) {
	fm := frontMatter(content)
	if !strings.HasPrefix(content, "---\n") {
		return fm, content
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return fm, content
	}
	rest := content[4+end+4:]
	if i := strings.Index(rest, "\n"); i >= 0 {
		rest = rest[i+1:]
	} else {
		rest = ""
	}
	return fm, rest
}

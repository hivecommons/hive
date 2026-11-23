package tokens

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Gemini CLI chat recordings (@google/gemini-cli-core, verified against
// 0.70.0 services/chatRecordingService.js and config/storage.js):
//
//   - Location: $HOME/.gemini/tmp/<project-id>/chats/session-<YYYY-MM-DDTHH-MM>-<id8>.jsonl
//     for a main session, and .../chats/<parent-session-id>/<session-id>.jsonl
//     for a subagent. <project-id> is a slug of the project directory name
//     (older releases used the sha256 of the project path); the directory
//     holds a .project_root file whose content is the project path.
//     Releases before the JSONL switch wrote one pretty-printed
//     ConversationRecord per session-*.json file.
//   - Format: append-only JSONL. The first line is the session metadata
//     ({"sessionId","projectHash","startTime","lastUpdated","kind",...});
//     every later line is either a full MessageRecord, a {"$set":{...}}
//     metadata update, a {"$patch":{...}} content patch or a
//     {"$rewindTo":"<id>"} rewind. A message is re-appended in full every
//     time it changes (tokens arriving after the text, tool calls being
//     added), so one message id appears on many lines.
//   - Usage: an assistant message ("type":"gemini") carries the CONCRETE
//     model that served it ("model":"gemini-2.5-pro", never an alias) and
//     "tokens":{"input","output","cached","thoughts","tool","total"} copied
//     from that API response's usageMetadata: input = promptTokenCount
//     (INCLUDES the cached tokens), output = candidatesTokenCount (EXCLUDES
//     thoughts), cached = cachedContentTokenCount, thoughts =
//     thoughtsTokenCount, tool = toolUsePromptTokenCount.
//
// Example lines (one per line on disk):
//
//	{"sessionId":"5d1c...","projectHash":"web","startTime":"2026-11-20T10:00:00.000Z","lastUpdated":"2026-11-20T10:00:00.000Z","kind":"main"}
//	{"id":"u1","timestamp":"2026-11-20T10:00:01.000Z","type":"user","content":[{"text":"fix the bug"}]}
//	{"id":"g1","timestamp":"2026-11-20T10:00:05.000Z","type":"gemini","content":"done","model":"gemini-2.5-pro","tokens":{"input":1200,"output":80,"cached":1000,"thoughts":40,"tool":0,"total":1320}}
//	{"$set":{"lastUpdated":"2026-11-20T10:00:05.000Z"}}

// geminiHomeRel is the Gemini CLI's state tree under a HOME.
const geminiHomeRel = ".gemini"

// geminiUnknownAgent is the attribution for a recording whose project root is
// not under an agent work dir and whose tree has no known owner (bob uses
// "bob" the same way).
const geminiUnknownAgent = "gemini"

// geminiUnknownModel labels usage recorded without a model. It is priced by
// the tier fallback (so it never shows $0) and listed as approximate.
const geminiUnknownModel = "gemini-unknown"

// maxGeminiLineBytes bounds one JSONL line. A message line embeds tool
// output, so it can be large; a longer line is skipped, not fatal.
const maxGeminiLineBytes = 16 * 1024 * 1024

// maxGeminiLegacyFileBytes bounds a legacy whole-file .json recording.
const maxGeminiLegacyFileBytes = 50 * 1024 * 1024

// GeminiSessionTrees returns the shared Gemini CLI home plus every per-agent
// <home>/agents/<name>/.gemini beside it (hivecommons/hive#17031).
func GeminiSessionTrees(sharedDir string) []SessionTree {
	return perAgentSessionTrees(sharedDir, geminiHomeRel)
}

// GeminiHomeBesideClaude derives the shared Gemini CLI home from the
// configured claude_sessions_dir: both CLIs keep their state under the same
// shared HOME, so "<home>/.claude/projects" maps to "<home>/.gemini". When the
// Claude dir is not in that layout it falls back to the default shared HOME.
func GeminiHomeBesideClaude(claudeSessionsDir string) string {
	clean := filepath.Clean(claudeSessionsDir)
	suffix := string(filepath.Separator) + filepath.FromSlash(claudeProjectsRel)
	if claudeSessionsDir != "" && strings.HasSuffix(clean, suffix) {
		return filepath.Join(strings.TrimSuffix(clean, suffix), geminiHomeRel)
	}
	return filepath.Join("/data/home", geminiHomeRel)
}

// geminiTokens is TokensSummary from chatRecordingTypes.d.ts.
type geminiTokens struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
	Thoughts int64 `json:"thoughts"`
	Tool     int64 `json:"tool"`
	Total    int64 `json:"total"`
}

// geminiRecord is any one JSONL line. Message records carry id/type; the
// metadata line carries sessionId/projectHash; control records carry one of
// the $-keys. Only the fields the scanner needs are decoded.
type geminiRecord struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Model     string          `json:"model"`
	Tokens    *geminiTokens   `json:"tokens"`
	SessionID string          `json:"sessionId"`
	StartTime string          `json:"startTime"`
	Updated   string          `json:"lastUpdated"`
	Messages  []geminiRecord  `json:"messages"`
	Set       *geminiSetBlock `json:"$set"`
	Patch     json.RawMessage `json:"$patch"`
	RewindTo  *string         `json:"$rewindTo"`
}

type geminiSetBlock struct {
	SessionID string         `json:"sessionId"`
	StartTime string         `json:"startTime"`
	Updated   string         `json:"lastUpdated"`
	Messages  []geminiRecord `json:"messages"`
}

// geminiMessage is the deduplicated state of one message id.
type geminiMessage struct {
	id        string
	order     int
	typ       string
	timestamp int64
	model     string
	tokens    *geminiTokens
}

// geminiRecording is one parsed recording file.
type geminiRecording struct {
	sessionID string
	startMs   int64
	updatedMs int64
	messages  map[string]*geminiMessage
}

// geminiUsage is one message's billable split in the collector's buckets.
//
// Gemini's promptTokenCount already includes the cached tokens, so the
// uncached input is input - cached (the Gemini CLI's own stats do the same,
// telemetry/uiTelemetry.js processApiResponse). toolUsePromptTokenCount is
// extra prompt-side input. Thinking tokens are billed at the output rate, so
// they are added to output. Gemini reports no cache-write count.
// The resulting Input+Output+CacheRead equals the response's totalTokenCount.
func (t *geminiTokens) usage() (input, output, cacheRead int64) {
	if t == nil {
		return 0, 0, 0
	}
	cached := max(t.Cached, 0)
	input = max(t.Input-cached, 0) + max(t.Tool, 0)
	output = max(t.Output, 0) + max(t.Thoughts, 0)
	return input, output, cached
}

func (r *geminiRecording) noteMessage(m geminiRecord) {
	if m.ID == "" {
		return
	}
	prev := r.messages[m.ID]
	if prev == nil {
		prev = &geminiMessage{id: m.ID, order: len(r.messages)}
		r.messages[m.ID] = prev
	}
	// A message is re-appended whole on every change; the latest copy wins,
	// but a later copy that lost a field never erases a recorded one.
	if m.Type != "" {
		prev.typ = m.Type
	}
	if ts := parseTimestampToUnixMilli(m.Timestamp); ts != 0 {
		prev.timestamp = ts
	}
	if m.Model != "" {
		prev.model = m.Model
	}
	if m.Tokens != nil {
		prev.tokens = m.Tokens
	}
}

func (r *geminiRecording) noteMeta(sessionID, start, updated string) {
	// The last sessionId wins, as in the CLI's own usage aggregator
	// (services/usageAggregator.js collectFileTurnUsages): a resumed session
	// re-stamps it with {"$set":{"sessionId":...}}.
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		r.sessionID = sessionID
	}
	if ts := parseTimestampToUnixMilli(start); ts != 0 && r.startMs == 0 {
		r.startMs = ts
	}
	if ts := parseTimestampToUnixMilli(updated); ts != 0 && ts > r.updatedMs {
		r.updatedMs = ts
	}
}

// apply folds one decoded line into the recording and reports whether it was
// a recording line at all. $rewindTo and $patch removals are deliberately
// ignored: a rewound or removed message was still a billed API call, and its
// tokens stay counted.
func (r *geminiRecording) apply(rec geminiRecord) bool {
	switch {
	case rec.RewindTo != nil || len(rec.Patch) > 0:
		return true
	case rec.Set != nil:
		r.noteMeta(rec.Set.SessionID, rec.Set.StartTime, rec.Set.Updated)
		for _, m := range rec.Set.Messages {
			r.noteMessage(m)
		}
		return true
	case rec.ID != "":
		r.noteMessage(rec)
		return true
	case rec.SessionID != "":
		r.noteMeta(rec.SessionID, rec.StartTime, rec.Updated)
		for _, m := range rec.Messages {
			r.noteMessage(m)
		}
		return true
	}
	return false
}

// maxGeminiProjectRootBytes bounds the .project_root marker read. The marker
// holds one path; anything longer is not a marker the CLI wrote.
const maxGeminiProjectRootBytes = 4096

// geminiAgentWorkDirRoot is where agent work dirs live. Hash-era project
// dirs are named sha256(<root>/<agent>), the same match resumehandle uses
// for gemini (crux/agent/resumehandle/resumehandle.go geminiProjectDir).
const geminiAgentWorkDirRoot = "/data/agents"

// openGeminiRegular opens path read-only without following a final symlink
// and without blocking on a planted FIFO, and returns it only when it is a
// regular file. The trees are agent-owned, so their contents are untrusted.
func openGeminiRegular(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("not a regular file (mode %s)", info.Mode())
	}
	return f, info, nil
}

// parseGeminiRecording reads one recording from f, never past size bytes
// (the size fstat reported when it was opened). JSONL is read line by line
// so a partially written last line (the CLI appends while we scan) is
// skipped and picked up on the next scan. A file with no usable JSONL line
// is retried as a legacy whole-file ConversationRecord.
func parseGeminiRecording(f *os.File, size int64) (*geminiRecording, error) {
	rec := &geminiRecording{messages: make(map[string]*geminiMessage)}
	parsed := 0
	reader := bufio.NewReaderSize(io.LimitReader(f, size), 64*1024)
	for {
		line, tooLong, err := readGeminiLine(reader)
		if len(line) > 0 && !tooLong {
			var raw geminiRecord
			if json.Unmarshal(line, &raw) == nil && rec.apply(raw) {
				parsed++
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	if parsed > 0 || size > maxGeminiLegacyFileBytes {
		return rec, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, size))
	if err != nil {
		return nil, err
	}
	var legacy geminiRecord
	if json.Unmarshal(data, &legacy) == nil {
		rec.apply(legacy)
	}
	return rec, nil
}

// readGeminiLine returns the next line without its newline. A line longer
// than maxGeminiLineBytes is consumed and reported as tooLong.
func readGeminiLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	var buf bytes.Buffer
	for {
		chunk, isPrefix, rerr := r.ReadLine()
		if !tooLong {
			if buf.Len()+len(chunk) > maxGeminiLineBytes {
				tooLong = true
				buf.Reset()
			} else {
				buf.Write(chunk)
			}
		}
		if rerr != nil {
			return bytes.TrimSpace(buf.Bytes()), tooLong, rerr
		}
		if !isPrefix {
			return bytes.TrimSpace(buf.Bytes()), tooLong, nil
		}
	}
}

// geminiFileKey identifies one version of a recording file. Recordings are
// append-only, so a changed size or mtime means new content.
type geminiFileKey struct {
	size    int64
	mtimeNs int64
}

type geminiCacheEntry struct {
	key geminiFileKey
	rec *geminiRecording
	gen uint64
}

// geminiScanCache keeps each recording's parsed result between scans so
// the 30-second loop reparses only files that changed. A Gemini recording
// re-appends whole messages on every tool update, so re-reading every file
// from the last 30 days each scan would cost far more than the new bytes.
// Entries not seen during a scan are pruned at its end, so the cache never
// outgrows the set of live files. Cached recordings are never mutated.
type geminiScanCache struct {
	mu      sync.Mutex
	gen     uint64
	entries map[string]*geminiCacheEntry

	// parses counts cache misses (files actually read); tests use it.
	parses int
}

func newGeminiScanCache() *geminiScanCache {
	return &geminiScanCache{entries: make(map[string]*geminiCacheEntry)}
}

// begin starts a scan generation; every file looked up during it survives
// the matching prune.
func (c *geminiScanCache) begin() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.gen++
	c.mu.Unlock()
}

// prune drops every entry not looked up since the last begin.
func (c *geminiScanCache) prune() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for path, e := range c.entries {
		if e.gen != c.gen {
			delete(c.entries, path)
		}
	}
}

func (c *geminiScanCache) get(path string, key geminiFileKey) *geminiRecording {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[path]
	if e == nil || e.key != key {
		return nil
	}
	e.gen = c.gen
	return e.rec
}

func (c *geminiScanCache) put(path string, key geminiFileKey, rec *geminiRecording) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.parses++
	c.entries[path] = &geminiCacheEntry{key: key, rec: rec, gen: c.gen}
}

// loadGeminiRecording returns the parsed recording at path, from the cache
// when its size and mtime are unchanged. It returns (nil, nil) for anything
// that is not a regular file (a symlink, FIFO or device is skipped).
func loadGeminiRecording(path string, lst os.FileInfo, cache *geminiScanCache) (*geminiRecording, error) {
	key := geminiFileKey{size: lst.Size(), mtimeNs: lst.ModTime().UnixNano()}
	if rec := cache.get(path, key); rec != nil {
		return rec, nil
	}
	f, info, err := openGeminiRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error
	if !os.SameFile(lst, info) {
		// Swapped between Lstat and open; the next scan sees the new file.
		return nil, nil
	}
	rec, err := parseGeminiRecording(f, info.Size())
	if err != nil {
		return nil, err
	}
	cache.put(path, geminiFileKey{size: info.Size(), mtimeNs: info.ModTime().UnixNano()}, rec)
	return rec, nil
}

// geminiUnpricedWarned remembers which models already produced the
// "no exact price" log line, so a 30-second scan loop logs each once per
// process instead of on every scan.
var geminiUnpricedWarned sync.Map

func warnGeminiUnpriced(logger *slog.Logger, model, path string) {
	if logger == nil {
		return
	}
	if _, seen := geminiUnpricedWarned.LoadOrStore(model, true); seen {
		return
	}
	if model == geminiUnknownModel {
		logger.Warn("gemini usage recorded without a model; pricing it with the tier fallback", "model", model, "path", path)
		return
	}
	logger.Warn("gemini model has no exact price; pricing it with the tier fallback", "model", model, "path", path)
}

// geminiProjectResolver maps a project dir under <gemini-home>/tmp to the
// agent whose work dir is that project, or "" when it cannot tell.
type geminiProjectResolver struct {
	homeDir    string
	owner      string
	byDir      map[string]string
	hashAgents map[string]string
}

func newGeminiProjectResolver(geminiHomeDir, owner string) *geminiProjectResolver {
	return &geminiProjectResolver{homeDir: geminiHomeDir, owner: owner, byDir: make(map[string]string)}
}

// agentFor returns the agent named by the project dir itself: the project
// root in its .project_root marker when that is under an agent work dir, or,
// for a hash-era dir with no marker, the agent whose work dir hashes to the
// dir name. "" means the project does not name an agent.
func (r *geminiProjectResolver) agentFor(projectDir string) string {
	if agent, ok := r.byDir[projectDir]; ok {
		return agent
	}
	agent := ""
	if root, ok := readGeminiProjectRoot(filepath.Join(projectDir, ".project_root")); ok {
		agent = detectAgentFromCwd(root)
	} else {
		agent = r.hashAgent(filepath.Base(projectDir))
	}
	r.byDir[projectDir] = agent
	return agent
}

// hashAgent matches a hash-era project dir name against sha256 of every
// known agent's work dir. The candidates are the per-agent HOMEs beside the
// shared home plus this tree's owner; the list is built once per scan and
// only when some project dir has no marker.
func (r *geminiProjectResolver) hashAgent(name string) string {
	if len(name) != sha256.Size*2 {
		return ""
	}
	if r.hashAgents == nil {
		r.hashAgents = make(map[string]string)
		names := []string{}
		if r.owner != "" {
			names = append(names, r.owner)
		}
		if entries, err := os.ReadDir(filepath.Join(filepath.Dir(r.homeDir), perAgentHomesDirName)); err == nil {
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					names = append(names, e.Name())
				}
			}
		}
		for _, n := range names {
			sum := sha256.Sum256([]byte(filepath.Join(geminiAgentWorkDirRoot, n)))
			r.hashAgents[hex.EncodeToString(sum[:])] = n
		}
	}
	return r.hashAgents[strings.ToLower(name)]
}

// readGeminiProjectRoot reads a .project_root marker: a regular file (not a
// symlink or FIFO) of at most maxGeminiProjectRootBytes.
func readGeminiProjectRoot(path string) (string, bool) {
	lst, err := os.Lstat(path)
	if err != nil || !lst.Mode().IsRegular() {
		return "", false
	}
	f, _, err := openGeminiRegular(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error
	data, err := io.ReadAll(io.LimitReader(f, maxGeminiProjectRootBytes+1))
	if err != nil || len(data) > maxGeminiProjectRootBytes {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// ScanGeminiSessionTree reads the Gemini CLI chat recordings under one
// Gemini home (typically <home>/.gemini). defaultAgent, when non-empty, is
// the agent whose per-agent HOME holds geminiHomeDir.
//
// Every scan rebuilds the totals from the recordings, like the Claude
// scanner, so a re-scan of a grown file never double counts: each message id
// is counted once, with its latest recorded tokens. Recordings that carry
// the same sessionId (the CLI copies tmp/<sha256>/ to tmp/<slug>/ when it
// migrates a project dir and leaves the original, config/storageMigration.js
// migrateDirectory) are merged, message by message, into one session.
//
// A session that used several models becomes one SessionSummary per model
// (SessionID "<sessionId>#<model>"), because cost is priced per session
// model (EstimateFromSummary); one summary per session would price every
// message at a single model's rate. SessionCount therefore counts
// (session, model) pairs, not sessions.
func ScanGeminiSessionTree(geminiHomeDir, defaultAgent string, logger *slog.Logger) (*AggregateSummary, error) {
	return scanGeminiSessionTree(geminiHomeDir, defaultAgent, logger, nil)
}

// geminiSessionGroup is every recording file of one session.
type geminiSessionGroup struct {
	key   string
	path  string
	agent string
	recs  []*geminiRecording
}

func scanGeminiSessionTree(geminiHomeDir, defaultAgent string, logger *slog.Logger, cache *geminiScanCache) (*AggregateSummary, error) {
	agg := &AggregateSummary{
		ByAgent:       make(map[string]int64),
		ByModel:       make(map[string]int64),
		ByAgentDetail: make(map[string]*AgentModelBucket),
		ByModelDetail: make(map[string]*AgentModelBucket),
	}
	if geminiHomeDir == "" {
		return agg, nil
	}

	var files []string
	for _, pattern := range []string{
		filepath.Join(geminiHomeDir, "tmp", "*", "chats", "*.jsonl"),
		filepath.Join(geminiHomeDir, "tmp", "*", "chats", "*", "*.jsonl"),
		filepath.Join(geminiHomeDir, "tmp", "*", "chats", "session-*.json"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return agg, err
		}
		files = append(files, matches...)
	}
	sort.Strings(files)

	cutoff := time.Now().Add(-maxSessionAgeDays * 24 * time.Hour)
	resolver := newGeminiProjectResolver(geminiHomeDir, defaultAgent)
	groups := make(map[string]*geminiSessionGroup)
	var order []string

	for _, path := range files {
		// A legacy .json is migrated by the CLI into "<name>.jsonl" on
		// resume and the old file is left behind; count only the new one.
		if strings.HasSuffix(path, ".json") {
			if _, err := os.Lstat(path + "l"); err == nil {
				continue
			}
		}
		// Lstat, not Stat: a symlink, FIFO or device in an agent-owned tree
		// is skipped rather than followed or read.
		lst, err := os.Lstat(path)
		if err != nil || !lst.Mode().IsRegular() || lst.ModTime().Before(cutoff) {
			continue
		}
		rec, err := loadGeminiRecording(path, lst, cache)
		if err != nil {
			if logger != nil {
				logger.Warn("gemini session parse failed", "path", path, "error", err)
			}
			continue
		}
		if rec == nil {
			continue
		}

		key := rec.sessionID
		if key == "" {
			key = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
		g := groups[key]
		if g == nil {
			g = &geminiSessionGroup{key: key, path: path}
			groups[key] = g
			order = append(order, key)
		}
		g.recs = append(g.recs, rec)
		if g.agent == "" {
			g.agent = resolver.agentFor(geminiProjectDir(geminiHomeDir, path))
		}
	}

	for _, key := range order {
		g := groups[key]
		agent := g.agent
		if agent == "" {
			agent = defaultAgent
		}
		if agent == "" {
			agent = geminiUnknownAgent
		}
		for _, s := range geminiSessionSummaries(mergeGeminiRecordings(g.recs), g.key, agent) {
			if _, exact := LookupPrice(s.Model); !exact {
				warnGeminiUnpriced(logger, s.Model, g.path)
			}
			addGeminiSession(agg, s)
		}
	}
	agg.SessionCount = len(agg.Sessions)
	return agg, nil
}

// mergeGeminiRecordings unions the messages of every file of one session by
// message id, without mutating the (possibly cached) inputs.
func mergeGeminiRecordings(recs []*geminiRecording) *geminiRecording {
	if len(recs) == 1 {
		return recs[0]
	}
	out := &geminiRecording{messages: make(map[string]*geminiMessage)}
	for _, rec := range recs {
		if rec.sessionID != "" {
			out.sessionID = rec.sessionID
		}
		if rec.startMs != 0 && (out.startMs == 0 || rec.startMs < out.startMs) {
			out.startMs = rec.startMs
		}
		if rec.updatedMs > out.updatedMs {
			out.updatedMs = rec.updatedMs
		}
		for _, m := range orderedGeminiMessages(rec) {
			id := m.id
			prev := out.messages[id]
			if prev == nil {
				cp := *m
				cp.order = len(out.messages)
				out.messages[id] = &cp
				continue
			}
			if m.typ != "" {
				prev.typ = m.typ
			}
			if m.timestamp != 0 {
				prev.timestamp = m.timestamp
			}
			if m.model != "" {
				prev.model = m.model
			}
			if m.tokens != nil {
				prev.tokens = m.tokens
			}
		}
	}
	return out
}

// orderedGeminiMessages returns a recording's messages in first-seen order.
func orderedGeminiMessages(rec *geminiRecording) []*geminiMessage {
	ordered := make([]*geminiMessage, 0, len(rec.messages))
	for _, m := range rec.messages {
		ordered = append(ordered, m)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].order < ordered[j].order })
	return ordered
}

// geminiProjectDir returns <home>/tmp/<project-id> for a recording path at
// either chats/ depth.
func geminiProjectDir(geminiHomeDir, path string) string {
	tmp := filepath.Join(geminiHomeDir, "tmp")
	rel, err := filepath.Rel(tmp, path)
	if err != nil {
		return filepath.Dir(filepath.Dir(path))
	}
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return filepath.Join(tmp, first)
}

// geminiEffectiveModel is m's recorded model or, for a message that carries
// tokens but no model (the CLI records some synthetic turns that way), the
// model of the nearest assistant message in the same session that has one,
// preferring the earlier at equal distance. "" when no message has a model.
func geminiEffectiveModel(ordered []*geminiMessage, i int) string {
	if m := strings.TrimSpace(ordered[i].model); m != "" {
		return m
	}
	for d := 1; d < len(ordered); d++ {
		for _, j := range []int{i - d, i + d} {
			if j < 0 || j >= len(ordered) {
				continue
			}
			if n := ordered[j]; n.typ == "gemini" {
				if m := strings.TrimSpace(n.model); m != "" {
					return m
				}
			}
		}
	}
	return ""
}

// geminiSessionSummaries splits one session into a summary per model.
func geminiSessionSummaries(rec *geminiRecording, key, agent string) []SessionSummary {
	ordered := orderedGeminiMessages(rec)

	type perModel struct {
		sum      SessionSummary
		timeline usageTimeline
	}
	byModel := make(map[string]*perModel)
	var models []string
	userMessages := 0
	for i, m := range ordered {
		if m.typ == "user" {
			userMessages++
		}
		if m.typ != "gemini" || m.tokens == nil {
			continue
		}
		in, out, cached := m.tokens.usage()
		if in+out+cached == 0 {
			continue
		}
		model := geminiEffectiveModel(ordered, i)
		if model == "" {
			model = geminiUnknownModel
		}
		pm := byModel[model]
		if pm == nil {
			pm = &perModel{sum: SessionSummary{
				SessionID: key + "#" + model,
				Agent:     agent,
				Model:     model,
				Backend:   BackendGemini,
			}}
			byModel[model] = pm
			models = append(models, model)
		}
		s := &pm.sum
		s.InputTokens += in
		s.OutputTokens += out
		s.CacheRead += cached
		s.Messages++
		if m.timestamp != 0 {
			if s.FirstActive == 0 || m.timestamp < s.FirstActive {
				s.FirstActive = m.timestamp
			}
			if m.timestamp > s.LastActive {
				s.LastActive = m.timestamp
			}
		}
		pm.timeline.add(UsageEvent{
			TimestampMs: m.timestamp,
			Model:       model,
			Input:       in,
			Output:      out,
			CacheRead:   cached,
		})
	}

	out := make([]SessionSummary, 0, len(models))
	for i, model := range models {
		pm := byModel[model]
		s := pm.sum
		s.TotalTokens = s.InputTokens + s.OutputTokens + s.CacheRead + s.CacheCreate
		if i == 0 {
			// The user turns belong to the session, not to a model; count
			// them once, on the first model's summary.
			s.Messages += userMessages
		}
		if s.FirstActive == 0 {
			s.FirstActive = rec.startMs
		}
		if s.LastActive == 0 {
			s.LastActive = rec.updatedMs
		}
		s.Usage, s.UsageCoalesced = pm.timeline.finish()
		out = append(out, s)
	}
	return out
}

func addGeminiSession(agg *AggregateSummary, s SessionSummary) {
	agg.Sessions = append(agg.Sessions, s)
	agg.TotalTokens += s.TotalTokens
	agg.TotalInput += s.InputTokens
	agg.TotalOutput += s.OutputTokens
	agg.TotalCacheRead += s.CacheRead
	agg.TotalCacheCreate += s.CacheCreate
	agg.TotalMessages += s.Messages
	agg.ByAgent[s.Agent] += s.TotalTokens
	agg.ByModel[s.Model] += s.TotalTokens
	for _, b := range []struct {
		m   map[string]*AgentModelBucket
		key string
	}{{agg.ByAgentDetail, s.Agent}, {agg.ByModelDetail, s.Model}} {
		bucket := b.m[b.key]
		if bucket == nil {
			bucket = &AgentModelBucket{}
			b.m[b.key] = bucket
		}
		bucket.Input += s.InputTokens
		bucket.Output += s.OutputTokens
		bucket.CacheRead += s.CacheRead
		bucket.CacheCreate += s.CacheCreate
		bucket.Messages += s.Messages
		bucket.Sessions++
	}
}

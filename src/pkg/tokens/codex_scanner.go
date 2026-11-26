package tokens

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BackendCodex names sessions read from the Codex CLI's own rollout files
// (hivecommons/hive#20003). A codex agent talks to its provider directly, so
// it never passes through the inference translator and the rollout is the
// only record of what it spent.
const BackendCodex = "codex"

// codexAgentHomePrefix is the per-agent CODEX_HOME directory name prefix under
// the homes root (crux/agent codexHomePrefix: /data/home/.codex-<agent>). The
// agent is the suffix. The shared /data/home/.codex has no owner and is not
// scanned.
const codexAgentHomePrefix = ".codex-"

// codexSessionIDPrefix keeps codex session ids distinct from every other
// scanner's ids before MergeAggregates deduplicates by id.
const codexSessionIDPrefix = "codex-"

// codexMeteringSinceFile is the marker, beside the token snapshot, holding
// the unix-millisecond time codex metering was first enabled on this hive.
const codexMeteringSinceFile = "token-codex-metering-since"

// maxCodexSessionAge matches the Claude and Bob scanners' mtime window.
const maxCodexSessionAge = 30 * 24 * time.Hour

// maxCodexRolloutLineBytes bounds one rollout line the scanner will decode.
// token_count and turn_context lines are small; a line past this is a large
// tool output or response item, which the scanner never needs.
const maxCodexRolloutLineBytes = 1 << 20

// Rollout markers checked before decoding a line, so the large response and
// tool-output lines that make up most of a rollout are never unmarshalled.
var (
	codexTokenCountMarker  = []byte(`"token_count"`)
	codexTurnContextMarker = []byte(`"turn_context"`)
)

// codexRolloutLine is the subset of a Codex rollout JSONL line the scanner
// reads. Usage lines look like
//
//	{"timestamp":"...","type":"event_msg","payload":{"type":"token_count",
//	 "info":{"total_token_usage":{...},"last_token_usage":{...}}}}
//
// and the model in effect comes from
//
//	{"timestamp":"...","type":"turn_context","payload":{"model":"gpt-..."}}
type codexRolloutLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Info  *struct {
			Total *codexTokenUsage `json:"total_token_usage"`
			Last  *codexTokenUsage `json:"last_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

// codexTokenUsage is one Codex usage block. Codex counts cached (and cache
// write) tokens INSIDE input_tokens, and reasoning tokens inside
// output_tokens.
type codexTokenUsage struct {
	Input      int64 `json:"input_tokens"`
	CachedRead int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
	Total      int64 `json:"total_tokens"`
}

// usageEvent maps a Codex usage block onto the collector's four buckets so
// that no token is counted twice: input is the uncached remainder.
func (u codexTokenUsage) usageEvent(tsMs int64, model string) UsageEvent {
	input := u.Input - u.CachedRead - u.CacheWrite
	if input < 0 {
		input = 0
	}
	return UsageEvent{
		TimestampMs: tsMs,
		Model:       model,
		Coalesced:   1,
		Input:       input,
		Output:      u.Output,
		CacheRead:   u.CachedRead,
		CacheCreate: u.CacheWrite,
	}
}

// minus is the per-turn usage implied by two cumulative totals, used only when
// a token_count event carries a total but no last_token_usage.
func (u codexTokenUsage) minus(prev codexTokenUsage) codexTokenUsage {
	return codexTokenUsage{
		Input:      u.Input - prev.Input,
		CachedRead: u.CachedRead - prev.CachedRead,
		CacheWrite: u.CacheWrite - prev.CacheWrite,
		Output:     u.Output - prev.Output,
		Reasoning:  u.Reasoning - prev.Reasoning,
		Total:      u.Total - prev.Total,
	}
}

// CodexRolloutFiles returns every per-agent rollout file under homesRoot
// (normally /data/home), keyed to its agent. Order is deterministic.
func CodexRolloutFiles(homesRoot string) ([]SessionTree, error) {
	if homesRoot == "" {
		return nil, nil
	}
	var out []SessionTree
	for _, pattern := range []string{
		filepath.Join(homesRoot, codexAgentHomePrefix+"*", "sessions", "*", "*", "*", "rollout-*.jsonl"),
		filepath.Join(homesRoot, codexAgentHomePrefix+"*", "archived_sessions", "rollout-*.jsonl"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if agent := codexAgentFromRolloutPath(homesRoot, m); agent != "" {
				out = append(out, SessionTree{Dir: m, Agent: agent})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// codexAgentFromRolloutPath returns the agent whose CODEX_HOME holds path,
// or "" when the home directory name has no agent suffix.
func codexAgentFromRolloutPath(homesRoot, path string) string {
	rel, err := filepath.Rel(homesRoot, path)
	if err != nil {
		return ""
	}
	home := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	agent := strings.TrimPrefix(home, codexAgentHomePrefix)
	if agent == home || agent == "" {
		return ""
	}
	return agent
}

// ScanCodexRollouts reads every per-agent Codex rollout under homesRoot
// modified in the last maxCodexSessionAge and returns one session per rollout
// with token usage. sinceMs, when positive, drops usage events stamped before
// it (and events with no timestamp), so enabling the scanner on a hive with
// retained codex history does not book that history as spend in the current
// budget window. Unreadable files are skipped; the first error is returned
// alongside whatever could be read, so one bad file never zeroes the rest.
func ScanCodexRollouts(homesRoot string, sinceMs int64) (*AggregateSummary, error) {
	agg := &AggregateSummary{
		ByAgent:       make(map[string]int64),
		ByModel:       make(map[string]int64),
		ByAgentDetail: make(map[string]*AgentModelBucket),
		ByModelDetail: make(map[string]*AgentModelBucket),
	}
	files, err := CodexRolloutFiles(homesRoot)
	if err != nil {
		return agg, err
	}
	cutoff := time.Now().Add(-maxCodexSessionAge)
	// filepath.Glob drops directories it cannot read, so an agent's sessions
	// tree that the hive UID cannot list would read as zero usage with no
	// error (the #19307 failure for Claude). Probe each one so that case is
	// reported as last_codex_scan_error instead.
	firstErr := codexUnreadableSessionsErr(homesRoot)
	var sessions []SessionSummary
	for _, f := range files {
		info, err := os.Stat(f.Dir)
		if err != nil || info.IsDir() || info.ModTime().Before(cutoff) {
			continue
		}
		sess, err := parseCodexRolloutFile(f.Dir, f.Agent, sinceMs)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if sess != nil {
			sessions = append(sessions, *sess)
		}
	}
	MergeAggregates(agg, &AggregateSummary{Sessions: sessions})
	return agg, firstErr
}

// codexUnreadableSessionsErr returns an error naming the first per-agent
// sessions directory under homesRoot that exists but cannot be read.
func codexUnreadableSessionsErr(homesRoot string) error {
	homes, err := filepath.Glob(filepath.Join(homesRoot, codexAgentHomePrefix+"*"))
	if err != nil {
		return err
	}
	sort.Strings(homes)
	for _, home := range homes {
		dir := filepath.Join(home, "sessions")
		f, err := os.Open(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("reading codex sessions: %w", err)
		}
		_, err = f.Readdirnames(1)
		_ = f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("reading codex sessions %s: %w", dir, err)
		}
	}
	return nil
}

func parseCodexRolloutFile(path, agent string, sinceMs int64) (*SessionSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening codex rollout: %w", err)
	}
	defer func() { _ = f.Close() }()
	id := codexSessionIDPrefix + agent + "-" + strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return parseCodexRollout(f, id, agent, sinceMs)
}

// parseCodexRollout builds one session from a rollout stream. It returns nil
// when the rollout carries no countable usage.
func parseCodexRollout(r io.Reader, sessionID, agent string, sinceMs int64) (*SessionSummary, error) {
	br := bufio.NewReader(r)
	var (
		model     string
		prevTotal *codexTokenUsage
		timeline  usageTimeline
		perModel  = map[string]int64{}
		summary   = SessionSummary{SessionID: sessionID, Agent: agent, Backend: BackendCodex}
	)
	addTurn := func(rec *codexRolloutLine) {
		info := rec.Payload.Info
		turn, ok := codexTurnUsage(info.Total, info.Last, prevTotal)
		if info.Total != nil {
			t := *info.Total
			prevTotal = &t
		}
		if !ok {
			return
		}
		ts := parseTimestampToUnixMilli(rec.Timestamp)
		if sinceMs > 0 && ts < sinceMs {
			return
		}
		m := model
		if m == "" {
			m = "unknown"
		}
		ev := turn.usageEvent(ts, m)
		if ev.Total() <= 0 {
			return
		}
		timeline.add(ev)
		perModel[m] += ev.Total()
		summary.InputTokens += ev.Input
		summary.OutputTokens += ev.Output
		summary.CacheRead += ev.CacheRead
		summary.CacheCreate += ev.CacheCreate
		summary.Messages++
		if ts > 0 {
			if summary.FirstActive == 0 || ts < summary.FirstActive {
				summary.FirstActive = ts
			}
			if ts > summary.LastActive {
				summary.LastActive = ts
			}
		}
	}
	for {
		line, err := readCodexLine(br)
		if len(line) > 0 && (bytes.Contains(line, codexTokenCountMarker) || bytes.Contains(line, codexTurnContextMarker)) {
			var rec codexRolloutLine
			if json.Unmarshal(line, &rec) == nil {
				switch {
				case rec.Type == "turn_context":
					if rec.Payload.Model != "" {
						model = rec.Payload.Model
					}
				case rec.Payload.Type == "token_count" && rec.Payload.Info != nil:
					addTurn(&rec)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading codex rollout: %w", err)
		}
	}
	summary.TotalTokens = summary.InputTokens + summary.OutputTokens + summary.CacheRead + summary.CacheCreate
	if summary.TotalTokens <= 0 {
		return nil, nil
	}
	summary.Model = dominantCodexModel(perModel)
	summary.Usage, summary.UsageCoalesced = timeline.finish()
	return &summary, nil
}

// codexTurnUsage returns the usage one token_count event adds. Codex can emit
// the same token_count more than once for a turn (for example alongside a
// rate-limit update); an event whose cumulative total did not move adds
// nothing, so a repeat is never counted twice.
func codexTurnUsage(total, last, prevTotal *codexTokenUsage) (codexTokenUsage, bool) {
	if total != nil && prevTotal != nil && *total == *prevTotal {
		return codexTokenUsage{}, false
	}
	if last != nil {
		return *last, true
	}
	if total == nil {
		return codexTokenUsage{}, false
	}
	if prevTotal == nil {
		return *total, true
	}
	d := total.minus(*prevTotal)
	if d.Input < 0 || d.CachedRead < 0 || d.CacheWrite < 0 || d.Output < 0 {
		// The cumulative counter went backwards (a reset): this total is
		// all new usage.
		return *total, true
	}
	return d, true
}

// dominantCodexModel labels a session with the model that spent the most
// tokens in it (ties break by name, so the label is stable). The per-event
// models stay on the Usage timeline.
func dominantCodexModel(perModel map[string]int64) string {
	best, bestTokens := "", int64(-1)
	for m, n := range perModel {
		if n > bestTokens || (n == bestTokens && m < best) {
			best, bestTokens = m, n
		}
	}
	return best
}

// readCodexLine returns the next line without its newline. A line longer than
// maxCodexRolloutLineBytes is consumed and returned empty, since only small
// lines can be usage or model lines.
func readCodexLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > maxCodexRolloutLineBytes {
				tooLong = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return nil, err
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

// SetCodexHomesRoot enables the Codex rollout scanner over the per-agent
// CODEX_HOME directories under root (normally /data/home). Configure before
// Start, like the other session-directory setters.
func (c *Collector) SetCodexHomesRoot(root string) { c.codexHomesRoot = root }

// mergeCodexUsage adds per-agent Codex rollout usage to agg. Only usage at or
// after the hive's codex metering start (see codexMeteringSince) is counted.
func (c *Collector) mergeCodexUsage(agg *AggregateSummary, diag *Diagnostics) {
	if c.codexHomesRoot == "" {
		return
	}
	since := c.codexMeteringSince()
	codexAgg, err := ScanCodexRollouts(c.codexHomesRoot, since)
	if err != nil {
		diag.LastCodexScanError = err.Error()
		if err.Error() != c.codexScanErr {
			c.logger.Warn("codex rollout scan failed", "root", c.codexHomesRoot, "error", err)
		}
		c.codexScanErr = err.Error()
	} else if c.codexScanErr != "" {
		c.logger.Info("codex rollout scan recovered", "root", c.codexHomesRoot)
		c.codexScanErr = ""
	}
	if codexAgg != nil && codexAgg.SessionCount > 0 {
		MergeAggregates(agg, codexAgg)
	}
}

// codexMeteringSince returns the unix-millisecond time from which codex usage
// is counted. The first time the scanner runs on a hive it records "now" in a
// marker beside the token snapshot, so the codex history already on disk is
// not added to lifetime totals in one step. The governor opens a budget window
// with the lifetime total as its baseline and counts any later rise as spend,
// so a one-time backfill of weeks of codex use would read as spend in the
// current window and could trip the exhausted threshold at once. Only the
// scan goroutine calls this.
func (c *Collector) codexMeteringSince() int64 {
	if c.codexSinceMs > 0 {
		return c.codexSinceMs
	}
	marker := c.codexMeteringSincePath()
	if marker != "" {
		if data, err := os.ReadFile(marker); err == nil {
			if v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && v > 0 {
				c.codexSinceMs = v
				return v
			}
		}
	}
	now := time.Now().UnixMilli()
	c.codexSinceMs = now
	if marker != "" {
		tmp := marker + ".tmp"
		err := os.WriteFile(tmp, []byte(strconv.FormatInt(now, 10)+"\n"), 0o644)
		if err == nil {
			err = os.Rename(tmp, marker)
		}
		if err != nil {
			c.logger.Warn("could not record codex metering start; codex usage before the next restart may be dropped", "path", marker, "error", err)
		} else {
			c.logger.Info("codex token metering enabled; counting codex usage from now on", "since_ms", now)
		}
	}
	return now
}

func (c *Collector) codexMeteringSincePath() string {
	if c.persistPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.persistPath), codexMeteringSinceFile)
}

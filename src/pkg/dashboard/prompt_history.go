package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Agent prompt (kick) history.
//
// Before this store existed, the fully-expanded prompt text sent to an agent
// was retained in exactly two places, neither of which could answer "what was
// my agent asked to do, and when?":
//
//   - agent.AgentProcess.LastKickMessage — the MOST RECENT kick only, in
//     memory, replaced on every subsequent kick and lost on restart. This is
//     what the dashboard's "Last Prompt" tab renders.
//   - agent.KickRecord.Snippet — persisted across restarts via the snapshot,
//     but truncated to 120 characters (200 for startup bootstraps), which is
//     far too short to diagnose why an agent behaved the way it did.
//
// The structured audit log (audit.go) records that a kick HAPPENED
// (action=agent_start / kick, detail=trigger=governor-eval) but never the
// prompt text. This file adds the missing durable, full-text store.
//
// Storage shape deliberately mirrors audit.go: newline-delimited JSON on the
// spoke's /data PVC, rotated and gzipped by lumberjack, with an in-memory ring
// serving reads so the hot path never touches the disk.

const (
	// promptHistoryPath is the JSONL file on the spoke's PVC. Rotated backups
	// land alongside it as promptHistory-<timestamp>.jsonl.gz, matching the
	// convention audit.jsonl already uses.
	promptHistoryPath = "/data/prompt-history.jsonl"

	// promptHistoryMaxSizeMB caps a single JSONL file before rotation.
	//
	// Sizing is measured, not guessed. A fully-expanded kick prompt is:
	//   template body      ~3.0 KiB (mean of the 30 shipped policy templates
	//                                in pkg/policies/defaults; largest is
	//                                scanner-automerge.md at 10.5 KiB)
	//   issue list         up to 12.7 KiB (maxIssuesPerKick=100 issues x ~127 B
	//                                per formatIssueList line: age, repo,
	//                                number, labels, 60-rune title)
	//   PR list            ~3.5 KiB at 30 open PRs x ~120 B per line
	//   knowledge section  ~3.0 KiB (knowledge_max_facts default 25)
	//   repos section      ~0.4 KiB
	// giving ~22 KiB for a worst-case prompt and ~8 KiB for a typical one.
	//
	// At the most aggressive shipped cadence pack (level-6 "busy": supervisor
	// 2m, scanner/ci-maintainer 10m, quality 15m, architect/sec-check 30m,
	// outreach/strategist 1h) a hive issues ~8,700 kicks/week. At the typical
	// 8 KiB that is ~70 MiB/week raw; at worst case ~190 MiB/week raw.
	//
	// Retaining that uncompressed would dwarf the audit log's own 5 MiB x 3
	// budget, so the cap below plus promptHistoryMaxBackups bounds the store at
	// promptHistoryMaxSizeMB * (promptHistoryMaxBackups + 1) = 96 MiB on disk
	// worst case, and rotated backups are gzipped (prompt text is highly
	// repetitive template boilerplate and compresses roughly 4x, so the
	// realistic steady-state footprint is ~25-30 MiB). That comfortably holds
	// more than the one-week window the UI offers at typical cadence, and
	// degrades gracefully — oldest prompts age out first — at surge cadence.
	promptHistoryMaxSizeMB = 24

	// promptHistoryMaxBackups is how many rotated .gz files are kept.
	promptHistoryMaxBackups = 3

	// promptHistoryMaxAgeDays bounds retention by age as well as by size. The
	// UI's widest window is one week; double that gives headroom for a user
	// looking back at "last week" a few days late without unbounded growth.
	promptHistoryMaxAgeDays = 14

	// promptHistoryRingCap is how many entries are held in memory for reads.
	// At the level-6 "busy" cadence (~52 kicks/hour across 8 agents) this is
	// roughly 19 hours of history in RAM; older entries remain on disk in the
	// rotated files but are not served. Bounded so a long-running spoke cannot
	// grow the resident set without limit: 1000 x the 8 KiB typical prompt is
	// ~8 MiB of heap, which is acceptable next to the agent output buffers.
	promptHistoryRingCap = 1000

	// promptHistoryMaxPromptBytes truncates an individual oversized prompt
	// rather than dropping it, so a pathological kick still shows up in the
	// history with an explicit marker instead of vanishing. Set above the
	// ~22 KiB measured worst case so normal prompts are never touched.
	promptHistoryMaxPromptBytes = 32768

	// promptHistoryTruncationMarker is appended to a prompt clipped at
	// promptHistoryMaxPromptBytes so the UI never implies it has the full text.
	promptHistoryTruncationMarker = "\n\n[... prompt truncated by hive: exceeded 32768 bytes ...]"

	// promptHistoryDefaultWindowHours is the window used when a request does
	// not specify one: the "last day" the feature request asked for.
	promptHistoryDefaultWindowHours = 24

	// promptHistoryMaxWindowHours caps the requested window at one week, the
	// widest window the UI offers and the widest the ring can meaningfully
	// serve.
	promptHistoryMaxWindowHours = 168

	// promptHistoryMaxResults bounds a single API response so a wide window on
	// a busy hive cannot return a multi-megabyte payload to the browser.
	promptHistoryMaxResults = 200
)

// PromptEntry is one fully-expanded prompt delivered to an agent.
type PromptEntry struct {
	Timestamp string `json:"ts"`
	Agent     string `json:"agent"`
	Trigger   string `json:"trigger"`
	Prompt    string `json:"prompt"`
	// Truncated reports that Prompt was clipped at
	// promptHistoryMaxPromptBytes and carries the truncation marker.
	Truncated bool `json:"truncated,omitempty"`
}

// PromptHistory is a durable, bounded store of agent kick prompts.
type PromptHistory struct {
	mu     sync.Mutex
	writer *lumberjack.Logger
	ring   []PromptEntry
}

func newPromptHistory() *PromptHistory {
	p := &PromptHistory{ring: make([]PromptEntry, 0, promptHistoryRingCap)}

	// Mirror audit.go: only wire durable storage when the PVC is mounted, so
	// unit tests and non-container runs stay purely in-memory.
	if _, err := os.Stat("/data"); err == nil {
		p.writer = &lumberjack.Logger{
			Filename:   promptHistoryPath,
			MaxSize:    promptHistoryMaxSizeMB,
			MaxBackups: promptHistoryMaxBackups,
			MaxAge:     promptHistoryMaxAgeDays,
			Compress:   true,
		}
		p.loadFromDisk(promptHistoryPath)
	}

	return p
}

// loadFromDisk repopulates the ring from the active JSONL file on startup.
//
// Malformed lines are skipped individually rather than aborting the load. This
// matters in practice: the process can be killed mid-write, leaving a
// truncated final line, and a single bad line must not blank the whole view.
func (p *PromptHistory) loadFromDisk(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry PromptEntry
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		if entry.Timestamp == "" || entry.Agent == "" {
			continue
		}
		p.ring = append(p.ring, entry)
	}
	if len(p.ring) > promptHistoryRingCap {
		p.ring = p.ring[len(p.ring)-promptHistoryRingCap:]
	}
}

// secretPattern matches credential material that can legitimately reach prompt
// text and must never be persisted.
//
// Why this is needed: per-kick template substitution resolves ${VAR} through
// resolve.Registry (pkg/resolve/registry.go). Bare-environment fallback is
// restricted to config scope, so an undeclared ${GITHUB_TOKEN} in a kick
// template stays literal. BUT an operator may declare a variable in the config
// `variables:` block with `scope: template` and `type: env`, which resolves
// from hive's process environment — the same environment that holds the
// GitHub token and model-gateway API keys — straight into the prompt body.
// The http resolver likewise expands ${ENV} into request headers and can
// return secret-bearing bodies. So secrets in prompt text are reachable, and
// this store persists prompts to disk where the previous 120-char snippet did
// not. Redaction is applied at write time: nothing unredacted is ever durable.
var secretPattern = regexp.MustCompile(
	// GitHub tokens (classic, OAuth, server, refresh, user, fine-grained PAT)
	`(gh[pousr]_[A-Za-z0-9_]{10,}|github_pat_[A-Za-z0-9_]{10,})` +
		// Common model-gateway / cloud provider key shapes
		`|(sk-[A-Za-z0-9_\-]{16,}|xox[baprs]-[A-Za-z0-9\-]{10,}|AKIA[0-9A-Z]{12,}|AIza[0-9A-Za-z_\-]{30,})`,
)

// secretAssignmentPattern catches `TOKEN=<value>` / `api_key: <value>` style
// assignments whose value does not match a known vendor prefix. Capture groups:
// 1=name, 2=separator (preserved so `:` does not become `=`), 3=optional quote,
// 4=value.
var secretAssignmentPattern = regexp.MustCompile(
	`(?i)\b([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|APIKEY|API_KEY|ACCESS_KEY|PRIVATE_KEY)[A-Z0-9_]*)(\s*[:=]\s*)("?)([^\s"']{8,})`,
)

// unresolvedPlaceholder matches a literal ${VAR}. An unsubstituted placeholder
// is the NAME of a variable, never its value, so redacting it would corrupt
// legitimate prompt text (and hide from the owner that a variable failed to
// resolve — exactly the kind of misconfiguration this view exists to surface).
var unresolvedPlaceholder = regexp.MustCompile(`^\$\{[^}]*\}$`)

const secretRedactionPlaceholder = "***REDACTED***"

// redactPromptSecrets removes credential material from prompt text before it is
// persisted or served.
func redactPromptSecrets(s string) string {
	if s == "" {
		return s
	}
	s = secretPattern.ReplaceAllString(s, secretRedactionPlaceholder)
	s = secretAssignmentPattern.ReplaceAllStringFunc(s, func(m string) string {
		groups := secretAssignmentPattern.FindStringSubmatch(m)
		if groups == nil {
			return m
		}
		// Leave an unresolved ${VAR} alone — it is a placeholder, not a value.
		if unresolvedPlaceholder.MatchString(groups[4]) {
			return m
		}
		return groups[1] + groups[2] + groups[3] + secretRedactionPlaceholder
	})
	return s
}

// truncatePrompt clips an oversized prompt, appending an explicit marker.
// Truncation is byte-based but backs off to a UTF-8 boundary so the stored
// text is always valid UTF-8 and survives JSON round-tripping.
func truncatePrompt(s string) (string, bool) {
	if len(s) <= promptHistoryMaxPromptBytes {
		return s, false
	}
	cut := promptHistoryMaxPromptBytes
	for cut > 0 && !utf8Boundary(s[cut]) {
		cut--
	}
	return s[:cut] + promptHistoryTruncationMarker, true
}

// utf8Boundary reports whether b can start a UTF-8 sequence (i.e. it is not a
// 10xxxxxx continuation byte).
func utf8Boundary(b byte) bool { return b&0xC0 != 0x80 }

// Record persists one delivered prompt. Redaction and truncation happen here,
// before anything reaches memory or disk.
func (p *PromptHistory) Record(agent, trigger, prompt string) {
	if agent == "" || prompt == "" {
		return
	}
	if trigger == "" {
		trigger = "unknown"
	}

	text, truncated := truncatePrompt(redactPromptSecrets(prompt))

	entry := PromptEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Agent:     agent,
		Trigger:   trigger,
		Prompt:    text,
		Truncated: truncated,
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.ring) >= promptHistoryRingCap {
		p.ring = p.ring[1:]
	}
	p.ring = append(p.ring, entry)

	if p.writer != nil {
		if data, err := json.Marshal(entry); err == nil {
			// lumberjack.Write is a single atomic append per call, so an entry
			// never interleaves with a concurrent writer's line.
			if _, err := p.writer.Write(append(data, '\n')); err != nil {
				slog.Error("prompt history write failed", "error", err)
			}
		}
	}
}

// Query returns entries for an agent within a time window, newest first.
//
// agent == "" matches every agent. windowHours <= 0 falls back to the default
// window; anything above promptHistoryMaxWindowHours is clamped. The result is
// capped at promptHistoryMaxResults.
func (p *PromptHistory) Query(agent string, windowHours int, now time.Time) []PromptEntry {
	if windowHours <= 0 {
		windowHours = promptHistoryDefaultWindowHours
	}
	if windowHours > promptHistoryMaxWindowHours {
		windowHours = promptHistoryMaxWindowHours
	}
	cutoff := now.Add(-time.Duration(windowHours) * time.Hour)

	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]PromptEntry, 0, len(p.ring))
	for _, e := range p.ring {
		if agent != "" && e.Agent != agent {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.Timestamp)
		// An unparseable timestamp cannot be placed in the window. Skip it
		// rather than surfacing an entry the user cannot situate in time.
		if err != nil || ts.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}

	// Newest first. Sort rather than reverse: the ring is append-ordered by
	// wall clock, but entries reloaded from disk after a restart can interleave
	// with fresh ones, so ordering is not guaranteed by construction.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp > out[j].Timestamp })

	if len(out) > promptHistoryMaxResults {
		out = out[:promptHistoryMaxResults]
	}
	return out
}

// handlePromptHistory serves GET /api/prompt-history.
//
// Access fails closed exactly like GET /api/audit (handleAuditLog): prompt
// bodies embed repo names, issue titles and knowledge-base content, so this
// must not be readable by anyone who cannot already read the audit log.
func (s *Server) handlePromptHistory(w http.ResponseWriter, r *http.Request) {
	role := r.Header.Get("X-Hive-Role")
	if !config.RoleAtLeast(role, config.RoleReadWrite) {
		http.Error(w, "insufficient access", http.StatusForbidden)
		return
	}

	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	windowHours := promptHistoryDefaultWindowHours
	if raw := r.URL.Query().Get("windowHours"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			windowHours = v
		}
	}

	entries := s.promptHistory.Query(agent, windowHours, time.Now())
	jsonResponse(w, map[string]any{
		"entries":     entries,
		"windowHours": windowHours,
	})
}

// RecordPrompt persists a delivered kick prompt from non-HTTP contexts (the
// agent manager's kick and startup-bootstrap paths).
func (s *Server) RecordPrompt(agent, trigger, prompt string) {
	if s == nil || s.promptHistory == nil {
		return
	}
	s.promptHistory.Record(agent, trigger, prompt)
}

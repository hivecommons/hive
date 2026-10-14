package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/config"
)

// Kick log transcript source (hivecommons/hive#11762, #9579).
//
// A tmux scrollback capture is a scrape of what the CLI painted, and the CLIs
// collapse their own output ("(N lines hidden)", repainted frames), so even a
// complete capture cannot reproduce the commands an agent ran. Copilot CLI and
// Claude Code persist the whole conversation as JSONL under the agent's HOME;
// when the kick log source asks for it, the kick log is rendered from that
// transcript instead. The transcript is located with the same resume-handle
// walk the PR follow-up path uses (backend_resume.go) and is read only at
// archive time and on a live full-log read — no watchers, no polling.
//
// The source is governor.logging.kick_log_source (Settings → Logging → Kick
// logs), overridden by HIVE_KICK_LOG_SOURCE; see config/kick_log_source.go for
// the precedence. main.go injects the live resolver with
// SetKickLogSourceResolver so a dashboard change applies to the next archive
// or live read without a restart; without a resolver only the env var is read.

const (
	// kickLogSourceEnv selects what a kick log is built from.
	kickLogSourceEnv = config.KickLogSourceEnvVar

	// kickLogSourceScrollback is today's tmux capture-pane path (default).
	kickLogSourceScrollback = config.KickLogSourceScrollback
	// kickLogSourceTranscript renders the backend CLI's own transcript,
	// falling back to scrollback (with a warning) when none is usable so a
	// kick never loses its log.
	kickLogSourceTranscript = config.KickLogSourceTranscript
	// kickLogSourceAuto renders the transcript when the backend has a reader
	// and the transcript exists and parses, else scrollback. Backends without
	// a reader fall back silently.
	kickLogSourceAuto = config.KickLogSourceAuto

	// kickTranscriptOmittedFormat marks the middle of an over-cap transcript.
	kickTranscriptOmittedFormat = "\n[… %d bytes omitted …]\n"
)

// errNoKickTranscriptReader reports a backend with no transcript reader.
var errNoKickTranscriptReader = errors.New("no transcript reader for backend")

// kickTranscriptReaders renders one backend's transcript as plain text,
// skipping entries timestamped before since (zero since keeps everything).
var kickTranscriptReaders = map[string]func(r io.Reader, since time.Time) (string, error){
	"copilot": renderCopilotTranscript,
	"claude":  renderClaudeTranscript,
}

// ansiEscapeRe matches CSI, OSC and two-byte escape sequences.
var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// kickLogSourceFromEnv resolves HIVE_KICK_LOG_SOURCE; unset or unknown values
// keep the scrollback default.
func kickLogSourceFromEnv() string {
	return config.NormalizeKickLogSource(os.Getenv(kickLogSourceEnv))
}

// SetKickLogSourceResolver injects the resolver for the kick log source.
// Called from main.go with a closure over the live config
// (LoggingConfig.ResolveKickLogSource, which already applies the env
// override), so a source changed from the dashboard applies to the next
// archive or live full-log read without a restart. A nil fn clears it,
// restoring the boot-time env-only value.
func (m *Manager) SetKickLogSourceResolver(fn func() string) {
	// Atomic store — no m.mu — because the source is read from paths that
	// already hold m.mu (archiveKickLogLocked, kickLogSourceLocked).
	if fn == nil {
		m.kickLogSourceResolver.Store(nil)
		return
	}
	m.kickLogSourceResolver.Store(&fn)
}

// currentKickLogSource is the source in force now: the resolver's answer when
// one is injected, else the boot-time value.
func (m *Manager) currentKickLogSource() string {
	if fnp := m.kickLogSourceResolver.Load(); fnp != nil && *fnp != nil {
		return config.NormalizeKickLogSource((*fnp)())
	}
	if m.kickLogSource == "" {
		return kickLogSourceScrollback
	}
	return m.kickLogSource
}

// KickLogSource reports the kick log source in force now.
func (m *Manager) KickLogSource() string {
	return m.currentKickLogSource()
}

// kickTranscriptTarget is the lock-free snapshot of what locating an agent's
// transcript needs.
type kickTranscriptTarget struct {
	agentName string
	uid       int
	backend   string
	workDir   string
	// since is the kick's start; earlier transcript entries belong to
	// previous kicks of the same CLI session.
	since time.Time
}

// kickTranscriptTargetLocked snapshots agent for loadKickTranscript. Callers
// must hold m.mu (read or write).
func (m *Manager) kickTranscriptTargetLocked(agent *AgentProcess) kickTranscriptTarget {
	t := kickTranscriptTarget{
		agentName: agent.Name,
		uid:       agent.UID,
		backend:   strings.TrimSpace(effectiveBackend(agent)),
	}
	if m.workDir != "" {
		t.workDir = filepath.Join(m.workDir, agent.Name)
	}
	if agent.LastKick != nil {
		t.since = *agent.LastKick
	}
	return t
}

// usesKickTranscript reports whether source ever tries the transcript.
func usesKickTranscript(source string) bool {
	return source == kickLogSourceTranscript || source == kickLogSourceAuto
}

// loadKickTranscript renders the agent's current CLI transcript as plain text,
// or returns why it cannot.
func loadKickTranscript(t kickTranscriptTarget) (string, error) {
	render, ok := kickTranscriptReaders[t.backend]
	if !ok {
		return "", fmt.Errorf("%w %q", errNoKickTranscriptReader, t.backend)
	}
	h, ok := CaptureResumeHandleIn(t.agentName, t.uid, t.backend, t.workDir, time.Now())
	if !ok || !h.TranscriptExists() {
		return "", fmt.Errorf("no %s transcript found", t.backend)
	}
	if !t.since.IsZero() && h.LastActive.Before(t.since) {
		return "", fmt.Errorf("newest %s transcript %s predates the kick", t.backend, h.Transcript)
	}
	f, err := os.Open(h.Transcript)
	if err != nil {
		return "", fmt.Errorf("opening %s transcript: %w", t.backend, err)
	}
	defer func() { _ = f.Close() }()
	text, err := render(f, t.since)
	if err != nil {
		return "", fmt.Errorf("parsing %s transcript %s: %w", t.backend, h.Transcript, err)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s transcript %s has no entries for this kick", t.backend, h.Transcript)
	}
	return text, nil
}

// kickLogContent produces a kick log body and names its source. In the
// default scrollback mode it is exactly the scrollback capture. Otherwise the
// transcript is preferred, capped to the per-agent byte cap; a missing or
// unparseable transcript falls back to the scrollback with one warning.
func (m *Manager) kickLogContent(t kickTranscriptTarget, capture func() (string, error)) (content, source string, err error) {
	if configured := m.currentKickLogSource(); usesKickTranscript(configured) {
		text, terr := loadKickTranscript(t)
		if terr == nil {
			return capKickTranscript(text, m.kickLogMaxBytes), kickLogSourceTranscript, nil
		}
		if m.logger != nil && (configured == kickLogSourceTranscript || !errors.Is(terr, errNoKickTranscriptReader)) {
			m.logger.Warn("kick log: transcript unavailable, using scrollback", "name", t.agentName, "source", configured, "error", terr)
		}
	}
	content, err = capture()
	return content, kickLogSourceScrollback, err
}

// kickLogSourceLocked names the source a live full-log read would use now,
// without logging. Callers must hold m.mu (read or write).
func (m *Manager) kickLogSourceLocked(agent *AgentProcess) string {
	if !usesKickTranscript(m.currentKickLogSource()) {
		return kickLogSourceScrollback
	}
	if _, err := loadKickTranscript(m.kickTranscriptTargetLocked(agent)); err != nil {
		return kickLogSourceScrollback
	}
	return kickLogSourceTranscript
}

// capKickTranscript keeps the head and tail of a transcript larger than
// maxBytes, replacing the middle with an explicit omission marker.
func capKickTranscript(text string, maxBytes int64) string {
	if maxBytes <= 0 || int64(len(text)) <= maxBytes {
		return text
	}
	marker := fmt.Sprintf(kickTranscriptOmittedFormat, len(text)-int(maxBytes))
	keep := int(maxBytes) - len(marker)
	if keep < 0 {
		keep = 0
	}
	head := keep / 2
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	tailStart := len(text) - (keep - keep/2)
	for tailStart < len(text) && !utf8.RuneStart(text[tailStart]) {
		tailStart++
	}
	return text[:head] + fmt.Sprintf(kickTranscriptOmittedFormat, tailStart-head) + text[tailStart:]
}

// forEachTranscriptLine calls fn with every non-blank JSONL line. A final line
// without a newline that fails to parse is a write in progress and ignored;
// any other failure makes the transcript corrupt.
func forEachTranscriptLine(r io.Reader, fn func(line []byte) error) error {
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return err
		}
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			if perr := fn(trimmed); perr != nil {
				if err == io.EOF && !bytes.HasSuffix(line, []byte("\n")) {
					return nil
				}
				return fmt.Errorf("line %d: %w", n, perr)
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// parseTranscriptTime parses a transcript timestamp; unparseable is zero.
func parseTranscriptTime(s string) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// beforeKick reports whether an entry at ts belongs to an earlier kick.
func beforeKick(ts, since time.Time) bool {
	return !since.IsZero() && !ts.IsZero() && ts.Before(since)
}

// transcriptWriter renders transcript entries as plain text blocks, with any
// ANSI escapes stripped from their bodies:
//
//	[2026-10-14T15:00:00Z] heading
//	body
type transcriptWriter struct {
	b strings.Builder
}

func (w *transcriptWriter) entry(ts time.Time, heading string, bodies ...string) {
	stamp := "-"
	if !ts.IsZero() {
		stamp = ts.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(&w.b, "[%s] %s\n", stamp, heading)
	for _, body := range bodies {
		body = ansiEscapeRe.ReplaceAllString(body, "")
		if body == "" {
			continue
		}
		w.b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			w.b.WriteByte('\n')
		}
	}
	w.b.WriteByte('\n')
}

// toolCall renders one tool invocation: the shell command, when the
// arguments carry one, then the full arguments.
func (w *transcriptWriter) toolCall(ts time.Time, name, id string, args json.RawMessage) {
	var bodies []string
	if cmd := transcriptShellCommand(args); cmd != "" {
		bodies = append(bodies, "$ "+cmd)
	}
	if a := compactTranscriptJSON(args); a != "" {
		bodies = append(bodies, "args: "+a)
	}
	w.entry(ts, fmt.Sprintf("tool call: %s (%s)", name, id), bodies...)
}

func (w *transcriptWriter) String() string { return w.b.String() }

// transcriptText renders a JSON value that is usually a string; objects with
// a message/content/text field yield that field, anything else its JSON.
func transcriptText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Message *string `json:"message"`
		Content *string `json:"content"`
		Text    *string `json:"text"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		for _, v := range []*string{obj.Message, obj.Content, obj.Text} {
			if v != nil {
				return *v
			}
		}
	}
	return compactTranscriptJSON(raw)
}

// transcriptShellCommand extracts the "command" argument of a shell tool
// call. Arguments encoded as a JSON string holding an object are unwrapped.
func transcriptShellCommand(args json.RawMessage) string {
	var s string
	if json.Unmarshal(args, &s) == nil {
		args = json.RawMessage(s)
	}
	var a struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	return a.Command
}

func compactTranscriptJSON(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

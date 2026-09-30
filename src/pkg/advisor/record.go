package advisor

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Skip reasons — every review produces exactly one record, including a review
// that was skipped, and the record says why.
const (
	SkipTimeout         = "timeout"
	SkipUnreachable     = "unreachable"
	SkipBudgetExhausted = "budget-exhausted"
	SkipUnusableAnswer  = "unusable-answer"
	// SkipInputBlocked records a turn whose transcript ioscan refused to hand
	// to the advisor. The agent proceeds — the lane fails open — but the
	// operator can see the review did not happen and why.
	SkipInputBlocked = "input-blocked"
)

// Heeded markers: whether the agent's following turn acted on an
// interjection. Undetermined is an honest answer and is the phase-1 default;
// the judging pass is a later phase.
const (
	HeededYes          = "yes"
	HeededNo           = "no"
	HeededUndetermined = "undetermined"
)

// Record is one advisor review — a first-class hive record: which agent,
// which turn, what severity, what was said, whether an interjection was made,
// what it cost, and when skipped, why. Shaped so trajectory verdicts and
// advisor interjections can be listed together on one per-agent timeline.
type Record struct {
	Timestamp string `json:"ts"`
	Agent     string `json:"agent"`
	// Turn is the backend's reference for the reviewed turn (the Claude Code
	// session id in phase 1).
	Turn  string `json:"turn,omitempty"`
	Model string `json:"model,omitempty"`
	// Severity is aside/concern/blocker; empty on a skipped review.
	Severity string `json:"severity,omitempty"`
	Text     string `json:"text,omitempty"`
	// Interjected reports whether the text was delivered into the session.
	Interjected bool   `json:"interjected"`
	Delivery    string `json:"delivery,omitempty"`
	// Downgraded marks a blocker delivered as an aside because the
	// consecutive-block limit was reached.
	Downgraded   bool    `json:"downgraded,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	// Skipped carries the skip reason when the review did not happen.
	Skipped string `json:"skipped,omitempty"`
	Heeded  string `json:"heeded,omitempty"`
}

const (
	// recordRingCap bounds the in-memory ring the REST listing serves from —
	// same posture as the dashboard audit ring.
	recordRingCap = 500
	// Rotation bounds for the on-disk JSONL, mirroring the audit log's.
	recordMaxSizeMB  = 5
	recordMaxBackups = 3
	recordMaxAgeDays = 90
)

// DefaultRecordsPath is where the hub persists advisor records.
const DefaultRecordsPath = "/data/advisor-records.jsonl"

// Store keeps advisor records: an append-only JSONL on disk (rotated) plus an
// in-memory ring the listing endpoints serve from. Safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	writer *lumberjack.Logger
	ring   []Record
}

// NewStore opens (or creates) the record store at path. When the parent
// directory does not exist the store is memory-only — the same degradation
// the audit log applies on hosts without /data.
func NewStore(path string) *Store {
	s := &Store{ring: make([]Record, 0, recordRingCap)}
	if path == "" {
		return s
	}
	dir := path[:strings.LastIndex(path, "/")+1]
	if dir != "" {
		if _, err := os.Stat(strings.TrimRight(dir, "/")); err != nil {
			return s
		}
	}
	s.writer = &lumberjack.Logger{
		Filename:   path,
		MaxSize:    recordMaxSizeMB,
		MaxBackups: recordMaxBackups,
		MaxAge:     recordMaxAgeDays,
		Compress:   true,
	}
	s.loadFromDisk(path)
	return s
}

// loadFromDisk restores the ring's tail from the current JSONL so records
// survive a hive restart. Malformed lines are skipped, not fatal.
func (s *Store) loadFromDisk(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var rec Record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		s.appendToRing(rec)
	}
}

func (s *Store) appendToRing(rec Record) {
	if len(s.ring) >= recordRingCap {
		s.ring = s.ring[1:]
	}
	s.ring = append(s.ring, rec)
}

// Append records one review. A zero Timestamp is stamped with the current
// UTC time.
func (s *Store) Append(rec Record) {
	if rec.Timestamp == "" {
		rec.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendToRing(rec)
	if s.writer != nil {
		if data, err := json.Marshal(rec); err == nil {
			_, _ = s.writer.Write(append(data, '\n'))
		}
	}
}

// List returns records newest first, filtered by agent (empty = all) and by
// since (zero = all), capped at limit (<=0 = all retained).
func (s *Store) List(agent string, since time.Time, limit int) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.ring))
	for i := len(s.ring) - 1; i >= 0; i-- {
		rec := s.ring[i]
		if agent != "" && rec.Agent != agent {
			continue
		}
		if !since.IsZero() {
			ts, err := time.Parse(time.RFC3339, rec.Timestamp)
			if err != nil || ts.Before(since) {
				continue
			}
		}
		out = append(out, rec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

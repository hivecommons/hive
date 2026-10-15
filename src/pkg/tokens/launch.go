package tokens

// Launch attribution (#12128): the token collector already knows how many
// tokens each agent session burned and when (UsageEvent timelines), and the
// repo-cost join already places those tokens against audited repo output. What
// it could not answer was "which issue / PR / head SHA was this turn launched
// FOR?" — the question behind repeat-spend on the same work item.
//
// A LaunchRecord is written by the kick path at the moment a kick is
// delivered, carrying the work refs the kick named. It is attribution only:
// nothing here gates, budgets, or refuses a kick. The enforced window budget
// in pkg/governor stays the single budget gate (RFC #12134, constraint D).

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// LaunchRefKindIssue marks a ref parsed from an "owner/repo#N" issue key.
	LaunchRefKindIssue = "issue"
	// LaunchRefKindPR marks a ref the review swarm dispatched for a pull
	// request; these carry the head SHA the review was launched against.
	LaunchRefKindPR = "pr"
	// LaunchRefKindExternal marks a non-GitHub work key ("repo!EXT-1" from
	// Linear/Jira sources); kept verbatim so external work is not dropped.
	LaunchRefKindExternal = "external"

	// DefaultLaunchLogPath is the durable JSONL log on the spoke PVC.
	DefaultLaunchLogPath = "/data/launch-attribution.jsonl"

	// launchLogRingCap bounds the in-memory records served to the cost join.
	// At the busiest shipped cadence (~52 kicks/hour) 4000 records is ~3 days;
	// older launches age out of the ring (and so out of attribution) while the
	// repo-cost window the join shares is 14 days — a bounded, documented gap.
	launchLogRingCap = 4000

	// launchLogCompactEvery rewrites the JSONL file from the ring after this
	// many appends so the on-disk file cannot grow without bound.
	launchLogCompactEvery = launchLogRingCap

	// launchLogMaxRefs bounds the refs stored per record. The governor caps
	// issue lists per kick already (kick_limits.max_issues, default 100); this
	// is a safety net against a misconfigured cap bloating the log.
	launchLogMaxRefs = 200
)

// LaunchRef is one work item a kick was launched for.
type LaunchRef struct {
	// Ref is the canonical work key ("owner/repo#123", "repo!EXT-1").
	Ref  string `json:"ref"`
	Kind string `json:"kind"`
	Repo string `json:"repo,omitempty"`
	// Number is the issue or PR number for GitHub-backed refs, 0 otherwise.
	Number int `json:"number,omitempty"`
	// HeadSHA is the PR head the launch targeted, when the dispatcher knew it
	// (review swarm kicks). Empty for issue kicks.
	HeadSHA string `json:"head_sha,omitempty"`
}

// ParseLaunchRef turns a scheduler work key into a LaunchRef. "owner/repo#N"
// becomes an issue ref; anything else non-empty is kept verbatim as external.
// Returns ok=false only for an empty key.
func ParseLaunchRef(key string) (LaunchRef, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return LaunchRef{}, false
	}
	if i := strings.LastIndex(key, "#"); i > 0 && i < len(key)-1 {
		if n, err := strconv.Atoi(key[i+1:]); err == nil && n > 0 {
			return LaunchRef{Ref: key, Kind: LaunchRefKindIssue, Repo: key[:i], Number: n}, true
		}
	}
	return LaunchRef{Ref: key, Kind: LaunchRefKindExternal}, true
}

// PRLaunchRef builds a pull-request ref with its head SHA.
func PRLaunchRef(repo string, number int, headSHA string) LaunchRef {
	return LaunchRef{
		Ref:     repo + "#" + strconv.Itoa(number),
		Kind:    LaunchRefKindPR,
		Repo:    repo,
		Number:  number,
		HeadSHA: strings.TrimSpace(headSHA),
	}
}

// LaunchRecord is one delivered kick and the work it named.
type LaunchRecord struct {
	Agent string `json:"agent"`
	// Trigger names the dispatcher: "governor-eval", "review-<kind>", etc.
	Trigger string `json:"trigger"`
	// Repo is the cadence-target repo for repo-scoped kicks, else "".
	Repo         string      `json:"repo,omitempty"`
	Refs         []LaunchRef `json:"refs,omitempty"`
	LaunchedAtMs int64       `json:"launched_at_ms"`
}

// LaunchLog is a durable, bounded append log of LaunchRecords.
type LaunchLog struct {
	mu           sync.Mutex
	path         string
	logger       *slog.Logger
	ring         []LaunchRecord
	sinceCompact int
}

// NewLaunchLog opens (or creates on first write) the JSONL log at path and
// restores the most recent launchLogRingCap records. path=="" keeps the log in
// memory only.
func NewLaunchLog(path string, logger *slog.Logger) *LaunchLog {
	if logger == nil {
		logger = slog.Default()
	}
	l := &LaunchLog{path: path, logger: logger}
	l.load()
	return l
}

func (l *LaunchLog) load() {
	if l.path == "" {
		return
	}
	f, err := os.Open(l.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			l.logger.Warn("launch log unreadable — starting fresh", "path", l.path, "error", err)
		}
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var rec LaunchRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.Agent == "" || rec.LaunchedAtMs == 0 {
			continue
		}
		l.ring = append(l.ring, rec)
		if len(l.ring) > launchLogRingCap {
			l.ring = l.ring[1:]
		}
	}
	if err := sc.Err(); err != nil {
		l.logger.Warn("launch log partially read", "path", l.path, "error", err)
	}
}

// Record appends rec. Records without an agent or timestamp are dropped; refs
// beyond launchLogMaxRefs are truncated.
func (l *LaunchLog) Record(rec LaunchRecord) {
	if l == nil || rec.Agent == "" {
		return
	}
	if rec.LaunchedAtMs == 0 {
		rec.LaunchedAtMs = time.Now().UnixMilli()
	}
	if len(rec.Refs) > launchLogMaxRefs {
		rec.Refs = rec.Refs[:launchLogMaxRefs]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring = append(l.ring, rec)
	if len(l.ring) > launchLogRingCap {
		l.ring = l.ring[1:]
	}
	l.sinceCompact++
	if l.sinceCompact >= launchLogCompactEvery {
		l.rewriteLocked()
		return
	}
	l.appendLocked(rec)
}

func (l *LaunchLog) appendLocked(rec LaunchRecord) {
	if l.path == "" {
		return
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		l.logger.Warn("launch log dir unavailable", "path", l.path, "error", err)
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		l.logger.Warn("launch log append failed", "path", l.path, "error", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		l.logger.Warn("launch log write failed", "path", l.path, "error", err)
	}
}

// rewriteLocked compacts the file to exactly the ring contents.
func (l *LaunchLog) rewriteLocked() {
	l.sinceCompact = 0
	if l.path == "" {
		return
	}
	var buf strings.Builder
	for _, rec := range l.ring {
		data, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o600); err != nil {
		l.logger.Warn("launch log compaction failed", "path", l.path, "error", err)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		l.logger.Warn("launch log compaction rename failed", "path", l.path, "error", err)
	}
}

// Records returns a copy of the retained records in launch order.
func (l *LaunchLog) Records() []LaunchRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]LaunchRecord, len(l.ring))
	copy(out, l.ring)
	sort.SliceStable(out, func(i, j int) bool { return out[i].LaunchedAtMs < out[j].LaunchedAtMs })
	return out
}

// SetLaunchLog attaches the launch log the kick path writes to. Nil detaches.
func (c *Collector) SetLaunchLog(l *LaunchLog) {
	c.mu.Lock()
	c.launchLog = l
	c.mu.Unlock()
}

// RecordLaunch records a delivered kick for attribution. No-op when no launch
// log is attached, so callers never need a nil check.
func (c *Collector) RecordLaunch(rec LaunchRecord) {
	if c == nil {
		return
	}
	c.mu.RLock()
	l := c.launchLog
	c.mu.RUnlock()
	l.Record(rec)
}

// Launches returns the retained launch records (nil when no log is attached).
func (c *Collector) Launches() []LaunchRecord {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	l := c.launchLog
	c.mu.RUnlock()
	return l.Records()
}

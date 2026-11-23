package ioscan

import (
	"strings"
	"sync"
	"time"
)

// DefaultSourceMemoryDays is how long a source stays flagged after it trips a
// High or Critical injection block (hivecommons/hive#19440).
const DefaultSourceMemoryDays = 30

// DefaultSourceMemoryEntries bounds each of the author and thread maps so an
// attacker spraying fresh accounts cannot grow the memory without limit.
const DefaultSourceMemoryEntries = 10000

// SourceFlaggedRule names the finding recorded when text from a flagged
// source is withheld because no classifier could vouch for it.
const SourceFlaggedRule = "injection.source_flagged"

// SourceMemory remembers the authors and issue/PR threads whose untrusted text
// tripped a High or Critical injection block, for a fixed window. The content
// hash cache alone lets an attacker edit a withheld comment slightly and get
// scored fresh until one variant passes; a flagged source instead keeps
// getting the strict path (always classified, fail-closed) whatever later
// variants score. The memory is in-process: a restart clears it.
type SourceMemory struct {
	ttl     time.Duration
	max     int
	now     func() time.Time
	mu      sync.Mutex
	authors map[string]time.Time
	threads map[string]time.Time
}

// NewSourceMemory returns a memory that keeps a flag for ttl. A ttl of zero or
// less returns nil, and a nil *SourceMemory flags nothing.
func NewSourceMemory(ttl time.Duration) *SourceMemory {
	if ttl <= 0 {
		return nil
	}
	return &SourceMemory{
		ttl:     ttl,
		max:     DefaultSourceMemoryEntries,
		now:     time.Now,
		authors: make(map[string]time.Time),
		threads: make(map[string]time.Time),
	}
}

// ShouldFlagSource reports whether v is the kind of block that flags its
// source: text withheld for a High or Critical injection finding.
func ShouldFlagSource(v Verdict) bool {
	if !v.Blocked {
		return false
	}
	for _, f := range v.Findings {
		if f.Kind == KindInjection && f.Severity >= SeverityHigh && f.Rule != SemanticClassifierUnavailableRule && f.Rule != SourceFlaggedRule {
			return true
		}
	}
	return false
}

// Flag records src's author and thread. Trusted sources are never flagged:
// the hive's own identities and repo owners/members are not the retry
// attacker this guards against.
func (m *SourceMemory) Flag(src Source) {
	if m == nil || src.TrustedAuthor {
		return
	}
	author, thread := normalizeSourceKey(src.Author), normalizeSourceKey(src.Thread)
	if author == "" && thread == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	until := m.now().Add(m.ttl)
	if author != "" {
		m.put(m.authors, author, until)
	}
	if thread != "" {
		m.put(m.threads, thread, until)
	}
}

// Flagged reports whether src's author or thread is still flagged. Trusted
// sources are never treated as flagged.
func (m *SourceMemory) Flagged(src Source) bool {
	if m == nil || src.TrustedAuthor {
		return false
	}
	author, thread := normalizeSourceKey(src.Author), normalizeSourceKey(src.Thread)
	if author == "" && thread == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	return m.live(m.authors, author, now) || m.live(m.threads, thread, now)
}

func (m *SourceMemory) live(set map[string]time.Time, key string, now time.Time) bool {
	if key == "" {
		return false
	}
	until, ok := set[key]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(set, key)
		return false
	}
	return true
}

// put stores key, first dropping expired entries and then, if still full,
// the entry closest to expiry.
func (m *SourceMemory) put(set map[string]time.Time, key string, until time.Time) {
	if _, ok := set[key]; !ok && len(set) >= m.max {
		now := m.now()
		oldestKey, oldest := "", time.Time{}
		for k, t := range set {
			if !now.Before(t) {
				delete(set, k)
				continue
			}
			if oldestKey == "" || t.Before(oldest) {
				oldestKey, oldest = k, t
			}
		}
		if len(set) >= m.max && oldestKey != "" {
			delete(set, oldestKey)
		}
	}
	set[key] = until
}

func normalizeSourceKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// SourceFlaggedVerdict renders text withheld because its source is flagged
// and no classifier answer could clear it. It is High, not Critical: the
// segment is withheld but the rest of the kick goes on.
func SourceFlaggedVerdict(reason string) Verdict {
	return Verdict{Findings: []Finding{{
		Kind:     KindInjection,
		Severity: SeverityHigh,
		Snippet:  snippet(reason),
		Rule:     SourceFlaggedRule,
	}}, Blocked: true}
}

// SourceFlaggedRedactionMarker is the visible marker for text withheld from a
// flagged source.
func SourceFlaggedRedactionMarker(reason string) string {
	return strings.Replace(redactionMarker, "%s", SourceFlaggedRule+"("+reason+")", 1)
}

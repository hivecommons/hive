package live

import (
	"sync"
	"time"
)

// Policy defaults.
const (
	// DefaultAnswerWindow is how long after an ask hive treats the next
	// human line as its answer.
	DefaultAnswerWindow = 2 * time.Minute
	// ChatMaxChars mirrors the contract's QUORUM_CHAT_MAX_CHARS default;
	// longer posts are truncated.
	ChatMaxChars = 2000
	// maxFindings bounds the findings kept per research item.
	maxFindings = 20
	// rateWindow is the chat rate-limit window.
	rateWindow = time.Minute
	// ledgerHour and ledgerDay are the hive-wide budget windows.
	ledgerHour = time.Hour
	ledgerDay  = 24 * time.Hour
)

// Policy is the per-meeting policy configuration.
type Policy struct {
	ChatPostsPerMinute int
	// ChatAnswerBurst is the extra chat posts a minute the persona's
	// answers to addressed requests may take once ChatPostsPerMinute is
	// spent; zero means none.
	ChatAnswerBurst      int
	ResearchPerMeeting   int
	VoiceOnAddress       bool
	MaxTokensPerMeeting  int
	MaxIntentsPerMeeting int
	AnswerWindow         time.Duration
}

// answerBurst is the extra chat room for addressed answers: none when the
// chat rate is unlimited or no burst is set.
func answerBurst(p Policy) rateLimiter {
	if p.ChatPostsPerMinute <= 0 || p.ChatAnswerBurst <= 0 {
		return rateLimiter{}
	}
	return rateLimiter{n: p.ChatAnswerBurst, window: rateWindow}
}

func (p Policy) withDefaults() Policy {
	if p.AnswerWindow <= 0 {
		p.AnswerWindow = DefaultAnswerWindow
	}
	return p
}

// rateLimiter allows n events per rolling window.
type rateLimiter struct {
	n      int
	window time.Duration
	times  []time.Time
}

func (r *rateLimiter) allow(now time.Time) bool {
	if r.n <= 0 {
		return true
	}
	cut := now.Add(-r.window)
	kept := r.times[:0]
	for _, t := range r.times {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	r.times = kept
	if len(r.times) >= r.n {
		return false
	}
	r.times = append(r.times, now)
	return true
}

// Ledger is the hive-wide meetings LLM budget across all meetings: hourly
// and daily token ceilings. Safe for concurrent use.
type Ledger struct {
	mu      sync.Mutex
	hourly  int
	daily   int
	entries []ledgerEntry
}

type ledgerEntry struct {
	at     time.Time
	tokens int
}

// NewLedger returns a ledger with the given ceilings (zero or negative
// means no ceiling).
func NewLedger(hourly, daily int) *Ledger { return &Ledger{hourly: hourly, daily: daily} }

// Add records tokens spent at now.
func (l *Ledger) Add(now time.Time, tokens int) {
	if l == nil || tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, ledgerEntry{at: now, tokens: tokens})
}

// Exceeded reports whether either ceiling is reached at now.
func (l *Ledger) Exceeded(now time.Time) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	dayCut, hourCut := now.Add(-ledgerDay), now.Add(-ledgerHour)
	kept := l.entries[:0]
	hour, day := 0, 0
	for _, e := range l.entries {
		if !e.at.After(dayCut) {
			continue
		}
		kept = append(kept, e)
		day += e.tokens
		if e.at.After(hourCut) {
			hour += e.tokens
		}
	}
	l.entries = kept
	return (l.hourly > 0 && hour >= l.hourly) || (l.daily > 0 && day >= l.daily)
}

// LedgerUsage is the hive-wide meetings spend in the last hour and day
// against the ceilings (zero ceiling: none).
type LedgerUsage struct {
	HourlyUsed    int `json:"hourly_used"`
	HourlyCeiling int `json:"hourly_ceiling"`
	DailyUsed     int `json:"daily_used"`
	DailyCeiling  int `json:"daily_ceiling"`
}

// Usage reports the spend in the rolling hour and day at now.
func (l *Ledger) Usage(now time.Time) LedgerUsage {
	if l == nil {
		return LedgerUsage{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u := LedgerUsage{HourlyCeiling: l.hourly, DailyCeiling: l.daily}
	dayCut, hourCut := now.Add(-ledgerDay), now.Add(-ledgerHour)
	for _, e := range l.entries {
		if !e.at.After(dayCut) {
			continue
		}
		u.DailyUsed += e.tokens
		if e.at.After(hourCut) {
			u.HourlyUsed += e.tokens
		}
	}
	return u
}

// truncate caps text at limit runes.
func truncate(text string, limit int) string {
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	const ellipsis = "…"
	return string(r[:limit-1]) + ellipsis
}

package ask

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/keystore"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/config"
)

// Outcomes. These are the only values that appear in audit lines and counters.
const (
	OutcomeOK                = "ok"
	OutcomeNoCitedAnswer     = "no_cited_answer"
	OutcomeRefusedSecret     = "refused_secret_shape"
	OutcomeRefusedLength     = "refused_length"
	OutcomeRefusedEmpty      = "refused_empty"
	OutcomeRateLimited       = "rate_limited"
	OutcomeBudgetSpent       = "budget_spent"
	OutcomeBreakerOpen       = "breaker_open"
	OutcomeUpstreamError     = "upstream_error"
	OutcomeNotConsented      = "not_consented"
	breakerThreshold         = 5
	breakerCooldown          = time.Minute
	rateLimitAuditWindow     = 10 * time.Minute
	maxThreadHandleChars     = 64
	maxSectionIDChars        = 64
	headerSectionLabelPrefix = "Dashboard section: "
)

// Chatter is the vendor call. *Client implements it; tests pass a fake.
type Chatter interface {
	Chat(ctx context.Context, query, threadID string) (ChatResponse, error)
}

// Request is one question from one signed-in, consented person.
type Request struct {
	// Person is the stem other audit lines use for the person. It is never
	// sent to the vendor.
	Person string
	// Hive is the relaying hive's id for a hub relay request, "" otherwise. It
	// is never sent to the vendor.
	Hive string
	// Question is what the person typed.
	Question string
	// Section is a dashboard section id; only ids in the closed vocabulary
	// are used.
	Section string
	// Thread is the opaque handle returned by an earlier answer, or "".
	Thread string
}

// Result is the outcome of one question. It carries no vendor identifier.
type Result struct {
	// Outcome is one of the Outcome constants.
	Outcome string `json:"outcome"`
	// Answer is plain text, set only when Outcome is OutcomeOK.
	Answer string `json:"answer,omitempty"`
	// Sources are the allowlisted citations, at least one when Outcome is OK.
	Sources []Source `json:"sources,omitempty"`
	// Uncertain is the vendor's own uncertainty flag.
	Uncertain bool `json:"uncertain,omitempty"`
	// Thread is the handle to send with the next question.
	Thread string `json:"thread,omitempty"`
	// Message is a short fixed explanation for a non-OK outcome.
	Message string `json:"message,omitempty"`
	// RetryAfterSeconds is set for rate_limited and budget_spent.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

var outcomeMessages = map[string]string{
	OutcomeNoCitedAnswer: "No cited answer found in the docs.",
	OutcomeRefusedSecret: "That looks like it contains a credential. Remove it and ask again.",
	OutcomeRefusedLength: "That question is too long. Shorten it and ask again.",
	OutcomeRefusedEmpty:  "Type a question first.",
	OutcomeRateLimited:   "Ask is busy. Try again shortly.",
	OutcomeBudgetSpent:   "Ask has reached its daily limit. Use the docs link for now.",
	OutcomeBreakerOpen:   "Ask is unavailable right now. Use the docs link.",
	OutcomeUpstreamError: "Ask is unavailable right now. Use the docs link.",
	OutcomeNotConsented:  "Confirm the disclosure before asking.",
}

// Service answers questions: it validates, limits, calls the vendor and
// filters what comes back.
type Service struct {
	settings config.AskSettings
	chat     Chatter
	lim      *Limiter
	br       *Breaker
	threads  *threads
	logger   *slog.Logger

	mu           sync.Mutex
	counts       map[string]int64
	lastLimitLog map[string]time.Time
	now          func() time.Time
}

// Options are test seams. The zero value is production behaviour.
type Options struct {
	Now func() time.Time
	// BaseURL overrides the vendor host (tests point it at a fake server).
	BaseURL string
	// Chatter replaces the vendor client entirely.
	Chatter Chatter
	// ThreadTTL overrides DefaultThreadTTL.
	ThreadTTL time.Duration
}

// ErrNotActive is returned by Open when the settings are not active.
var ErrNotActive = errors.New("ask: not active")

// ErrKeyUnreadable is returned by Open when the key file cannot be used. It
// names no path and no value.
var ErrKeyUnreadable = errors.New("ask: api key file is unreadable or empty")

// Open builds a Service from resolved settings, reading the key file. It
// returns ErrNotActive for inactive settings and ErrKeyUnreadable when the key
// cannot be loaded; in both cases the feature stays off and the caller logs one
// warning with the settings' Reason, never a value.
func Open(settings config.AskSettings, logger *slog.Logger, opts Options) (*Service, error) {
	if !settings.Active {
		return nil, ErrNotActive
	}
	chat := opts.Chatter
	if chat == nil {
		key, err := loadKey(settings.APIKeyFile)
		if err != nil {
			return nil, err
		}
		chat = NewClient(opts.BaseURL, settings.ProjectID, settings.IntegrationID, key,
			time.Duration(settings.Limits.TimeoutSeconds)*time.Second)
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{
		settings:     settings,
		chat:         chat,
		lim:          NewLimiter(settings.Limits, opts.Now),
		br:           NewBreaker(breakerThreshold, breakerCooldown, opts.Now),
		threads:      newThreads(opts.ThreadTTL, opts.Now),
		logger:       logger,
		counts:       map[string]int64{},
		lastLimitLog: map[string]time.Time{},
		now:          opts.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.br.OnOpen = func() { logger.Warn("audit: ask circuit breaker opened") }
	s.br.OnClose = func() { logger.Info("audit: ask circuit breaker closed") }
	return s, nil
}

func loadKey(path string) (string, error) {
	if !config.SecretFilePathAllowed(path) {
		return "", ErrKeyUnreadable
	}
	b, err := keystore.ReadFile(context.Background(), path)
	if err != nil {
		return "", ErrKeyUnreadable
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", ErrKeyUnreadable
	}
	return key, nil
}

// Limits returns the resolved limits. It is the only part of the settings the
// service exposes: the vendor values stay private to the client.
func (s *Service) Limits() config.AskLimits { return s.settings.Limits }

// Counters returns requests by outcome since start, plus the number admitted
// to the vendor today.
func (s *Service) Counters() (byOutcome map[string]int64, today int) {
	s.mu.Lock()
	out := make(map[string]int64, len(s.counts))
	for k, v := range s.counts {
		out[k] = v
	}
	s.mu.Unlock()
	return out, s.lim.DailyCount()
}

// Forget drops the person's conversation handles (consent withdrawn).
func (s *Service) Forget(person string) { s.threads.forget(person) }

// RecordNotConsented counts and audits a request refused before reaching
// Ask because the person has no current consent.
func (s *Service) RecordNotConsented(person, hive string, questionLen int) Result {
	return s.finish(person, hive, Result{Outcome: OutcomeNotConsented}, questionLen, 0, time.Time{})
}

// Ask answers one question.
func (s *Service) Ask(ctx context.Context, req Request) Result {
	start := s.now()
	qlen := utf8.RuneCountInString(req.Question)
	q := strings.TrimSpace(req.Question)
	switch {
	case q == "":
		return s.finish(req.Person, req.Hive, Result{Outcome: OutcomeRefusedEmpty}, qlen, 0, start)
	case utf8.RuneCountInString(q) > s.settings.Limits.QuestionChars:
		return s.finish(req.Person, req.Hive, Result{Outcome: OutcomeRefusedLength}, qlen, 0, start)
	}
	if _, found := logscrub.FindSecret(q); found {
		return s.finish(req.Person, req.Hive, Result{Outcome: OutcomeRefusedSecret}, qlen, 0, start)
	}

	release, denial, wait := s.lim.Acquire(req.Person, req.Hive)
	if denial != DenyNone {
		r := Result{Outcome: OutcomeRateLimited, RetryAfterSeconds: ceilSeconds(wait)}
		if denial == DenyBudget {
			r.Outcome = OutcomeBudgetSpent
		}
		return s.finish(req.Person, req.Hive, r, qlen, 0, start)
	}
	defer release()
	if !s.br.Allow() {
		return s.finish(req.Person, req.Hive, Result{Outcome: OutcomeBreakerOpen}, qlen, 0, start)
	}

	query := q
	if s.settings.SendSectionLabel && len(req.Section) <= maxSectionIDChars {
		if label := SectionLabel(req.Section); label != "" {
			query = headerSectionLabelPrefix + label + "\n\n" + q
		}
	}
	handle := req.Thread
	if len(handle) > maxThreadHandleChars {
		handle = ""
	}
	vendorThread := s.threads.lookup(handle, req.Person, req.Hive)

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(s.settings.Limits.TimeoutSeconds)*time.Second)
	defer cancel()
	resp, err := s.chat.Chat(callCtx, query, vendorThread)
	if err != nil {
		s.br.Failure()
		r := Result{Outcome: OutcomeUpstreamError}
		if errors.Is(err, ErrRateLimited) {
			r = Result{Outcome: OutcomeRateLimited, RetryAfterSeconds: 30}
		}
		return s.finish(req.Person, req.Hive, r, qlen, 0, start)
	}
	s.br.Success()

	sources := FilterSources(resp.RelevantSources)
	answer := PlainText(resp.Answer, s.settings.Limits.AnswerChars)
	if len(sources) == 0 || answer == "" {
		return s.finish(req.Person, req.Hive, Result{Outcome: OutcomeNoCitedAnswer}, qlen, 0, start)
	}
	out := Result{Outcome: OutcomeOK, Answer: answer, Sources: sources, Uncertain: resp.IsUncertain}
	if resp.ThreadID != "" {
		out.Thread = s.threads.store(handle, req.Person, req.Hive, resp.ThreadID)
	}
	return s.finish(req.Person, req.Hive, out, qlen, len(sources), start)
}

func ceilSeconds(d time.Duration) int {
	n := int((d + time.Second - 1) / time.Second)
	if n < 1 {
		n = 1
	}
	return n
}

// finish counts and audits one request and fills the fixed message. The audit
// line carries the outcome and counts only: never the question, the answer, the
// sources, a thread id, the key, the project id or the integration id.
func (s *Service) finish(person, hive string, r Result, qlen, nsources int, start time.Time) Result {
	if r.Message == "" {
		r.Message = outcomeMessages[r.Outcome]
	}
	now := s.now()
	s.mu.Lock()
	s.counts[r.Outcome]++
	logIt := true
	if r.Outcome == OutcomeRateLimited || r.Outcome == OutcomeBudgetSpent {
		// One audit line per person per window, not one per request.
		key := r.Outcome + "|" + person
		if last, ok := s.lastLimitLog[key]; ok && now.Sub(last) < rateLimitAuditWindow {
			logIt = false
		} else {
			s.lastLimitLog[key] = now
			if len(s.lastLimitLog) > 4096 {
				s.lastLimitLog = map[string]time.Time{key: now}
			}
		}
	}
	s.mu.Unlock()
	if logIt {
		var latency int64
		if !start.IsZero() {
			latency = now.Sub(start).Milliseconds()
		}
		attrs := []any{"person", person, "outcome", r.Outcome, "question_len", qlen,
			"sources", nsources, "latency_ms", latency}
		if hive != "" {
			attrs = append(attrs, "hive", hive)
		}
		s.logger.Info("audit: ask request", attrs...)
	}
	return r
}

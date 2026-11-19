package askai

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/logscrub"
)

// Outcome is the result class of one request. Audit lines and counters use
// it; it never carries text.
type Outcome string

const (
	OutcomeOK                 Outcome = "ok"
	OutcomeNoCitedAnswer      Outcome = "no_cited_answer"
	OutcomeRefusedSecretShape Outcome = "refused_secret_shape"
	OutcomeRateLimited        Outcome = "rate_limited"
	OutcomeBreakerOpen        Outcome = "breaker_open"
	OutcomeUpstreamError      Outcome = "upstream_error"
	OutcomeNotConsented       Outcome = "not_consented"
	OutcomeInvalidQuestion    Outcome = "invalid_question"
	OutcomeRelayDisabled      Outcome = "relay_disabled"
)

const (
	// MaxQuestionRunes caps the question text.
	MaxQuestionRunes = 1000
	// sectionLabelPrefix introduces the closed-vocabulary section label in
	// the query text.
	sectionLabelPrefix = "Dashboard section: "
)

// Plain-language messages the panel shows. None echoes input.
const (
	msgNoCitedAnswer = "No cited answer found in the docs. Try the docs link."
	msgRefused       = "This looks like it contains a password, token or key, so it was not sent. Remove it and ask again."
	msgUnavailable   = "Ask is unavailable right now. Try the docs link."
	msgNotConsented  = "Confirm the notice before asking."
	msgRelayDisabled = "Ask is not available for this hive. Try the docs link."
	msgBusyPrefix    = "Busy, try again in "
	msgBusySuffix    = " seconds."
)

var msgInvalid = "Ask a question of up to " + strconv.Itoa(MaxQuestionRunes) + " characters."

// ConsentChecker answers whether a person holds a current consent.
type ConsentChecker interface {
	Has(person string) bool
}

// Options configure a Service.
type Options struct {
	Upstream         Upstream
	Limits           Limits
	SendSectionLabel bool
	Consent          ConsentChecker
	Logger           *slog.Logger
	// DocsURL is the fallback docs link; "" means DocsHomeURL.
	DocsURL string
	// UpstreamTimeout bounds one vendor call; 0 means DefaultUpstreamTimeout.
	UpstreamTimeout time.Duration
	BreakerFailures int
	BreakerCooldown time.Duration
	ConversationTTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Request is one question. Person is the stem other audit lines use; HiveID is
// set for relayed requests only. Section is an id from the closed vocabulary;
// anything else sends no label. Conversation is the handle from a previous
// Result.
type Request struct {
	Person       string `json:"-"`
	HiveID       string `json:"-"`
	Question     string `json:"question"`
	Section      string `json:"section,omitempty"`
	Conversation string `json:"conversation,omitempty"`
}

// Result is what the page receives. Answer is plain text; Sources passed the
// allowlist. AIGenerated is always true so the label is never optional.
type Result struct {
	Outcome           Outcome  `json:"outcome"`
	Answer            string   `json:"answer,omitempty"`
	Sources           []Source `json:"sources,omitempty"`
	Uncertain         bool     `json:"uncertain,omitempty"`
	Conversation      string   `json:"conversation,omitempty"`
	RetryAfterSeconds int      `json:"retry_after_seconds,omitempty"`
	Message           string   `json:"message,omitempty"`
	AIGenerated       bool     `json:"ai_generated"`
	DocsURL           string   `json:"docs_url"`
}

// Stats is the operator view: counts by outcome and today's total against
// the daily budget. No per-person data.
type Stats struct {
	ByOutcome   map[Outcome]uint64 `json:"by_outcome"`
	Today       int                `json:"today"`
	DailyBudget int                `json:"daily_budget"`
	BreakerOpen bool               `json:"breaker_open"`
}

// Service answers questions. It is safe for concurrent use.
type Service struct {
	opts    Options
	limiter *limiter
	breaker *breaker
	convs   *conversations
	now     func() time.Time
	logger  *slog.Logger

	mu           sync.Mutex
	counts       map[Outcome]uint64
	limitAudited map[string]time.Time
	dailyAudited time.Time
}

// NewService builds a Service. A nil Upstream is a programming error.
func NewService(o Options) (*Service, error) {
	if o.Upstream == nil {
		return nil, errors.New("ask: no upstream")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.DocsURL == "" {
		o.DocsURL = DocsHomeURL
	}
	if o.UpstreamTimeout <= 0 {
		o.UpstreamTimeout = DefaultUpstreamTimeout
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		opts:         o,
		limiter:      newLimiter(o.Limits),
		breaker:      newBreaker(o.BreakerFailures, o.BreakerCooldown),
		convs:        newConversations(o.ConversationTTL),
		now:          now,
		logger:       o.Logger,
		counts:       map[Outcome]uint64{},
		limitAudited: map[string]time.Time{},
	}, nil
}

// Ask answers a question from a person signed in to this deployment's own
// dashboard. A current consent is required.
func (s *Service) Ask(ctx context.Context, req Request) Result {
	return s.ask(ctx, req, true)
}

// Relay answers a question relayed by a hive over the hub (phase 4, #19183).
// The hive took the person's consent and applied its own per-person limits
// before sending, so neither is checked again here; the hive's hourly cap and
// the process-wide caps are. req.HiveID must be the authenticated hive; the
// conversation handle is bound to it, so another hive cannot continue it.
func (s *Service) Relay(ctx context.Context, req Request) Result {
	req.Person = ""
	if req.HiveID == "" {
		return s.result(OutcomeRelayDisabled, msgRelayDisabled)
	}
	return s.ask(ctx, req, false)
}

// RelayDisabledResult is the answer for a hive the relay does not serve.
func (s *Service) RelayDisabledResult() Result {
	return s.result(OutcomeRelayDisabled, msgRelayDisabled)
}

// ValidateQuestion trims the question and reports whether it is usable and
// free of credential shapes. It is exported so a relaying hive can refuse
// before anything leaves it.
func ValidateQuestion(q string) (string, Outcome) {
	q = strings.TrimSpace(q)
	if q == "" || !utf8.ValidString(q) || utf8.RuneCountInString(q) > MaxQuestionRunes {
		return "", OutcomeInvalidQuestion
	}
	if CredentialShaped(q) {
		return "", OutcomeRefusedSecretShape
	}
	return q, OutcomeOK
}

// CredentialShaped reports whether s holds anything the log scrubber treats
// as a credential, including the heuristic shapes FindSecret skips: refusing
// a question costs the person a rephrase, sending a token costs much more.
func CredentialShaped(s string) bool {
	if _, found := logscrub.FindSecret(s); found {
		return true
	}
	return logscrub.ScrubString(s) != s
}

func (s *Service) ask(ctx context.Context, req Request, checkConsent bool) Result {
	start := s.now()
	question, outcome := ValidateQuestion(req.Question)
	questionLen := utf8.RuneCountInString(strings.TrimSpace(req.Question))
	if outcome == OutcomeInvalidQuestion {
		return s.finish(req, s.result(OutcomeInvalidQuestion, msgInvalid), questionLen, start)
	}
	if checkConsent && (s.opts.Consent == nil || !s.opts.Consent.Has(req.Person)) {
		return s.finish(req, s.result(OutcomeNotConsented, msgNotConsented), questionLen, start)
	}
	if outcome == OutcomeRefusedSecretShape {
		return s.finish(req, s.result(OutcomeRefusedSecretShape, msgRefused), questionLen, start)
	}
	if ok, wait := s.breaker.allow(start); !ok {
		r := s.result(OutcomeBreakerOpen, msgUnavailable)
		r.RetryAfterSeconds = seconds(wait)
		return s.finish(req, r, questionLen, start)
	}
	decision := s.limiter.reserve(start, req.Person, req.HiveID)
	if !decision.ok {
		s.breaker.cancelTrial()
		if decision.dailySpent {
			s.auditDailySpent(start)
		}
		r := s.result(OutcomeRateLimited, busyMessage(decision.retryAfter))
		r.RetryAfterSeconds = seconds(decision.retryAfter)
		return s.finish(req, r, questionLen, start)
	}
	defer s.limiter.release(req.Person)

	query := question
	if s.opts.SendSectionLabel {
		if label, ok := SectionLabel(req.Section); ok {
			query = sectionLabelPrefix + label + ".\n\n" + question
		}
	}
	thread := s.convs.thread(req.Conversation, req.Person, req.HiveID, start)
	callCtx, cancel := context.WithTimeout(ctx, s.opts.UpstreamTimeout)
	answer, err := s.opts.Upstream.Chat(callCtx, thread, query)
	cancel()
	if err != nil {
		var status *UpstreamStatusError
		if errors.As(err, &status) && status.RateLimited() {
			s.breaker.cancelTrial()
			s.limiter.backoff(s.now(), status.RetryAfter)
			r := s.result(OutcomeRateLimited, busyMessage(status.RetryAfter))
			r.RetryAfterSeconds = seconds(status.RetryAfter)
			return s.finish(req, r, questionLen, start)
		}
		if s.breaker.failure(s.now()) == breakerOpened {
			s.logger.Warn("audit: ask breaker opened", "cooldown_s", seconds(s.breaker.cooldown))
		}
		return s.finish(req, s.result(OutcomeUpstreamError, msgUnavailable), questionLen, start)
	}
	if s.breaker.success() == breakerClosed {
		s.logger.Info("audit: ask breaker closed")
	}
	r := s.result(OutcomeOK, "")
	r.Conversation = s.convs.remember(req.Conversation, req.Person, req.HiveID, answer.ThreadID, s.now())
	r.Sources = FilterSources(answer.Sources)
	r.Answer = SanitizeAnswer(answer.Answer)
	r.Uncertain = answer.Uncertain
	if len(r.Sources) == 0 || r.Answer == "" {
		// An uncited answer is not shown as an answer.
		r.Outcome, r.Answer, r.Sources, r.Message, r.Uncertain = OutcomeNoCitedAnswer, "", nil, msgNoCitedAnswer, false
	}
	return s.finish(req, r, questionLen, start)
}

func (s *Service) result(o Outcome, msg string) Result {
	return Result{Outcome: o, Message: msg, AIGenerated: true, DocsURL: s.opts.DocsURL}
}

// finish counts the outcome and writes the audit line: outcome and counts
// only, never the question, the answer, the sources or the thread.
func (s *Service) finish(req Request, r Result, questionLen int, start time.Time) Result {
	s.mu.Lock()
	s.counts[r.Outcome]++
	skip := false
	if r.Outcome == OutcomeRateLimited {
		// One audit line per person per window, not one per request.
		key := req.HiveID + "/" + req.Person
		if last, ok := s.limitAudited[key]; ok && start.Sub(last) < minuteWindow {
			skip = true
		} else {
			if len(s.limitAudited) >= maxTrackedKeys {
				s.limitAudited = map[string]time.Time{}
			}
			s.limitAudited[key] = start
		}
	}
	s.mu.Unlock()
	if !skip {
		attrs := []any{"outcome", string(r.Outcome), "question_len", questionLen,
			"sources_kept", len(r.Sources), "latency_ms", s.now().Sub(start).Milliseconds()}
		if req.Person != "" {
			attrs = append(attrs, "person", req.Person)
		}
		if req.HiveID != "" {
			attrs = append(attrs, "hive", req.HiveID)
		}
		s.logger.Info("audit: ask request", attrs...)
	}
	return r
}

func (s *Service) auditDailySpent(now time.Time) {
	s.mu.Lock()
	day := now.Truncate(dayWindow)
	already := s.dailyAudited.Equal(day)
	s.dailyAudited = day
	s.mu.Unlock()
	if !already {
		_, budget := s.limiter.dailyUsed(now)
		s.logger.Warn("audit: ask daily budget spent", "daily_budget", budget)
	}
}

// Stats returns the operator counters.
func (s *Service) Stats() Stats {
	s.mu.Lock()
	by := make(map[Outcome]uint64, len(s.counts))
	for k, v := range s.counts {
		by[k] = v
	}
	s.mu.Unlock()
	today, budget := s.limiter.dailyUsed(s.now())
	return Stats{ByOutcome: by, Today: today, DailyBudget: budget, BreakerOpen: s.breaker.isOpen()}
}

func busyMessage(d time.Duration) string {
	return msgBusyPrefix + strconv.Itoa(seconds(d)) + msgBusySuffix
}

func seconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Seconds()))
}

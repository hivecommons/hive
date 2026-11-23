package askai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Hub relay (phase 4, #19183). A hosted hive holds no Kapa key: it takes the
// person's consent, refuses credential-shaped input and applies its own
// per-person limits, then forwards only {question, section, conversation} to
// the hub, authenticated with the per-hive heartbeat bearer it already has.
// The hub applies the hive's quota and the process-wide caps, calls the vendor
// and returns a filtered Result. The hive filters it again before showing it.
const (
	// RelayPath is the hub route a hive posts to.
	RelayPath = "/api/hub/ask/relay"
	// RelayHiveIDHeader names the hive the bearer is bound to.
	RelayHiveIDHeader = "X-Hive-ID"
	// maxRelayResponseBytes caps what a hive reads back from the hub.
	maxRelayResponseBytes = 64 << 10
)

// Asker is what a dashboard calls: a Service in direct mode, a RelayAsker in
// hub mode.
type Asker interface {
	Ask(ctx context.Context, req Request) Result
}

// knownOutcomes is the closed set a relayed Result may carry.
var knownOutcomes = map[Outcome]bool{
	OutcomeOK: true, OutcomeNoCitedAnswer: true, OutcomeRefusedSecretShape: true, OutcomeRateLimited: true,
	OutcomeBreakerOpen: true, OutcomeUpstreamError: true, OutcomeNotConsented: true, OutcomeInvalidQuestion: true,
	OutcomeRelayDisabled: true,
}

// RelayOptions configure a RelayAsker.
type RelayOptions struct {
	// HubURL is the hub base URL; HiveID the authenticated hive.
	HubURL string
	HiveID string
	// Bearer returns the per-hive heartbeat bearer at call time.
	Bearer  func() string
	Consent ConsentChecker
	// Limits are this hive's own limits (HivePerHour is unused here).
	Limits Limits
	HTTP   *http.Client
	Logger *slog.Logger
	// DocsURL is the fallback docs link; "" means DocsHomeURL.
	DocsURL string
	// Timeout bounds one hub call; 0 means DefaultUpstreamTimeout.
	Timeout time.Duration
	Now     func() time.Time
}

// RelayAsker answers through the hub relay. It never holds a vendor key and
// never falls back to one.
type RelayAsker struct {
	url     string
	hiveID  string
	bearer  func() string
	consent ConsentChecker
	limiter *limiter
	convs   *conversations
	http    *http.Client
	logger  *slog.Logger
	docsURL string
	timeout time.Duration
	now     func() time.Time
}

// NewRelayAsker builds a relay client. It needs a hub URL, a hive id and a
// bearer source.
func NewRelayAsker(o RelayOptions) (*RelayAsker, error) {
	base := strings.TrimRight(strings.TrimSpace(o.HubURL), "/")
	if base == "" || strings.TrimSpace(o.HiveID) == "" || o.Bearer == nil {
		return nil, errors.New("ask: hub relay is not configured")
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultUpstreamTimeout
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: o.Timeout}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.DocsURL == "" {
		o.DocsURL = DocsHomeURL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &RelayAsker{
		url: base + RelayPath, hiveID: strings.TrimSpace(o.HiveID), bearer: o.Bearer, consent: o.Consent,
		limiter: newLimiter(o.Limits), convs: newConversations(0), http: o.HTTP, logger: o.Logger,
		docsURL: o.DocsURL, timeout: o.Timeout, now: o.Now,
	}, nil
}

func (a *RelayAsker) result(o Outcome, msg string) Result {
	return Result{Outcome: o, Message: msg, AIGenerated: true, DocsURL: a.docsURL}
}

// Ask validates locally, so nothing a person must not send ever reaches the
// hub, then forwards.
func (a *RelayAsker) Ask(ctx context.Context, req Request) Result {
	start := a.now()
	questionLen := utf8.RuneCountInString(strings.TrimSpace(req.Question))
	r := a.ask(ctx, req, start)
	a.logger.Info("audit: ask request", "outcome", string(r.Outcome), "person", req.Person, "relay", true,
		"question_len", questionLen, "sources_kept", len(r.Sources), "latency_ms", a.now().Sub(start).Milliseconds())
	return r
}

func (a *RelayAsker) ask(ctx context.Context, req Request, start time.Time) Result {
	question, outcome := ValidateQuestion(req.Question)
	if outcome == OutcomeInvalidQuestion {
		return a.result(OutcomeInvalidQuestion, msgInvalid)
	}
	if a.consent == nil || !a.consent.Has(req.Person) {
		return a.result(OutcomeNotConsented, msgNotConsented)
	}
	if outcome == OutcomeRefusedSecretShape {
		return a.result(OutcomeRefusedSecretShape, msgRefused)
	}
	decision := a.limiter.reserve(start, req.Person, "")
	if !decision.ok {
		r := a.result(OutcomeRateLimited, busyMessage(decision.retryAfter))
		r.RetryAfterSeconds = seconds(decision.retryAfter)
		return r
	}
	defer a.limiter.release(req.Person)

	hubHandle := a.convs.thread(req.Conversation, req.Person, "", start)
	section := ""
	if _, ok := SectionLabel(req.Section); ok {
		section = req.Section
	}
	body, err := json.Marshal(Request{Question: question, Section: section, Conversation: hubHandle})
	if err != nil {
		return a.result(OutcomeUpstreamError, msgUnavailable)
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		return a.result(OutcomeUpstreamError, msgUnavailable)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.bearer())
	httpReq.Header.Set(RelayHiveIDHeader, a.hiveID)
	resp, err := a.http.Do(httpReq)
	if err != nil {
		return a.result(OutcomeUpstreamError, msgUnavailable)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Relay off on the hub, or this hive not served: never retry with a
		// key the hive does not hold.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRelayResponseBytes))
		return a.result(OutcomeRelayDisabled, msgRelayDisabled)
	case resp.StatusCode != http.StatusOK:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRelayResponseBytes))
		return a.result(OutcomeUpstreamError, msgUnavailable)
	}
	var got Result
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRelayResponseBytes)).Decode(&got); err != nil || !knownOutcomes[got.Outcome] {
		return a.result(OutcomeUpstreamError, msgUnavailable)
	}
	return a.reFilter(got, req, start)
}

// reFilter applies the allowlist and text rules again on the hive, keeps the
// hub's conversation handle server side behind a local one, and never lets the
// hub decide the docs link or the AI label.
func (a *RelayAsker) reFilter(got Result, req Request, now time.Time) Result {
	r := a.result(got.Outcome, cleanText(got.Message, MaxAnswerRunes, ""))
	if r.Message == "" {
		r.Message = msgUnavailable
	}
	r.RetryAfterSeconds = got.RetryAfterSeconds
	if got.Outcome != OutcomeOK {
		return r
	}
	in := make([]UpstreamSource, 0, len(got.Sources))
	for _, s := range got.Sources {
		in = append(in, UpstreamSource{URL: s.URL, Title: s.Title})
	}
	r.Message = ""
	r.Sources = FilterSources(in)
	r.Answer = SanitizeAnswer(got.Answer)
	r.Uncertain = got.Uncertain
	if handle := strings.TrimSpace(got.Conversation); handle != "" && len(handle) <= maxHandleLen {
		r.Conversation = a.convs.remember(req.Conversation, req.Person, "", handle, now)
	}
	if len(r.Sources) == 0 || r.Answer == "" {
		r.Outcome, r.Answer, r.Sources, r.Message, r.Uncertain = OutcomeNoCitedAnswer, "", nil, msgNoCitedAnswer, false
	}
	return r
}

// maxHandleLen bounds a hub conversation handle the hive will keep.
const maxHandleLen = 2 * conversationHandleBytes

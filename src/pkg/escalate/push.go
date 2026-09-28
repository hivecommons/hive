package escalate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const defaultHTTPTimeout = 10 * time.Second

// Provider payload limits. A payload over any of them is rejected with a 4xx,
// which no retry can fix, so every sink truncates to them before sending.
// Pushover and PagerDuty count characters; ntfy counts bytes.
const (
	// https://pushover.net/api#limits
	pushoverMaxTitle   = 250
	pushoverMaxMessage = 1024
	pushoverMaxURL     = 512
	// https://developer.pagerduty.com/docs/events-api-v2/trigger-events/
	pagerDutyMaxSummary = 1024
	// ntfy's default message-size-limit; beyond it a publish is turned into an
	// attachment or refused, depending on the server's attachment config.
	ntfyMaxBodyBytes = 4096
)

// ellipsis marks text shortened to fit a provider limit.
const ellipsis = "…"

type NtfySink struct {
	URL, Token string
	Client     *http.Client
}

func (s *NtfySink) Name() string { return "ntfy" }
func (s *NtfySink) Deliver(ctx context.Context, ev Event) error {
	// Scrub here as well as in Dispatch: the sinks are exported and can be
	// delivered to directly, so the guarantee must not depend on which door
	// the event came through.
	ev = scrubEvent(ev)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, strings.NewReader(ntfyMessage(ev.Body, ev.Link)))
	if err != nil {
		return redactURLError(err)
	}
	// HTTP header values are not UTF-8; ntfy decodes RFC 2047 encoded-words,
	// so a title carrying "—" or any other non-ASCII rune arrives intact.
	// ASCII-only titles pass through unchanged.
	req.Header.Set("Title", mime.QEncoding.Encode("utf-8", cleanHeader(ev.Title)))
	req.Header.Set("Priority", ntfyPriority(ev.Severity))
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	return doOK(client(s.Client), req)
}

// ntfyMessage joins body and link within ntfy's message size limit, cutting
// the body rather than the link so the notification still leads somewhere.
func ntfyMessage(body, link string) string {
	body, link = strings.TrimSpace(body), strings.TrimSpace(link)
	if link != "" {
		body = truncateBytes(body, ntfyMaxBodyBytes-len(link)-1)
	}
	return truncateBytes(strings.TrimSpace(body+"\n"+link), ntfyMaxBodyBytes)
}

func ntfyPriority(s Severity) string {
	if s == SeverityPage {
		return "high"
	}
	return "default"
}

type PushoverSink struct {
	AppToken, UserKey string
	URL               string
	Client            *http.Client
}

func (s *PushoverSink) Name() string { return "pushover" }
func (s *PushoverSink) Deliver(ctx context.Context, ev Event) error {
	ev = scrubEvent(ev)
	endpoint := s.URL
	if endpoint == "" {
		endpoint = "https://api.pushover.net/1/messages.json"
	}
	v := url.Values{
		"token":   {s.AppToken},
		"user":    {s.UserKey},
		"title":   {truncateRunes(ev.Title, pushoverMaxTitle)},
		"message": {truncateRunes(strings.TrimSpace(ev.Body), pushoverMaxMessage)},
	}
	// A cut URL points nowhere, so an oversize link is left off rather than
	// truncated; the notification still arrives.
	if ev.Link != "" && utf8.RuneCountInString(ev.Link) <= pushoverMaxURL {
		v.Set("url", ev.Link)
	}
	if ev.Severity == SeverityPage {
		v.Set("priority", "1")
	} else {
		v.Set("priority", "0")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(v.Encode()))
	if err != nil {
		return redactURLError(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doOK(client(s.Client), req)
}

type PagerDutySink struct {
	RoutingKey string
	URL        string
	Client     *http.Client
}

func (s *PagerDutySink) Name() string { return "pagerduty" }
func (s *PagerDutySink) Deliver(ctx context.Context, ev Event) error {
	ev = scrubEvent(ev)
	endpoint := s.URL
	if endpoint == "" {
		endpoint = "https://events.pagerduty.com/v2/enqueue"
	}
	sev := "error"
	if ev.Severity == SeverityPage {
		sev = "critical"
	}
	payload := map[string]any{"routing_key": s.RoutingKey, "event_action": "trigger", "payload": map[string]any{"summary": truncateRunes(ev.Title, pagerDutyMaxSummary), "source": "hive", "severity": sev, "custom_details": map[string]string{"body": ev.Body, "link": ev.Link}}}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return redactURLError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doOK(client(s.Client), req)
}

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: defaultHTTPTimeout}
}

// statusError is a provider's non-2xx answer. retryAfter carries the
// provider's Retry-After hint, zero when it sent none.
type statusError struct {
	code       int
	retryAfter time.Duration
}

func (e *statusError) Error() string { return fmt.Sprintf("status %d", e.code) }

// retryable reports whether resending the same request can succeed: a
// throttle (429), a request timeout (408), or a server-side failure (5xx).
// Any other 4xx rejected the request itself and will reject it again.
func (e *statusError) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code == http.StatusRequestTimeout || e.code >= 500
}

func doOK(c *http.Client, req *http.Request) error {
	resp, err := c.Do(req)
	if err != nil {
		return redactURLError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return &statusError{code: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	return nil
}

// parseRetryAfter reads a Retry-After value in either of its RFC 9110 forms,
// delay-seconds or an HTTP-date. Unparseable or past values yield zero.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// redactURLError strips the request path and query from a *url.Error. An
// ntfy topic URL is the topic's only credential on a public server — anyone
// who knows it can read and publish — and the error text lands in the log
// and the escalation audit trail.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	target := "[redacted]"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		target = u.Scheme + "://" + u.Host
	}
	return fmt.Errorf("%s %s: %w", ue.Op, target, ue.Err)
}

func cleanHeader(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

// truncateRunes shortens s to at most max runes, marking the cut.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	if max <= 0 {
		return ""
	}
	n := 0
	for i := range s {
		if n == max-1 {
			return s[:i] + ellipsis
		}
		n++
	}
	return s
}

// truncateBytes shortens s to at most max bytes without splitting a rune,
// marking the cut.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max < len(ellipsis) {
		return ""
	}
	cut := max - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

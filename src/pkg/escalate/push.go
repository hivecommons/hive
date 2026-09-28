package escalate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const defaultHTTPTimeout = 10 * time.Second

// Provider payload limits. Sending past these makes the provider reject the
// whole delivery (Pushover/PagerDuty return 4xx; ntfy silently converts an
// oversize body into a file attachment instead of a notification), so the
// title/body/summary are truncated to fit before the request is sent.
const (
	ntfyTitleLimit        = 1024 // bytes; X-Title header
	ntfyBodyLimit         = 4096 // bytes; message body
	pushoverTitleLimit    = 250  // chars
	pushoverMessageLimit  = 1024 // chars
	pagerDutySummaryLimit = 1024 // chars
)

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
	body := truncateBytes(strings.TrimSpace(ev.Body+"\n"+ev.Link), ntfyBodyLimit)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", truncateBytes(cleanHeader(ev.Title), ntfyTitleLimit))
	req.Header.Set("Priority", ntfyPriority(ev.Severity))
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	return doOK(client(s.Client), req)
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
	v := url.Values{"token": {s.AppToken}, "user": {s.UserKey}, "title": {truncate(ev.Title, pushoverTitleLimit)}, "message": {truncate(strings.TrimSpace(ev.Body), pushoverMessageLimit)}, "url": {ev.Link}}
	if ev.Severity == SeverityPage {
		v.Set("priority", "1")
	} else {
		v.Set("priority", "0")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(v.Encode()))
	if err != nil {
		return err
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
	payload := map[string]any{"routing_key": s.RoutingKey, "event_action": "trigger", "payload": map[string]any{"summary": truncate(ev.Title, pagerDutySummaryLimit), "source": "hive", "severity": sev, "custom_details": map[string]string{"body": ev.Body, "link": ev.Link}}}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
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
func doOK(c *http.Client, req *http.Request) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
func cleanHeader(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

// truncate cuts s to at most limit runes, matching provider limits that are
// documented in characters (Pushover, PagerDuty).
func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}

// truncateBytes cuts s to at most limit bytes without splitting a multi-byte
// rune, matching provider limits that are documented in bytes (ntfy).
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	b := []byte(s)[:limit]
	for len(b) > 0 && !utf8.RuneStart(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return string(b)
}

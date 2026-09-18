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
)

const defaultHTTPTimeout = 10 * time.Second

type NtfySink struct {
	URL, Token string
	Client     *http.Client
}

func (s *NtfySink) Name() string { return "ntfy" }
func (s *NtfySink) Deliver(ctx context.Context, ev Event) error {
	body := strings.TrimSpace(ev.Body + "\n" + ev.Link)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", cleanHeader(ev.Title))
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
	endpoint := s.URL
	if endpoint == "" {
		endpoint = "https://api.pushover.net/1/messages.json"
	}
	v := url.Values{"token": {s.AppToken}, "user": {s.UserKey}, "title": {ev.Title}, "message": {strings.TrimSpace(ev.Body)}, "url": {ev.Link}}
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
	endpoint := s.URL
	if endpoint == "" {
		endpoint = "https://events.pagerduty.com/v2/enqueue"
	}
	sev := "error"
	if ev.Severity == SeverityPage {
		sev = "critical"
	}
	payload := map[string]any{"routing_key": s.RoutingKey, "event_action": "trigger", "payload": map[string]any{"summary": ev.Title, "source": "hive", "severity": sev, "custom_details": map[string]string{"body": ev.Body, "link": ev.Link}}}
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

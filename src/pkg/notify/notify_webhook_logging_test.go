package notify

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

type fixedRedirectTransport struct {
	status   int
	location string
}

func (t fixedRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	h := http.Header{}
	h.Set("Location", t.location)
	return &http.Response{StatusCode: t.status, Header: h, Body: http.NoBody, Request: r}, nil
}

func TestWebhookRedirectStaysOnOrigin(t *testing.T) {
	cases := []struct {
		name     string
		from     string
		location string
		keep     bool
	}{
		{"same host https", "https://hooks.example.com/secret", "/moved", true},
		{"same host upgrade", "http://hooks.example.com/secret", "https://hooks.example.com/moved", true},
		{"host case differs", "https://hooks.example.com/secret", "https://HOOKS.example.com/moved", true},
		{"https downgrade", "https://hooks.example.com/secret", "http://hooks.example.com/moved", false},
		{"other host", "https://hooks.example.com/secret", "https://attacker.example.net/moved", false},
		{"other port", "https://hooks.example.com/secret", "https://hooks.example.com:8443/moved", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, tc.from, http.NoBody)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			rt := webhookRedirectTransport{base: fixedRedirectTransport{status: http.StatusTemporaryRedirect, location: tc.location}}
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("round trip: %v", err)
			}
			if got := resp.Header.Get("Location") != ""; got != tc.keep {
				t.Errorf("Location kept = %t, want %t", got, tc.keep)
			}
		})
	}
}

type webhookErrorTransport struct{}

func (webhookErrorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("transport failed for " + r.URL.String())
}

func TestWebhookFailureLogging(t *testing.T) {
	const (
		webhookToken  = "secret-webhook-token"
		redirectToken = "secret-redirect-target"
		webhookPath   = "/" + webhookToken
		redirectPath  = "/" + redirectToken
		responseBody  = "secret-response-body"
	)
	cases := []struct {
		name         string
		status       int
		location     string
		targetStatus int
	}{
		{"ok", 200, "", 0},
		{"no content", 204, "", 0},
		{"last success status", 299, "", 0},
		{"multiple choices", 300, redirectPath, 0},
		{"moved permanently", 301, redirectPath, 0},
		{"found", 302, redirectPath, 0},
		{"see other", 303, redirectPath, 0},
		{"malformed found redirect", 302, "/%zz" + redirectPath, 0},
		{"temporary redirect", 307, redirectPath, 200},
		{"permanent redirect", 308, redirectPath, 204},
		{"malformed temporary redirect", 307, "/%zz" + redirectPath, 0},
		{"missing redirect host", 307, "http:" + redirectPath, 0},
		{"redirect port without hostname", 307, "http://:0" + redirectPath, 0},
		{"out of range redirect port", 308, "http://127.0.0.1:65536" + redirectPath, 0},
		{"unsupported redirect scheme", 308, "ftp://example.invalid" + redirectPath, 0},
		{"redirect without location", 307, "", 0},
		{"cross-host redirect", 307, "http://example.invalid" + redirectPath, 0},
		{"cross-host permanent redirect", 308, "http://localhost.invalid:8080" + redirectPath, 0},
		{"redirect target rejects", 307, redirectPath, 429},
		{"redirect limit", 308, webhookPath, 0},
		{"bad request", 400, "", 0},
		{"forbidden", 403, "", 0},
		{"rate limited", 429, "", 0},
		{"server error", 500, "", 0},
		{"transport error", 0, "", 0},
	}
	for _, provider := range []string{"slack", "discord"} {
		for _, tc := range cases {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				var followedTarget atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					status := tc.status
					if r.URL.Path == redirectPath {
						followedTarget.Store(true)
						status = tc.targetStatus
						if status == 0 {
							w.WriteHeader(http.StatusOK)
							return
						}
						payload, err := io.ReadAll(r.Body)
						if err != nil {
							t.Errorf("read redirected payload: %v", err)
						}
						if r.Method != http.MethodPost || !bytes.Contains(payload, []byte("message")) {
							t.Errorf("redirected request = %s %q, want POST with the alert payload", r.Method, payload)
						}
					} else if tc.location != "" {
						w.Header().Set("Location", tc.location)
					}
					if status == 0 {
						t.Error("unexpected request when the transport should fail")
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(status)
					if status >= 300 {
						_, _ = w.Write([]byte(responseBody))
					}
				}))
				defer server.Close()
				webhook := server.URL + webhookPath
				var logs bytes.Buffer
				n := New(config.NotificationsConfig{
					Slack:   &config.SlackConfig{Webhook: webhook},
					Discord: &config.DiscordConfig{Webhook: webhook},
				}, slog.New(slog.NewTextHandler(&logs, nil)))
				if tc.status == 0 {
					n.client.Transport = webhookErrorTransport{}
				}
				if provider == "slack" {
					n.sendSlack("title", "message")
				} else {
					n.sendDiscordWebhook("title", "message")
				}
				if got, want := followedTarget.Load(), tc.targetStatus != 0; got != want {
					t.Errorf("redirect reached the target: got %t, want %t", got, want)
				}
				status := tc.status
				if tc.targetStatus != 0 {
					status = tc.targetStatus
				}
				output := logs.String()
				if status >= 200 && status < 300 {
					if output != "" {
						t.Fatalf("successful webhook logged a warning: %s", output)
					}
					return
				}
				if strings.Count(output, "level=WARN") != 1 || !strings.Contains(output, "provider="+provider) {
					t.Errorf("want one warning with provider %s, got %s", provider, output)
				}
				if status == 0 && strings.Contains(output, " status=") {
					t.Errorf("unexpected HTTP status for a transport failure: %s", output)
				}
				if status != 0 && !strings.Contains(output, fmt.Sprintf("status=%d", status)) {
					t.Errorf("missing HTTP status: %s", output)
				}
				for _, secret := range []string{server.URL, webhookToken, redirectToken, responseBody} {
					if strings.Contains(output, secret) {
						t.Errorf("log leaked webhook data: %s", output)
					}
				}
			})
		}
	}
}

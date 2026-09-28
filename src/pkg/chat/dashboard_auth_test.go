package chat

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
)

// newAuthedDashboard stands up a real dashboard.Server (full middleware chain
// via Handler()) holding the shared token, on either deployment shape, and a
// chat.Service pointed at it with the same token — the wiring bootDashboard
// and the chat bootstrap in cmd/hive produce.
func newAuthedDashboard(t *testing.T, hubProxied bool) (*Service, *httptest.Server) {
	t.Helper()
	const token = "shared-secret-token"
	srv := dashboard.NewServerWithAuth(0, token, discardLogger())
	srv.RegisterAPI(&dashboard.Dependencies{
		Config: &config.Config{Dashboard: config.DashboardConfig{
			AuthToken:       token,
			AuthorizedUsers: []string{"owneruser"},
			HubProxied:      hubProxied,
		}},
		Logger: discardLogger(),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	s := NewService(&recordingBackend{}, Config{
		DashboardURL:   ts.URL,
		DashboardToken: token,
		AllowedUsers:   []string{"uid:owner"},
	}, discardLogger())
	s.client = ts.Client()
	return s, ts
}

// TestDashboardCalls_AuthenticateOnDirectRouteSpoke is the #9134 regression: a
// standalone spoke with an authorized_users allowlist (HubProxied=false) has
// the Authorization: Bearer path disabled, so the chat service must present
// the token as X-Hive-Internal or every command and the SSE stream answers 401.
func TestDashboardCalls_AuthenticateOnDirectRouteSpoke(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hubProxied bool
	}{
		{name: "direct-route", hubProxied: false},
		{name: "hub-proxied", hubProxied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newAuthedDashboard(t, tc.hubProxied)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			body, err := s.dashboardGet(ctx, "/api/status")
			if err != nil {
				t.Fatalf("GET /api/status: %v", err)
			}
			if !strings.Contains(string(body), "initializing") {
				t.Fatalf("GET /api/status body = %q, want the status payload", body)
			}

			if err := s.dashboardPost(ctx, "/api/presence", []byte(`{"engaged":true}`)); err != nil {
				t.Fatalf("POST /api/presence: %v", err)
			}

			sseCtx, sseCancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer sseCancel()
			connected, err := s.consumeSSE(sseCtx)
			if !connected {
				t.Fatalf("SSE /api/events never connected: %v", err)
			}
		})
	}
}

// TestDashboardCalls_WrongTokenStillRejected guards the invariant the fix must
// not weaken: the internal header authenticates only by possession of the real
// token, so a chat service holding the wrong one is still refused.
func TestDashboardCalls_WrongTokenStillRejected(t *testing.T) {
	s, _ := newAuthedDashboard(t, false)
	s.dashboardToken = "not-the-token"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := s.dashboardGet(ctx, "/api/status"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("GET with wrong token: err = %v, want HTTP 401", err)
	}
	if connected, err := s.consumeSSE(ctx); connected || err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("SSE with wrong token: connected=%v err=%v, want 401", connected, err)
	}
}

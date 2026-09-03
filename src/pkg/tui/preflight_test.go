package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/tui/client"
)

// preflightClient builds the model's own client against url, so these tests
// exercise the same construction path run() does rather than a hand-built
// client that could diverge from it.
func preflightClient(t *testing.T, url string) *client.Client {
	t.Helper()
	pinDashboard(t, url)
	return newModel().api
}

// TestPreflightRefusesOnUnauthorized is the behaviour this whole path exists
// for: an operator with no usable credential gets told so, in the shell, with
// their scrollback intact — instead of an alt-screen full of panes that will
// read "waiting for data" until they give up and press q.
func TestPreflightRefusesOnUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	err := preflight(context.Background(), preflightClient(t, server.URL))
	if err == nil {
		t.Fatal("preflight() = nil on 401, want a refusal")
	}

	// Both credentials must be named. Which one this hive wants depends on how
	// it was deployed, and an operator sent to fix only the token would keep
	// regenerating a secret a direct-route spoke will never accept.
	for _, want := range []string{client.TokenEnv, client.CookieEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("preflight() error does not name %s:\n%s", want, err)
		}
	}
}

// TestPreflightAllowsForbidden pins that a working-but-narrow credential still
// opens the TUI.
//
// A 403 is a real session belonging to a viewer rather than the owner. Most of
// the screen works for them — only the owner-gated reads fail, and those are
// already handled per-pane — so refusing here would lock a whole class of
// authorized users out of a dashboard they are entitled to see.
func TestPreflightAllowsForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"owner access required"}`, http.StatusForbidden)
	}))
	defer server.Close()

	if err := preflight(context.Background(), preflightClient(t, server.URL)); err != nil {
		t.Fatalf("preflight() = %v on 403, want nil", err)
	}
}

// TestPreflightAllowsUnreachableDashboard pins that a hive which is DOWN still
// opens the TUI.
//
// This is the case an operator is most likely to be opening the TUI to look
// into, and the poll loop is already built to fill the panes when the hive
// comes back. Refusing to start here would make the tool unavailable during
// precisely the incident it exists for.
func TestPreflightAllowsUnreachableDashboard(t *testing.T) {
	if err := preflight(context.Background(), preflightClient(t, closedDashboard)); err != nil {
		t.Fatalf("preflight() = %v against a closed dashboard, want nil", err)
	}
}

// TestPreflightAllowsServerError covers the other non-401 failure: the proxy is
// up but the Go API behind it is not. Transient by nature, and the panes
// recover on their own once it clears.
func TestPreflightAllowsServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Go API unavailable"}`, http.StatusBadGateway)
	}))
	defer server.Close()

	if err := preflight(context.Background(), preflightClient(t, server.URL)); err != nil {
		t.Fatalf("preflight() = %v on 502, want nil", err)
	}
}

// TestPreflightAllowsHealthyDashboard is the ordinary path — credentials
// accepted, nothing in the way of the frame.
func TestPreflightAllowsHealthyDashboard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"mode":"BUSY"}`))
	}))
	defer server.Close()

	if err := preflight(context.Background(), preflightClient(t, server.URL)); err != nil {
		t.Fatalf("preflight() = %v, want nil", err)
	}
}

// TestPreflightSendsConfiguredCookie is the end-to-end tie between the env var
// an operator sets and the header the hive sees.
//
// The unit tests in pkg/tui/client prove authorize() emits the header; this
// proves the value actually travels from HIVE_DASHBOARD_COOKIE through
// client.New() and the model to the wire, which is the part an operator's
// working session depends on and which no client-package test can see.
func TestPreflightSendsConfiguredCookie(t *testing.T) {
	gotCookie := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		_, _ = w.Write([]byte(`{"mode":"BUSY"}`))
	}))
	defer server.Close()

	t.Setenv(client.CookieEnv, "hive_session=from-env")
	if err := preflight(context.Background(), preflightClient(t, server.URL)); err != nil {
		t.Fatalf("preflight() = %v, want nil", err)
	}

	if gotCookie != "hive_session=from-env" {
		t.Errorf("Cookie = %q, want %q", gotCookie, "hive_session=from-env")
	}
}

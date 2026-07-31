package hub

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ============================================================
// auth_audit.go
// ============================================================

func newAuditClient() *http.Client {
	return &http.Client{
		Timeout: authAuditProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestProbeWideOpen(t *testing.T) {
	// 200 => wide open.
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer open.Close()
	_, wideOpen, reachable := probeWideOpen(context.Background(), newAuditClient(), open.URL)
	if !wideOpen || !reachable {
		t.Errorf("200 should be wideOpen+reachable, got %v %v", wideOpen, reachable)
	}

	// 302 => protected (reachable, not open).
	prot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer prot.Close()
	_, wideOpen, reachable = probeWideOpen(context.Background(), newAuditClient(), prot.URL)
	if wideOpen || !reachable {
		t.Errorf("302 should be protected+reachable, got %v %v", wideOpen, reachable)
	}

	// Unreachable (closed port).
	_, wideOpen, reachable = probeWideOpen(context.Background(), newAuditClient(), "http://127.0.0.1:1")
	if wideOpen || reachable {
		t.Errorf("closed port should be unreachable, got %v %v", wideOpen, reachable)
	}

	// Bad URL -> request build error.
	_, wideOpen, reachable = probeWideOpen(context.Background(), newAuditClient(), "http://\x7f\x00")
	if wideOpen || reachable {
		t.Errorf("bad URL should be unreachable, got %v %v", wideOpen, reachable)
	}
}

func TestRunAuthAuditNoOpenSpokes(t *testing.T) {
	prot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer prot.Close()

	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{
		{ID: "protected", DashboardURL: prot.URL},
		{ID: "no-url", DashboardURL: ""},                        // skipped
		{ID: "local", DashboardURL: "http://localhost:8080"},    // skipped
		{ID: "unreachable", DashboardURL: "http://127.0.0.1:1"}, // counted unreachable
	}
	// Should not panic; no open spokes -> the "no wide-open" path.
	s.runAuthAudit(context.Background(), newAuditClient())
}

func TestRunAuthAuditDetectsOpen(t *testing.T) {
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer open.Close()

	s := &HubServer{logger: slog.Default()}
	s.registry.Hives = []RegistryEntry{{ID: "wide-open", DashboardURL: open.URL}}
	// No ntfy configured -> pushAuthAuditAlert returns early; exercises the
	// wide-open branch and the alert no-op path.
	t.Setenv("HIVE_NTFY_SERVER", "")
	t.Setenv("HIVE_NTFY_TOPIC", "")
	s.runAuthAudit(context.Background(), newAuditClient())
}

func TestPushAuthAuditAlertNoConfig(t *testing.T) {
	t.Setenv("HIVE_NTFY_SERVER", "")
	t.Setenv("HIVE_NTFY_TOPIC", "")
	s := &HubServer{logger: slog.Default()}
	s.pushAuthAuditAlert("test message") // no-op, no panic
}

func TestPushAuthAuditAlertSends(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Title")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("HIVE_NTFY_SERVER", srv.URL)
	t.Setenv("HIVE_NTFY_TOPIC", "hive-alerts")
	s := &HubServer{logger: slog.Default()}
	s.pushAuthAuditAlert("wide open detected")

	select {
	case title := <-got:
		if title == "" {
			t.Error("expected Title header on ntfy request")
		}
	case <-time.After(2 * time.Second):
		t.Error("ntfy request never arrived")
	}
}

func TestPushAuthAuditAlertConnError(t *testing.T) {
	t.Setenv("HIVE_NTFY_SERVER", "http://127.0.0.1:1")
	t.Setenv("HIVE_NTFY_TOPIC", "hive-alerts")
	s := &HubServer{logger: slog.Default()}
	s.pushAuthAuditAlert("wide open") // client.Do fails -> logged, no panic
}

func TestStartAuthAuditCancels(t *testing.T) {
	// Cancel immediately: exercises the goroutine setup + ctx.Done() return.
	s := &HubServer{logger: slog.Default()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		s.StartAuthAudit(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("StartAuthAudit did not return after cancel")
	}
}

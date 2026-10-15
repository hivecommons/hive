package dashboard

import (
	"context"
	"testing"
)

func TestDashboardAdminMCPReadErrorKeepsHiveReason(t *testing.T) {
	// NewServerWithAuth leaves /api/logs/read to RegisterAPI; wire the log
	// routes the same way api_logs_test.go does so the read reaches the handler.
	s := logTestServer(t, t.TempDir())
	p := dashboardAdminMCPProvider{server: s, user: "owner", role: "owner"}
	_, err := p.Read(context.Background(), "log_read", map[string]any{
		"since": "2026-01-02T00:00:00Z",
		"until": "2026-01-01T00:00:00Z",
	})
	if err == nil {
		t.Fatal("expected refused read to fail")
	}
	// Same text the standalone client's APIError gives for this refusal.
	if want := "Hive API returned HTTP 400: until must not be before since"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestAdminMCPRefusalReason(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":"bad arg","ok":false}`: "bad arg",
		" plain text ":                   "plain text",
		"":                               "",
	} {
		if got := adminMCPRefusalReason([]byte(body)); got != want {
			t.Errorf("adminMCPRefusalReason(%q) = %q, want %q", body, got, want)
		}
	}
}

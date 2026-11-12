package kubejob

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJobKickAcceptanceFollowsCreate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		createErr int
		want      bool
	}{
		{"accepted", 0, true},
		{"rejected", http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.createErr = tc.createErr
			api.statuses = []string{statusFailed}
			srv := httptest.NewServer(api.handler(t))
			defer srv.Close()
			spec := specFor("/data/agents/sandbox/deps-123")
			calls := 0
			spec.OnStarted = func() { calls++ }
			_, _ = newLauncher(srv, Options{}).Run(context.Background(), spec)
			if (calls == 1) != tc.want || calls > 1 {
				t.Fatalf("acceptance callbacks = %d, want accepted=%v", calls, tc.want)
			}
		})
	}
}

package automerge

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

func TestClearUnauthorizedReadOnlyLaneSkipsLabelRemoval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readOnly bool
	}{
		{"read-only lane", true},
		{"write-capable lane", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deletes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/issues/7/labels/"+unauthorizedLabel) {
					deletes.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()
			c := newContributorLaneEngine(srv.URL, ContributorLanePolicy{}, nil, Options{})
			var logs bytes.Buffer
			c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
			pr := &gh.PullRequest{Labels: []*gh.Label{{Name: gh.Ptr(unauthorizedLabel)}}}
			ctx := context.Background()
			if tc.readOnly {
				ctx = hgithub.WithIntegrationReadOnly(ctx)
			}
			for range 3 {
				c.clearUnauthorized(ctx, "acme/widget", "acme", "widget", pr, 7, "approve-workflow-runs")
			}
			if tc.readOnly {
				if got := deletes.Load(); got != 0 {
					t.Fatalf("deletes = %d, want 0", got)
				}
			} else if deletes.Load() < 1 {
				t.Fatal("write-capable lane did not remove the label")
			}
			if strings.Contains(logs.String(), "level=WARN") {
				t.Fatalf("unexpected warning: %s", logs.String())
			}
		})
	}
}

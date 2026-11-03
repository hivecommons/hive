package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestDependencyBotWritebackRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/org/repo/issues/42" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"number":42,"user":{"login":"renovate[bot]"},"labels":[],"pull_request":{}}`))
	}))
	defer srv.Close()
	client := github.NewClientForTest(srv.URL, "org", []string{"repo"}, slog.Default())
	gate := writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 1 }}, "")
	client.SetWritebackGate(gate)
	for _, kind := range []writeback.Kind{writeback.KindNeedsDecisionComment, writeback.KindStageLabel} {
		if depBotWritebackAllowed(context.Background(), client, "org/repo", 42, kind, "needs-human", slog.Default()) {
			t.Fatalf("unsolicited %s on a dependency-bot PR was allowed", kind)
		}
	}
	if suppressed := gate.Recent(); len(suppressed) != 2 || !suppressed[0].IsPR || suppressed[0].Number != 42 {
		t.Fatalf("suppressed=%+v", suppressed)
	}
}

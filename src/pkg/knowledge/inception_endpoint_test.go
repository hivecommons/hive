package knowledge

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInceptionOptionalProjectEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nilAPI     bool
		layer      LayerType
		status     int
		wantWrites int32
		wantWarn   bool
	}{
		{name: "no API", nilAPI: true},
		{name: "no endpoints"},
		{name: "org endpoint only", layer: LayerOrg},
		{name: "project endpoint", layer: LayerProject, status: http.StatusOK, wantWrites: 2},
		{name: "project write fails", layer: LayerProject, status: http.StatusInternalServerError, wantWrites: 2, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			var writes atomic.Int32
			var api *KnowledgeAPI
			if !tc.nilAPI {
				api = &KnowledgeAPI{logger: logger}
				if tc.layer != "" {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodPost || r.URL.Path != "/api/ingest" {
							t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						}
						writes.Add(1)
						status := tc.status
						if status == 0 {
							status = http.StatusOK
						}
						w.WriteHeader(status)
					}))
					defer server.Close()
					api.layers = []layerClient{{layerType: tc.layer, client: NewClient(server.URL, logger)}}
				}
			}

			dir := t.TempDir()
			engine := NewInceptionEngine(dir, api, logger)
			if _, err := engine.Start("A project"); err != nil {
				t.Fatal(err)
			}
			if err := engine.SetQuestions([]Question{{ID: "scope", Text: "What should it do?"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.SubmitAnswers(map[string]string{"scope": "Meet acceptance criteria"}); err != nil {
				t.Fatal(err)
			}
			fact := IdeationFact{Title: "Acceptance", Body: "The project meets its criteria", Type: FactAcceptance}
			if err := engine.RecordFacts(context.Background(), []IdeationFact{fact}); err != nil {
				t.Fatal(err)
			}
			if state := engine.GetState(); state.Phase != PhaseScaffold || len(state.FactSlugs) != 2 {
				t.Fatalf("unexpected inception state: %+v", state)
			}
			if got := writes.Load(); got != tc.wantWrites {
				t.Errorf("remote writes = %d, want %d", got, tc.wantWrites)
			}
			if got := strings.Contains(logs.String(), "failed to record ideation fact"); got != tc.wantWarn {
				t.Errorf("record warning = %v, want %v; logs: %s", got, tc.wantWarn, &logs)
			}
			if !tc.wantWarn && logs.Len() != 0 {
				t.Errorf("unexpected warnings: %s", &logs)
			}
			if api != nil {
				assertInceptionVaultFact(t, api, "inception-wiki", fact)
			}

			// The local facts must remain available to agents after a restart,
			// including when the remote endpoint is absent or its write failed.
			restartedAPI := &KnowledgeAPI{logger: logger}
			NewInceptionEngine(dir, restartedAPI, logger)
			assertInceptionVaultFact(t, restartedAPI, engine.GetState().WikiName, fact)
		})
	}
}

func assertInceptionVaultFact(t *testing.T, api *KnowledgeAPI, name string, want IdeationFact) {
	t.Helper()
	facts := api.VaultFacts(name)
	if len(facts) != 1 {
		t.Fatalf("local wiki facts = %d, want 1", len(facts))
	}
	if got := facts[0]; got.Title != want.Title || got.Type != want.Type || !strings.Contains(got.Body, want.Body) {
		t.Errorf("local wiki fact = %+v, want %+v", got, want)
	}
}

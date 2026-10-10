package dashboard

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

type fakePublishRuntime struct {
	mu       sync.Mutex
	status   connector.PublishStatus
	triggers int
}

func (f *fakePublishRuntime) Status() connector.PublishStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakePublishRuntime) Trigger() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggers++
}

func decodePublishView(t *testing.T, s *Server, path string) *knowledgePublishJSON {
	t.Helper()
	rec := doGet(s, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var body struct {
		Publish *knowledgePublishJSON `json:"publish"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return body.Publish
}

func TestKnowledgePublishStatusPayload(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		configured bool
		runtime    *fakePublishRuntime
		check      func(t *testing.T, v *knowledgePublishJSON)
	}{
		{
			name: "not configured",
			check: func(t *testing.T, v *knowledgePublishJSON) {
				if v != nil {
					t.Fatalf("publish = %+v, want null", v)
				}
			},
		},
		{
			name:       "configured but not running",
			configured: true,
			check: func(t *testing.T, v *knowledgePublishJSON) {
				if v == nil || v.Active || v.Connector != "team-docs" || v.Root != "Hive/Knowledge" || len(v.Layers) != 1 || v.Layers[0] != "org" || !v.DryRun {
					t.Fatalf("publish = %+v", v)
				}
			},
		},
		{
			name:       "running mirror",
			configured: true,
			runtime: &fakePublishRuntime{status: connector.PublishStatus{
				Connector: "team-docs", Root: "Hive/Knowledge", DryRun: true, LastRun: at, Pages: 3, LastError: "boom",
				LastReport: &connector.PublishReport{
					Created: []string{"org/a"}, Updated: []string{"org/b", "org/c"}, Deprecated: []string{"org/d"},
					Unchanged: 4, Collisions: []string{"org/e (page hive-org-e owned by org/f)"},
				},
			}},
			check: func(t *testing.T, v *knowledgePublishJSON) {
				if v == nil || !v.Active || v.Running || v.Pages != 3 || v.LastError != "boom" || v.LastRun != "2026-10-09T08:00:00Z" || v.LastSuccess != "" {
					t.Fatalf("publish = %+v", v)
				}
				if v.PagesCreated != 1 || v.PagesUpdated != 2 || v.PagesDeprecated != 1 || v.PagesUnchanged != 4 || len(v.Collisions) != 1 {
					t.Fatalf("publish counts = %+v", v)
				}
				if len(v.Layers) != 1 || v.Layers[0] != "org" {
					t.Fatalf("layers = %v", v.Layers)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, deps := apiServer(t)
			if tt.configured {
				deps.Config.Knowledge.Publish = config.KnowledgePublish{Connector: "team-docs", Layers: []string{"org"}, Root: "Hive/Knowledge", DryRun: true}
			}
			if tt.runtime != nil {
				deps.KnowledgePublish = tt.runtime
			}
			for _, path := range []string{"/api/knowledge/connectors", "/api/config/knowledge/connectors/status"} {
				t.Run(path, func(t *testing.T) { tt.check(t, decodePublishView(t, s, path)) })
			}
		})
	}
}

func TestKnowledgePublishSync(t *testing.T) {
	tests := []struct {
		name         string
		runtime      *fakePublishRuntime
		owner        bool
		wantCode     int
		wantTriggers int
	}{
		{name: "non-owner is forbidden", runtime: &fakePublishRuntime{}, wantCode: http.StatusForbidden},
		{name: "no mirror", owner: true, wantCode: http.StatusServiceUnavailable},
		{name: "already running", owner: true, runtime: &fakePublishRuntime{status: connector.PublishStatus{Running: true}}, wantCode: http.StatusConflict},
		{name: "triggers a batch", owner: true, runtime: &fakePublishRuntime{status: connector.PublishStatus{Connector: "team-docs"}}, wantCode: http.StatusAccepted, wantTriggers: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, deps := apiServer(t)
			if tt.runtime != nil {
				deps.KnowledgePublish = tt.runtime
			}
			post := doPost
			if !tt.owner {
				post = doPostNoOwner
			}
			rec := post(s, "/api/knowledge/publish/sync", nil)
			if rec.Code != tt.wantCode {
				t.Fatalf("POST = %d, want %d: %s", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.runtime != nil && tt.runtime.triggers != tt.wantTriggers {
				t.Fatalf("triggers = %d, want %d", tt.runtime.triggers, tt.wantTriggers)
			}
			if tt.wantCode == http.StatusAccepted {
				var v knowledgePublishJSON
				if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || !v.Active || v.Connector != "team-docs" {
					t.Fatalf("response = %+v, %v", v, err)
				}
			}
		})
	}
}

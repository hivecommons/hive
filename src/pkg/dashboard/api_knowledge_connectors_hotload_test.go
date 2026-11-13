package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// hotloadTestTimeout bounds every wait in the hot-load tests;
// hotloadPollInterval is how often they re-check.
const (
	hotloadTestTimeout  = 10 * time.Second
	hotloadPollInterval = 10 * time.Millisecond
)

const kcDocsEnabled = `{"connectors":[{"name":"docs","type":"document","enabled":true,"layer":"project","scope":{"url":"https://example.org/a"}}]}`
const kcDocsDisabled = `{"connectors":[{"name":"docs","type":"document","enabled":false,"layer":"project","scope":{"url":"https://example.org/a"}}]}`

// hotloadFactory is a KnowledgeConnectorFactory that builds a real
// connector.Syncer whose "document" type is conn, so no network is touched.
func hotloadFactory(t *testing.T, conn connector.Connector) (KnowledgeConnectorFactory, *int) {
	t.Helper()
	reg := connector.NewRegistry()
	if err := reg.Register("document", func(connector.ConnectorConfig, connector.Deps) (connector.Connector, error) {
		return conn, nil
	}); err != nil {
		t.Fatal(err)
	}
	vault := t.TempDir()
	calls := 0
	return func(entries []config.KnowledgeConnector) (KnowledgeConnectorRuntime, error) {
		calls++
		cfgs := make([]connector.ConnectorConfig, 0, len(entries))
		for _, e := range entries {
			rc, err := knowledgeConnectorRuntimeConfig(e)
			if err != nil {
				return nil, err
			}
			cfgs = append(cfgs, rc)
		}
		s, err := connector.NewSyncer(cfgs, connector.SyncerOptions{
			Registry: reg,
			VaultDir: func(string) (string, error) { return vault, nil },
		})
		if err != nil {
			return nil, err
		}
		return s, nil
	}, &calls
}

func putConnectors(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	rec := doPutRaw(s, "/api/config/knowledge/connectors", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %q", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitSynced(t *testing.T, rt KnowledgeConnectorRuntime, name string) {
	t.Helper()
	deadline := time.Now().Add(hotloadTestTimeout)
	for {
		if st, ok := knowledgeConnectorStatus(rt, name); ok && !st.Running && !st.LastSync.IsZero() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never finished syncing", name)
		}
		time.Sleep(hotloadPollInterval)
	}
}

// TestKnowledgeConnectors_HotLoad boots with zero connectors, adds one through
// the config API and expects the runtime to follow without a restart
// (#18091): Statuses lists it, Sync answers 202 (not 404), an edit is applied
// in place, and removing it makes it disappear again.
func TestKnowledgeConnectors_HotLoad(t *testing.T) {
	s, deps := apiServer(t)
	factory, calls := hotloadFactory(t, &blockingConn{})
	deps.NewKnowledgeConnectors = factory
	// Dependents (the Inbox in production) hear about every reload that
	// reached the runtime, with the entry count that was applied.
	var reloaded []int
	deps.KnowledgeConnectorsReloaded = func(entries []config.KnowledgeConnector, rt KnowledgeConnectorRuntime) {
		if rt == nil {
			t.Error("reload hook must see the runtime")
		}
		reloaded = append(reloaded, len(entries))
	}

	if rec := doPost(s, "/api/knowledge/connectors/docs/sync", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("sync before any connector = %d, want 404", rec.Code)
	}

	resp := putConnectors(t, s, kcDocsEnabled)
	if resp["runtime"] != true || resp["runtime_error"] != nil {
		t.Fatalf("PUT response = %v, want runtime true and no error", resp)
	}
	if *calls != 1 {
		t.Fatalf("factory calls = %d, want 1", *calls)
	}
	rt := s.knowledgeConnectors()
	if rt == nil {
		t.Fatal("runtime not installed after adding a connector")
	}
	if got := rt.Statuses(); len(got) != 1 || got[0].Name != "docs" || !got[0].Enabled {
		t.Fatalf("Statuses after add = %+v", got)
	}
	if rec := doPost(s, "/api/knowledge/connectors/docs/sync", nil); rec.Code != http.StatusAccepted {
		t.Fatalf("sync after add = %d %q, want 202", rec.Code, rec.Body.String())
	}
	waitSynced(t, rt, "docs")

	// Disable in place: same syncer, status follows, no second build.
	if resp := putConnectors(t, s, kcDocsDisabled); resp["runtime"] != true || resp["runtime_error"] != nil {
		t.Fatalf("PUT disable response = %v", resp)
	}
	if *calls != 1 {
		t.Fatalf("factory calls after edit = %d, want 1 (applied in place)", *calls)
	}
	if got := rt.Statuses(); len(got) != 1 || got[0].Enabled || got[0].LastSync.IsZero() {
		t.Fatalf("Statuses after disable = %+v, want disabled with history kept", got)
	}
	rec := doGetNoRole(s, "/api/config/knowledge/connectors/status")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runtime":true`) || !strings.Contains(rec.Body.String(), `"status":"disabled"`) {
		t.Fatalf("status after disable = %d %q", rec.Code, rec.Body.String())
	}

	// Remove: gone from the runtime, sync is 404 again.
	if resp := putConnectors(t, s, `{"connectors":[]}`); resp["runtime_error"] != nil {
		t.Fatalf("PUT remove response = %v", resp)
	}
	if got := rt.Statuses(); len(got) != 0 {
		t.Fatalf("Statuses after remove = %+v, want none", got)
	}
	if rec := doPost(s, "/api/knowledge/connectors/docs/sync", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("sync after remove = %d, want 404", rec.Code)
	}
	if rec := doPostRaw(s, "/api/config/knowledge/connectors/docs/sync", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("config sync after remove = %d, want 404", rec.Code)
	}
	if len(reloaded) != 3 || reloaded[0] != 1 || reloaded[1] != 1 || reloaded[2] != 0 {
		t.Fatalf("reload hook calls = %v, want [1 1 0] (add, disable, remove)", reloaded)
	}
}

// TestKnowledgeConnectors_HotLoadUnavailable covers the paths where the saved
// config cannot reach a running syncer: the save still succeeds and the
// response says why the runtime did not follow.
func TestKnowledgeConnectors_HotLoadUnavailable(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(s *Server, deps *Dependencies)
		wantErr   string
		wantAlive bool
	}{
		{
			name:    "no factory wired",
			setup:   func(*Server, *Dependencies) {},
			wantErr: "restart the hive",
		},
		{
			name: "factory fails",
			setup: func(_ *Server, deps *Dependencies) {
				deps.NewKnowledgeConnectors = func([]config.KnowledgeConnector) (KnowledgeConnectorRuntime, error) {
					return nil, errors.New("state dir unwritable")
				}
			},
			wantErr: "state dir unwritable",
		},
		{
			name: "factory returns nothing",
			setup: func(_ *Server, deps *Dependencies) {
				deps.NewKnowledgeConnectors = func([]config.KnowledgeConnector) (KnowledgeConnectorRuntime, error) {
					return nil, nil
				}
			},
			wantErr: "restart the hive",
		},
		{
			name: "runtime cannot reload",
			setup: func(_ *Server, deps *Dependencies) {
				deps.KnowledgeConnectors = &fakeConnectorRuntime{synced: make(chan string, 1)}
			},
			wantErr:   "does not support reloading",
			wantAlive: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, deps := apiServer(t)
			tt.setup(s, deps)
			deps.KnowledgeConnectorsReloaded = func([]config.KnowledgeConnector, KnowledgeConnectorRuntime) {
				t.Error("reload hook must not run when the runtime did not follow")
			}
			resp := putConnectors(t, s, kcDocsEnabled)
			msg, _ := resp["runtime_error"].(string)
			if !strings.Contains(msg, tt.wantErr) {
				t.Fatalf("runtime_error = %q, want %q", msg, tt.wantErr)
			}
			if resp["runtime"] != tt.wantAlive {
				t.Fatalf("runtime = %v, want %v", resp["runtime"], tt.wantAlive)
			}
			if n := len(deps.Config.Knowledge.Connectors); n != 1 {
				t.Fatalf("config must still be saved, have %d connectors", n)
			}
			if !tt.wantAlive {
				if rec := doPostRaw(s, "/api/config/knowledge/connectors/docs/sync", ""); rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("sync without runtime = %d, want 503", rec.Code)
				}
			}
		})
	}
}

// TestKnowledgeConnectors_HotLoadEmptyListNoRuntime: clearing the list on a
// hive that never had a syncer must not try to build one.
func TestKnowledgeConnectors_HotLoadEmptyListNoRuntime(t *testing.T) {
	s, deps := apiServer(t)
	factory, calls := hotloadFactory(t, &blockingConn{})
	deps.NewKnowledgeConnectors = factory
	resp := putConnectors(t, s, `{"connectors":[]}`)
	if resp["runtime"] != false || resp["runtime_error"] != nil || *calls != 0 {
		t.Fatalf("empty PUT response = %v, factory calls = %d", resp, *calls)
	}
}

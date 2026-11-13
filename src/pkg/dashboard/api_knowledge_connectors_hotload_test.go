package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// hotloadDocConn stands in for the document connector so a hot-loaded sync
// never touches the network.
type hotloadDocConn struct{ blockingConn }

func (*hotloadDocConn) Type() string { return connector.TypeDocument }

// withHotloadRuntime wires ReloadKnowledgeConnectors the way boot does: build
// the syncer on first use, reconfigure it afterwards.
func withHotloadRuntime(t *testing.T, s *Server) {
	t.Helper()
	reg := connector.NewRegistry()
	if err := reg.Register(connector.TypeDocument, func(connector.ConnectorConfig, connector.Deps) (connector.Connector, error) {
		return &hotloadDocConn{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	vault := t.TempDir()
	var syncer *connector.Syncer
	s.deps.ReloadKnowledgeConnectors = func(entries []config.KnowledgeConnector) (KnowledgeConnectorRuntime, error) {
		cfgs := make([]connector.ConnectorConfig, 0, len(entries))
		for _, e := range entries {
			rc, err := knowledgeConnectorRuntimeConfig(e)
			if err != nil {
				return nil, err
			}
			cfgs = append(cfgs, rc)
		}
		if syncer != nil {
			return syncer, syncer.Reconfigure(cfgs)
		}
		if len(cfgs) == 0 {
			return nil, nil
		}
		built, err := connector.NewSyncer(cfgs, connector.SyncerOptions{
			Registry: reg,
			VaultDir: func(string) (string, error) { return vault, nil },
		})
		if err != nil {
			return nil, err
		}
		syncer = built
		return syncer, nil
	}
}

// A connector added from the dashboard to a hive booted with none is loaded
// without a restart: it is listed, Sync answers 202, and disabling or
// removing it takes effect immediately (#18091).
func TestKnowledgeConnectorsHotLoad(t *testing.T) {
	s := covApiServer(t)
	withHotloadRuntime(t, s)

	if rec := doPostRaw(s, "/api/knowledge/connectors/docs/sync", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("sync before any connector = %d, want 404", rec.Code)
	}

	put := func(enabled bool) {
		t.Helper()
		body := `{"connectors":[{"name":"docs","type":"document","enabled":` + strconv.FormatBool(enabled) +
			`,"layer":"project","scope":{"url":"https://example.org/guide.html"}}]}`
		if rec := doPutRaw(s, "/api/config/knowledge/connectors", body); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d %q", rec.Code, rec.Body.String())
		}
	}
	put(true)

	got := decodeConnectorList(t, s)
	if len(got) != 1 || got[0].Name != "docs" || !got[0].Enabled {
		t.Fatalf("connectors after add = %+v, want docs enabled", got)
	}
	rec := doGetNoRole(s, "/api/config/knowledge/connectors/status")
	var status struct {
		Runtime bool `json:"runtime"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil || !status.Runtime {
		t.Fatalf("status runtime = %v (%v): %s", status.Runtime, err, rec.Body.String())
	}

	if rec := doPostRaw(s, "/api/knowledge/connectors/docs/sync", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("sync after hot-load = %d %q, want 202", rec.Code, rec.Body.String())
	}
	deadline := time.After(10 * time.Second)
	for {
		if st, ok := knowledgeConnectorStatus(s.knowledgeConnectors(), "docs"); ok && !st.LastSync.IsZero() && !st.Running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("hot-loaded connector never finished its sync")
		case <-time.After(5 * time.Millisecond):
		}
	}

	put(false)
	if got := decodeConnectorList(t, s); len(got) != 1 || got[0].Enabled {
		t.Fatalf("connectors after disable = %+v, want docs disabled", got)
	}

	if rec := doPutRaw(s, "/api/config/knowledge/connectors", `{"connectors":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT empty = %d %q", rec.Code, rec.Body.String())
	}
	if got := decodeConnectorList(t, s); len(got) != 0 {
		t.Fatalf("connectors after remove = %+v, want none", got)
	}
	if rec := doPostRaw(s, "/api/knowledge/connectors/docs/sync", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("sync after remove = %d, want 404", rec.Code)
	}
}

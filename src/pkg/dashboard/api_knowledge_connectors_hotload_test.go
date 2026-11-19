package dashboard

import (
	"encoding/json"
	"errors"
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

// A save the running syncer refuses is still persisted, but the PUT response
// carries runtime_error verbatim and keeps the previous runtime, so the
// dashboard can say "saved, but not reloaded" instead of a plain success
// (#18091). A save that reloads cleanly answers without runtime_error.
func TestKnowledgeConnectorsPutReportsRuntimeError(t *testing.T) {
	s := covApiServer(t)
	prev := newTestConnectorSyncer(t, &blockingConn{})
	s.deps.KnowledgeConnectors = prev
	var fail bool
	s.deps.ReloadKnowledgeConnectors = func([]config.KnowledgeConnector) (KnowledgeConnectorRuntime, error) {
		if fail {
			return nil, errors.New("plaud: scope.home must be an absolute path")
		}
		return prev, nil
	}
	body := `{"connectors":[{"name":"docs","type":"document","enabled":true,"layer":"project","scope":{"url":"https://example.org/guide.html"}}]}`

	put := func() map[string]any {
		t.Helper()
		rec := doPutRaw(s, "/api/config/knowledge/connectors", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d %q", rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		return resp
	}

	resp := put()
	if _, ok := resp["runtime_error"]; ok {
		t.Fatalf("clean reload must not report runtime_error: %v", resp)
	}
	if resp["runtime"] != true {
		t.Fatalf("runtime = %v, want true while a syncer is running", resp["runtime"])
	}

	fail = true
	resp = put()
	if got, _ := resp["runtime_error"].(string); got != "plaud: scope.home must be an absolute path" {
		t.Fatalf("runtime_error = %q, want the reload error verbatim (%v)", got, resp)
	}
	if resp["runtime"] != true {
		t.Fatalf("runtime = %v, want true: the previous syncer stays in place", resp["runtime"])
	}
	if s.knowledgeConnectors() != prev {
		t.Fatal("a failed reload must keep the previous runtime")
	}
	if got := s.deps.Config.Knowledge.Connectors; len(got) != 1 || got[0].Name != "docs" {
		t.Fatalf("config after failed reload = %+v, want the saved list", got)
	}
}

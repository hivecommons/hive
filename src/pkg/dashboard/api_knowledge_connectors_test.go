package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// blockingConn emits one page per sync; when release is non-nil each sync
// waits for it, so a test can observe the running state.
type blockingConn struct {
	started chan struct{}
	release chan struct{}
}

func (c *blockingConn) Type() string                             { return "fake" }
func (c *blockingConn) Validate(connector.ConnectorConfig) error { return nil }
func (c *blockingConn) Sync(ctx context.Context, _ connector.Cursor, emit func(connector.Page) error) (connector.Cursor, error) {
	if c.started != nil {
		c.started <- struct{}{}
	}
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "c1", emit(connector.Page{ID: "p1", Title: "Page", Markdown: "body"})
}

func newTestConnectorSyncer(t *testing.T, conn connector.Connector) *connector.Syncer {
	t.Helper()
	reg := connector.NewRegistry()
	if err := reg.Register("fake", func(connector.ConnectorConfig, connector.Deps) (connector.Connector, error) {
		return conn, nil
	}); err != nil {
		t.Fatal(err)
	}
	vault := t.TempDir()
	s, err := connector.NewSyncer([]connector.ConnectorConfig{
		{Name: "wiki", Type: "fake", Enabled: true, Layer: "project"},
	}, connector.SyncerOptions{
		Registry: reg,
		VaultDir: func(string) (string, error) { return vault, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func decodeConnectorList(t *testing.T, s *Server) []connector.Status {
	t.Helper()
	rec := doGet(s, "/api/knowledge/connectors")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Connectors []connector.Status `json:"connectors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if body.Connectors == nil {
		t.Fatalf("connectors must be a JSON array, got %s", rec.Body.String())
	}
	return body.Connectors
}

func TestKnowledgeConnectorsListEmpty(t *testing.T) {
	s, _ := apiServer(t)
	if got := decodeConnectorList(t, s); len(got) != 0 {
		t.Fatalf("connectors = %+v, want empty", got)
	}
	if rec := doPost(s, "/api/knowledge/connectors/wiki/sync", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("sync without syncer = %d, want 404", rec.Code)
	}
}

func TestKnowledgeConnectorsListAndSync(t *testing.T) {
	s, deps := apiServer(t)
	conn := &blockingConn{started: make(chan struct{}, 1), release: make(chan struct{})}
	syncer := newTestConnectorSyncer(t, conn)
	deps.KnowledgeConnectors = syncer

	got := decodeConnectorList(t, s)
	if len(got) != 1 || got[0].Name != "wiki" || got[0].Type != "fake" || got[0].Layer != "project" || !got[0].Enabled || got[0].Running {
		t.Fatalf("connectors = %+v", got)
	}

	if rec := doPostNoOwner(s, "/api/knowledge/connectors/wiki/sync", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner sync = %d, want 403", rec.Code)
	}
	if rec := doPost(s, "/api/knowledge/connectors/nope/sync", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown connector = %d, want 404", rec.Code)
	}

	rec := doPost(s, "/api/knowledge/connectors/wiki/sync", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("sync = %d: %s", rec.Code, rec.Body.String())
	}
	var st connector.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || st.Name != "wiki" || !st.Running {
		t.Fatalf("sync response = %+v, %v", st, err)
	}

	select {
	case <-conn.started:
	case <-time.After(10 * time.Second):
		t.Fatal("sync never started")
	}
	if rec := doPost(s, "/api/knowledge/connectors/wiki/sync", nil); rec.Code != http.StatusConflict {
		t.Fatalf("sync while running = %d, want 409", rec.Code)
	}
	close(conn.release)

	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := syncer.Status("wiki")
		if !st.Running && !st.LastSync.IsZero() {
			if st.Pages != 1 || st.Facts != 1 || st.Cursor != "c1" || st.LastError != "" {
				t.Fatalf("status after sync = %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sync did not finish: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := decodeConnectorList(t, s); got[0].Pages != 1 || got[0].LastSync.IsZero() {
		t.Fatalf("listed status after sync = %+v", got[0])
	}
}

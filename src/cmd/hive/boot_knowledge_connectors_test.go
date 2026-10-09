package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

func TestConnectorConfigsConversion(t *testing.T) {
	off := false
	got := connectorConfigs([]config.KnowledgeConnector{
		{Name: "wiki", Type: "confluence", Interval: "2h", Layer: "org", Scope: map[string]string{"spaces": "ENG"},
			Auth: config.KnowledgeConnectorAuth{Env: "CONFLUENCE_TOKEN"}},
		{Name: "notes", Type: "notion", Enabled: &off, Layer: "project", Auth: config.KnowledgeConnectorAuth{File: "/secrets/notion"}},
	})
	if len(got) != 2 {
		t.Fatalf("got %d configs", len(got))
	}
	if c := got[0]; c.Name != "wiki" || c.Type != "confluence" || !c.Enabled || c.Interval != 2*time.Hour ||
		c.Layer != "org" || c.Scope["spaces"] != "ENG" || c.Auth.Env != "CONFLUENCE_TOKEN" {
		t.Fatalf("wiki = %+v", c)
	}
	if c := got[1]; c.Enabled || c.Interval != 0 || c.Auth.File != "/secrets/notion" {
		t.Fatalf("notes = %+v", c)
	}
}

type bootFakeConnector struct{}

func (bootFakeConnector) Type() string                             { return "fake" }
func (bootFakeConnector) Validate(connector.ConnectorConfig) error { return nil }
func (bootFakeConnector) Sync(_ context.Context, _ connector.Cursor, emit func(connector.Page) error) (connector.Cursor, error) {
	return "c1", emit(connector.Page{ID: "guide", Title: "Guide", Markdown: "How we deploy."})
}

func TestNewKnowledgeConnectorSyncer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if s, err := newKnowledgeConnectorSyncer(nil, nil, nil, t.TempDir(), logger); s != nil || err != nil {
		t.Fatalf("no connectors = %v, %v; want nil, nil", s, err)
	}

	base := t.TempDir()
	reg := connector.NewRegistry()
	if err := reg.Register("fake", func(connector.ConnectorConfig, connector.Deps) (connector.Connector, error) {
		return bootFakeConnector{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, logger)
	s, err := newKnowledgeConnectorSyncer([]config.KnowledgeConnector{
		{Name: "guide", Type: "fake", Layer: "project"},
	}, reg, api, base, logger)
	if err != nil || s == nil {
		t.Fatalf("newKnowledgeConnectorSyncer = %v, %v", s, err)
	}
	st, err := s.SyncNow(context.Background(), "guide")
	if err != nil {
		t.Fatalf("SyncNow: %v (status %+v)", err, st)
	}
	vault := filepath.Join(base, "connectors", "project")
	store := api.GetVaultStore(vault)
	if store == nil {
		t.Fatalf("vault %s not connected", vault)
	}
	if store.Stats().TotalPages == 0 {
		t.Fatalf("vault not reindexed after sync: %+v", store.Stats())
	}
	if _, err := os.Stat(filepath.Join(base, "connector-state", "status.json")); err != nil {
		t.Fatalf("status not persisted: %v", err)
	}
	// A second sync reuses the connected vault.
	if _, err := s.SyncNow(context.Background(), "guide"); err != nil {
		t.Fatal(err)
	}
	if n := len(api.Vaults()); n != 1 {
		t.Fatalf("vaults = %d, want 1", n)
	}
}

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// bootFakePublisher is a connector type that can publish; it records every
// published batch.
type bootFakePublisher struct {
	bootFakeConnector
	pages *[]connector.Page
}

func (bootFakePublisher) Type() string { return "fakepub" }
func (p bootFakePublisher) Publish(_ context.Context, _ string, pages []connector.Page) error {
	*p.pages = append(*p.pages, pages...)
	return nil
}

func bootPublishRegistry(t *testing.T, pages *[]connector.Page) *connector.Registry {
	t.Helper()
	reg := connector.NewRegistry()
	if err := reg.Register("fake", func(connector.ConnectorConfig, connector.Deps) (connector.Connector, error) {
		return bootFakeConnector{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register("fakepub", func(connector.ConnectorConfig, connector.Deps) (connector.Connector, error) {
		return bootFakePublisher{pages: pages}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return reg
}

// bootPublishLayer writes one curator-promoted fact into a fresh org layer
// directory and returns the layer config pointing at it.
func bootPublishLayer(t *testing.T) []config.KnowledgeLayer {
	t.Helper()
	dir := t.TempDir()
	fact := "---\ntitle: Deploy\ntype: decision\nsource: promoted from project by curator: high confidence\n---\nShip on Tuesdays.\n"
	if err := os.WriteFile(filepath.Join(dir, "deploy.md"), []byte(fact), 0o644); err != nil {
		t.Fatal(err)
	}
	return []config.KnowledgeLayer{{Type: "org", Path: dir}}
}

func TestNewKnowledgePublishMirror(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	entries := []config.KnowledgeConnector{
		{Name: "team-docs", Type: "fakepub", Layer: "org"},
		{Name: "wiki", Type: "fake", Layer: "project"},
	}
	publish := func(name string) config.KnowledgePublish {
		return config.KnowledgePublish{Connector: name, Layers: []string{"org"}, Root: "Hive/Knowledge"}
	}
	tests := []struct {
		name    string
		pub     config.KnowledgePublish
		wantErr string
		wantNil bool
	}{
		{name: "not configured", pub: config.KnowledgePublish{}, wantNil: true},
		{name: "unknown connector", pub: publish("missing"), wantErr: "not a knowledge.connectors entry"},
		{name: "type cannot publish", pub: publish("wiki"), wantErr: "does not support publishing"},
		{name: "configured", pub: publish("team-docs")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var published []connector.Page
			base := t.TempDir()
			m, err := newKnowledgePublishMirror(tt.pub, entries, bootPublishLayer(t), bootPublishRegistry(t, &published), base, logger)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || m != nil {
					t.Fatalf("got %v, %v; want error containing %q", m, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantNil {
				if m != nil {
					t.Fatalf("mirror = %v, want nil", m)
				}
				return
			}
			if m == nil {
				t.Fatal("mirror is nil")
			}
			if st := m.Status(); st.Connector != "team-docs" || st.Root != "Hive/Knowledge" {
				t.Fatalf("status = %+v", st)
			}
			if _, err := m.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(published) != 1 || published[0].ID != "org/deploy" ||
				published[0].Attrs[connector.PublishKeyAttr] != connector.PublishKey("org/deploy") {
				t.Fatalf("published = %+v", published)
			}
			if _, err := os.Stat(filepath.Join(base, "connector-state", "_publish", "state.json")); err != nil {
				t.Fatalf("publish state not persisted: %v", err)
			}
		})
	}
}

func TestBootKnowledgePublish(t *testing.T) {
	tests := []struct {
		name        string
		connector   string
		promotion   bool
		wantStarted bool
		wantLog     string
	}{
		{name: "not configured"},
		{name: "bad connector leaves publishing off", connector: "missing", wantLog: "knowledge publish mirror disabled"},
		{name: "configured starts the mirror", connector: "team-docs", wantStarted: true, wantLog: "knowledge publish mirror started"},
		{name: "configured with promotion scheduler", connector: "team-docs", promotion: true, wantStarted: true, wantLog: "knowledge publish mirror started"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			knowledge.SetBaseDirForTest(t, t.TempDir())
			cfg := &config.Config{}
			cfg.Knowledge.Connectors = []config.KnowledgeConnector{{Name: "team-docs", Type: "fakepub", Layer: "org"}}
			cfg.Knowledge.Layers = bootPublishLayer(t)
			if tt.connector != "" {
				cfg.Knowledge.Publish = config.KnowledgePublish{Connector: tt.connector, Layers: []string{"org"}, Root: "Hive"}
			}
			b, log := newDepsTestBoot(t, cfg)
			if tt.promotion {
				b.promotionScheduler = knowledge.NewPromotionScheduler(nil, knowledge.CuratorConfig{}, b.logger)
			}
			var started *connector.Mirror
			deps := bootKnowledgeDeps{startPublishMirror: func(_ context.Context, m *connector.Mirror) { started = m }}
			var published []connector.Page

			b.bootKnowledgePublish(deps, bootPublishRegistry(t, &published))

			if got := started != nil; got != tt.wantStarted {
				t.Fatalf("started = %v, want %v\n%s", got, tt.wantStarted, log.String())
			}
			if started != b.knowledgePublish {
				t.Fatal("started mirror is not the boot's mirror")
			}
			if rt := b.knowledgePublishRuntime(); (rt != nil) != tt.wantStarted {
				t.Fatalf("runtime = %v, want non-nil %v", rt, tt.wantStarted)
			}
			if tt.wantLog != "" && !strings.Contains(log.String(), tt.wantLog) {
				t.Fatalf("log missing %q:\n%s", tt.wantLog, log.String())
			}
		})
	}
}

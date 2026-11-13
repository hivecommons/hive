package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// TestNewKnowledgeConnectorFactory: a hive booted with zero connectors keeps
// boot behaviour (no syncer) but hands the dashboard a factory that builds
// and starts one on demand (#18091).
func TestNewKnowledgeConnectorFactory(t *testing.T) {
	knowledge.SetBaseDirForTest(t, t.TempDir())
	cfg := &config.Config{}
	b, log := newDepsTestBoot(t, cfg)
	var started *connector.Syncer
	deps := bootKnowledgeDeps{startConnectorSyncer: func(_ context.Context, s *connector.Syncer) { started = s }}

	b.bootKnowledgeConnectors(deps)

	if b.knowledgeConnectors != nil || b.knowledgeConnectorRuntime() != nil || started != nil {
		t.Fatal("zero connectors at boot must not build a syncer")
	}
	if b.knowledgeConnectorFactory == nil {
		t.Fatal("boot must wire the hot-load factory even with zero connectors")
	}

	if rt, err := b.knowledgeConnectorFactory(nil); rt != nil || err != nil {
		t.Fatalf("factory(empty) = %v, %v; want nil interface, nil", rt, err)
	}

	rt, err := b.knowledgeConnectorFactory([]config.KnowledgeConnector{
		{Name: "guide", Type: "document", Layer: "project", Scope: map[string]string{"url": "https://example.org/guide.html"}},
	})
	if err != nil || rt == nil {
		t.Fatalf("factory = %v, %v", rt, err)
	}
	syncer, ok := rt.(*connector.Syncer)
	if !ok || syncer != started {
		t.Fatalf("factory must start the syncer it returns: got %T, started %p", rt, started)
	}
	if got := rt.Statuses(); len(got) != 1 || got[0].Name != "guide" {
		t.Fatalf("Statuses = %+v", got)
	}
	if !strings.Contains(log.String(), "hot_load=true") {
		t.Fatalf("log missing hot-load start line:\n%s", log.String())
	}
}

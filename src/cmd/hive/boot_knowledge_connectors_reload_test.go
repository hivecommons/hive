package main

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// A hive booted with zero connectors builds and starts the syncer the first
// time the dashboard saves one, then reconfigures that same syncer (#18091).
func TestReloadKnowledgeConnectors(t *testing.T) {
	knowledge.SetBaseDirForTest(t, t.TempDir())
	b, log := newDepsTestBoot(t, &config.Config{})
	var started []*connector.Syncer
	b.bootKnowledgeConnectors(bootKnowledgeDeps{startConnectorSyncer: func(_ context.Context, s *connector.Syncer) {
		started = append(started, s)
	}})
	if b.knowledgeConnectorRuntime() != nil || len(started) != 0 {
		t.Fatal("no connectors at boot must leave the syncer off")
	}

	if rt, err := b.reloadKnowledgeConnectors(nil); rt != nil || err != nil {
		t.Fatalf("reload with none = %v, %v; want nil, nil", rt, err)
	}

	docs := config.KnowledgeConnector{Name: "docs", Type: connector.TypeDocument, Layer: "project",
		Scope: map[string]string{"url": "https://example.org/guide.html"}}
	rt, err := b.reloadKnowledgeConnectors([]config.KnowledgeConnector{docs})
	if err != nil || rt == nil {
		t.Fatalf("reload = %v, %v\n%s", rt, err, log.String())
	}
	if len(started) != 1 || started[0] != b.knowledgeConnectors {
		t.Fatalf("hot-loaded syncer not started once: %d", len(started))
	}
	if sts := rt.Statuses(); len(sts) != 1 || sts[0].Name != "docs" {
		t.Fatalf("statuses = %+v", sts)
	}

	rt2, err := b.reloadKnowledgeConnectors(nil)
	if err != nil || rt2 != rt {
		t.Fatalf("second reload = %v, %v; want the same runtime", rt2, err)
	}
	if sts := rt2.Statuses(); len(sts) != 0 {
		t.Fatalf("removed connector still listed: %+v", sts)
	}
	if len(started) != 1 {
		t.Fatalf("reconfigure must not start a second syncer: %d", len(started))
	}
}

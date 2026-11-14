package main

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

func plaudEntry() config.KnowledgeConnector {
	return config.KnowledgeConnector{Name: "voice", Type: connector.TypePlaud, Layer: "personal"}
}

// The Inbox follows the connector set the dashboard saves (#18091): a hive
// booted with no connectors lists no sources, a hot-loaded Plaud connector
// becomes the direct-read source on the same save, and removing every
// connector drops it again without a restart.
func TestReloadKnowledgeConnectorsRefreshesInboxSources(t *testing.T) {
	knowledge.SetBaseDirForTest(t, t.TempDir())
	b, log := newDepsTestBoot(t, &config.Config{})
	b.bootKnowledgeConnectors(bootKnowledgeDeps{startConnectorSyncer: func(context.Context, *connector.Syncer) {}})
	if b.inboxSvc == nil {
		t.Fatal("the Inbox must boot even with no connectors")
	}
	if b.inboxSvc.HasSource(inbox.PlaudType) {
		t.Fatal("no connectors at boot must leave the Inbox without a plaud source")
	}

	rt, err := b.reloadKnowledgeConnectors([]config.KnowledgeConnector{plaudEntry()})
	if err != nil || rt == nil {
		t.Fatalf("reload = %v, %v\n%s", rt, err, log.String())
	}
	if !b.inboxSvc.HasSource(inbox.PlaudType) {
		t.Fatalf("hot-loaded plaud connector must become an Inbox source\n%s", log.String())
	}
	srcs := b.inboxSources(knowledge.BaseDir(), []config.KnowledgeConnector{plaudEntry()}, b.knowledgeConnectors)
	if len(srcs) != 1 {
		t.Fatalf("sources = %d, want the one plaud source", len(srcs))
	}
	if _, direct := srcs[0].(*inbox.PreferDirect); !direct {
		t.Fatalf("plaud source with a live syncer = %T, want *inbox.PreferDirect", srcs[0])
	}

	if rt, err := b.reloadKnowledgeConnectors(nil); err != nil || rt == nil {
		t.Fatalf("reload with none = %v, %v; want the same runtime kept", rt, err)
	}
	if b.inboxSvc.HasSource(inbox.PlaudType) {
		t.Fatal("removing every connector must drop the Inbox plaud source")
	}
}

// A hive booted with a Plaud connector attaches the direct-read source once
// the syncer exists, not the fact-only one the Inbox had to settle for while
// booting ahead of the syncer (#18091).
func TestBootKnowledgeConnectorsAttachesDirectPlaudSource(t *testing.T) {
	knowledge.SetBaseDirForTest(t, t.TempDir())
	cfg := &config.Config{}
	cfg.Knowledge.Connectors = []config.KnowledgeConnector{plaudEntry()}
	b, log := newDepsTestBoot(t, cfg)

	if srcs := b.inboxSources(knowledge.BaseDir(), cfg.Knowledge.Connectors, nil); len(srcs) != 1 {
		t.Fatalf("sources without a syncer = %d, want one fact-backed plaud source", len(srcs))
	} else if _, facts := srcs[0].(*inbox.FactSource); !facts {
		t.Fatalf("plaud source without a syncer = %T, want *inbox.FactSource", srcs[0])
	}

	b.bootKnowledgeConnectors(bootKnowledgeDeps{startConnectorSyncer: func(context.Context, *connector.Syncer) {}})
	if b.knowledgeConnectorRuntime() == nil {
		t.Fatalf("syncer not built\n%s", log.String())
	}
	if !b.inboxSvc.HasSource(inbox.PlaudType) {
		t.Fatal("plaud source missing after boot")
	}
	srcs := b.inboxSources(knowledge.BaseDir(), cfg.Knowledge.Connectors, b.knowledgeConnectors)
	if len(srcs) != 1 {
		t.Fatalf("sources = %d, want one", len(srcs))
	}
	if _, direct := srcs[0].(*inbox.PreferDirect); !direct {
		t.Fatalf("plaud source after boot = %T, want *inbox.PreferDirect", srcs[0])
	}
}

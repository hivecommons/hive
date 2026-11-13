package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// With no knowledge connectors configured the Inbox service must still boot
// so GET /api/inbox returns an empty list, not a 404 (#17918).
func TestBootInboxWithoutConnectors(t *testing.T) {
	b := &boot{cfg: &config.Config{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	b.bootKnowledgeConnectors(bootKnowledgeDeps{})
	if b.inboxSvc == nil {
		t.Fatal("inbox service not built without connectors")
	}
	items, err := b.inboxSvc.Items(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("items = %v %v", items, err)
	}
	if b.knowledgeConnectors != nil {
		t.Fatal("connector syncer should stay nil with no connectors")
	}
}

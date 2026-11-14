package inbox

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSetSourcesFollowsConnectors: a service booted with no sources lists
// nothing, picks up a source handed to SetSources on the next Items call,
// and drops it again when the set is replaced (#18091).
func TestSetSourcesFollowsConnectors(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "inbox.json"))
	svc := NewService(config.InboxConfig{}, store, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if svc.HasSource(PlaudType) {
		t.Fatal("no sources wired at construction")
	}
	if items, err := svc.Items(context.Background()); err != nil || len(items) != 0 {
		t.Fatalf("Items without sources = %+v, %v", items, err)
	}

	svc.SetSources([]Source{&fakeSource{items: []Item{{Source: PlaudType, Connector: "voice", ID: "r1", Title: "Standup"}}}})
	if !svc.HasSource(PlaudType) {
		t.Fatal("SetSources must wire the plaud source")
	}
	items, err := svc.Items(context.Background())
	if err != nil || len(items) != 1 || items[0].ID != "r1" {
		t.Fatalf("Items after SetSources = %+v, %v", items, err)
	}

	svc.SetSources(nil)
	if svc.HasSource(PlaudType) {
		t.Fatal("SetSources(nil) must drop every source")
	}
	if items, err := svc.Items(context.Background()); err != nil || len(items) != 0 {
		t.Fatalf("Items after clearing sources = %+v, %v", items, err)
	}
}

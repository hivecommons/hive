package adminmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pendingFixture(id string, created time.Time) PendingConfirmation {
	return PendingConfirmation{ID: id, Operation: WriteOpAgentPause, Args: map[string]any{"agent": id}, Hive: "hive-a", CreatedAt: created, ExpiresAt: created.Add(time.Hour)}
}

// #9162: previews that are never confirmed must not grow the store without bound.
func TestFilePendingStoreCapsEntriesEvictingOldest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	store := NewFilePendingStore(path)
	base := time.Now().UTC()
	total := MaxPendingConfirmations + 5
	for i := 0; i < total; i++ {
		if err := store.Put(context.Background(), pendingFixture(fmt.Sprintf("p%03d", i), base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != MaxPendingConfirmations {
		t.Fatalf("entries = %d, want cap %d", len(list), MaxPendingConfirmations)
	}
	if _, err := store.Take(context.Background(), "p000"); err != ErrConfirmationMissing {
		t.Fatalf("oldest entry survived the cap: err = %v", err)
	}
	newest := fmt.Sprintf("p%03d", total-1)
	if _, err := store.Take(context.Background(), newest); err != nil {
		t.Fatalf("newest entry evicted: %v", err)
	}
}

func TestMemoryPendingStoreCapsEntriesEvictingOldest(t *testing.T) {
	store := NewMemoryPendingStore()
	base := time.Now().UTC()
	for i := 0; i < MaxPendingConfirmations+3; i++ {
		if err := store.Put(context.Background(), pendingFixture(fmt.Sprintf("p%03d", i), base.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := store.List(context.Background())
	if len(list) != MaxPendingConfirmations {
		t.Fatalf("entries = %d, want cap %d", len(list), MaxPendingConfirmations)
	}
	if _, err := store.Take(context.Background(), "p000"); err != ErrConfirmationMissing {
		t.Fatalf("oldest entry survived the cap: err = %v", err)
	}
}

// #9162: a torn write must not leave every later preview/confirm failing on a JSON error.
func TestFilePendingStoreRecoversFromTornFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.json")
	if err := os.WriteFile(path, []byte(`{"abc": {"id": "abc", "operat`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFilePendingStore(path)
	if err := store.Put(context.Background(), pendingFixture("fresh", time.Now().UTC())); err != nil {
		t.Fatalf("put after torn file: %v", err)
	}
	list, err := store.List(context.Background())
	if err != nil || len(list) != 1 || list[0].ID != "fresh" {
		t.Fatalf("list = %#v, err = %v", list, err)
	}
	quarantined, _ := filepath.Glob(path + ".corrupt-*")
	if len(quarantined) != 1 {
		t.Fatalf("corrupt file not preserved for inspection: %v", quarantined)
	}
}

// #9162: the file must be replaced via rename, never truncated in place, so a
// crash mid-write leaves the previous complete file rather than a torn one.
func TestFilePendingStoreReplacesFileAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.json")
	store := NewFilePendingStore(path)
	if err := store.Put(context.Background(), pendingFixture("first", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A hard link pins the old inode: an in-place truncate+write mutates it,
	// a temp-file rename leaves it untouched.
	snapshot := filepath.Join(dir, "snapshot.json")
	if err := os.Link(path, snapshot); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if err := store.Put(context.Background(), pendingFixture("second", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	pinned, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(pinned) != string(before) {
		t.Fatalf("pending file was rewritten in place:\nbefore=%s\nafter=%s", before, pinned)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pending.json") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// #9162: only the request ConfirmWrite executes is persisted, not the whole
// human-facing preview (summary, effects, details duplicate the args).
func TestWritePreviewPersistsOnlyExecutableRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	h := NewHandler(&writeProvider{}, WithWritesEnabled(true), WithPendingStore(NewFilePendingStore(path)), WithHiveID("hive-a"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"agent.pause","args":{"agent":"scanner"}}}}`)))
	_ = resultText(t, rec.Body.Bytes())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var items map[string]PendingConfirmation
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	for _, p := range items {
		if p.Preview.Request.Path != "/api/pause/scanner" || p.Preview.Request.Method != http.MethodPost {
			t.Fatalf("stored request = %#v", p.Preview.Request)
		}
		if p.Preview.Summary != "" || len(p.Preview.Effects) != 0 || p.Preview.WideningDisclosure != "" || p.Preview.ConfirmationMessage != "" || p.Preview.Details != nil {
			t.Fatalf("stored preview carries display-only fields: %#v", p.Preview)
		}
	}
}

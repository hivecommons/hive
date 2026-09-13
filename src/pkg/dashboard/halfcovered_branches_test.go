package dashboard

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================
// api.go — EnableLifecyclePersistence error branch
//
// The function sat at 50% coverage: only the happy path ran. This test
// exercises the warn-and-continue half.
// ============================================================

// TestEnableLifecyclePersistenceWarnsOnError: an unreadable existing journal
// must be reported (the #5656 history is silently gone otherwise) but must not
// crash startup — the server keeps running with an empty timeline.
func TestEnableLifecyclePersistenceWarnsOnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions are advisory for root")
	}
	resetLifecycleStore()
	path := filepath.Join(t.TempDir(), "lifecycle-timeline.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"journeys":[]}`), 0o000); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	s := newTestServer()
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))

	s.EnableLifecyclePersistence(path) // must warn, not panic

	if !strings.Contains(buf.String(), "lifecycle timeline persistence unavailable") {
		t.Errorf("expected persistence-unavailable warning, got: %s", buf.String())
	}
}

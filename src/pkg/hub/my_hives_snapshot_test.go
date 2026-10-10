package hub

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandleMyHivesServesCachedSnapshotWithoutNetwork(t *testing.T) {
	origUsers := saasUsersDir
	saasUsersDir = filepath.Join(t.TempDir(), "users")
	if err := os.MkdirAll(saasUsersDir, 0o755); err != nil {
		t.Fatalf("mkdir users: %v", err)
	}
	t.Cleanup(func() { saasUsersDir = origUsers })
	mkUser(t, "snapshot-owner")
	s := &HubServer{
		logger:         slog.Default(),
		hubSecret:      testHubSecret,
		keyGenerations: legacyGenerationSet(testHubSecret),
		myHivesCache:   make(map[string]myHivesSnapshot),
	}
	var rows []string
	for i := 0; i < 50; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"hive-%02d","role":"owner"}`, i))
	}
	body := []byte(`{"hives":[` + strings.Join(rows, ",") + `],"hives_total":50,"hives_matched":50}`)
	key := myHivesSnapshotKey("snapshot-owner", "")
	s.myHivesCache[key] = myHivesSnapshot{body: body, storedAt: time.Now()}

	origFetch := fetchCommitBehindCount
	fetchCommitBehindCount = func(base, head string, logger *slog.Logger) (int, bool, error) {
		t.Fatalf("cached my-hives response attempted GitHub compare for %s...%s", base, head)
		return 0, false, nil
	}
	t.Cleanup(func() { fetchCommitBehindCount = origFetch })

	req := reqWithUser(http.MethodGet, "/api/saas/my-hives", "", "snapshot-owner")
	rec := httptest.NewRecorder()
	start := time.Now()
	s.handleMyHives(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("handleMyHives = %d, body=%s", rec.Code, rec.Body.String())
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("cached my-hives took %s, want <100ms", elapsed)
	}
	if got := rec.Header().Get("Server-Timing"); !strings.Contains(got, myHivesServerTimingCacheHit) {
		t.Fatalf("Server-Timing = %q, want cache hit timing", got)
	}
	if rec.Body.String() != string(body) {
		t.Fatalf("cached body changed: %s", rec.Body.String())
	}
}

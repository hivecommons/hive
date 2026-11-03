package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

func TestSharedUIDReadTokenPermissionsAndDelivery(t *testing.T) {
	auth, logs, closeAuth := newFakeAppAuth(t, "unused")
	defer closeAuth()
	useTempCacheDir(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Permissions map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		want := map[string]string{"metadata": "read", "contents": "read", "actions": "read", "checks": "read", "pull_requests": "read", "issues": "read"}
		if !reflect.DeepEqual(req.Permissions, want) {
			t.Errorf("permissions = %v, want exactly %v", req.Permissions, want)
		}
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_shared_read", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	}))
	defer server.Close()
	auth.apiURL = server.URL
	for _, name := range []string{"scanner", "quality"} {
		path := AgentTokenCachePath(name)
		for i := 0; i < 2; i++ {
			if err := auth.WriteAgentToken(context.Background(), name, crux.SharedUIDReadTokenTier, 0); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "ghs_shared_read" {
				t.Fatalf("cache unavailable: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %o, want 600", info.Mode().Perm())
			}
			// A pre-existing cache must also be narrowed on refresh.
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		}
	}
	if requests.Load() != 4 {
		t.Fatalf("mint requests = %d, want 4", requests.Load())
	}
	if AgentTokenCachePath("scanner") == AgentTokenCachePath("quality") {
		t.Fatal("agents share a cache path")
	}
	if strings.Contains(logs.String(), "ghs_shared_read") {
		t.Fatal("token leaked in log")
	}
}

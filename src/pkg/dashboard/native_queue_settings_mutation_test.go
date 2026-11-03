package dashboard

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestNativeQueueSettingsUpdatesAndValidation(t *testing.T) {
	s := covApiServer(t)
	put := func(patch map[string]any, status int) {
		t.Helper()
		rec := doPut(s, "/api/config/auto-merge", map[string]any{"native_merge_queue": patch})
		if rec.Code != status {
			t.Fatalf("PUT: %d want %d: %s", rec.Code, status, rec.Body.String())
		}
	}
	put(map[string]any{
		"enabled": true, "repos": []string{"HiveCommons/Hive"},
		"bases":     []string{"HiveCommons/Hive#v5", "hivecommons/hive#v5"},
		"probe_ttl": "5m", "skip_current_base_gate": true,
	}, http.StatusOK)
	q := s.deps.Config.AutoMerge.NativeMergeQueue
	if !q.Enabled || !q.SkipCurrentBaseGate || q.ProbeTTL != 5*time.Minute ||
		!reflect.DeepEqual(q.Bases, []string{"hivecommons/hive#v5"}) {
		t.Fatalf("saved queue: %+v", q)
	}
	put(map[string]any{"probe_ttl": "0"}, http.StatusOK)
	if got := s.deps.Config.AutoMerge.NativeMergeQueue; !got.Enabled || !got.SkipCurrentBaseGate || got.EffectiveProbeTTL() != config.DefaultNativeMergeQueueProbeTTL || !reflect.DeepEqual(got.Bases, q.Bases) {
		t.Fatalf("partial update lost settings: %+v", got)
	}
	for _, invalid := range []map[string]any{
		{"repos": []string{}}, {"bases": []string{}}, {"repos": []string{"*"}},
		{"bases": []string{"hivecommons/hive#*"}}, {"bases": []string{"hivecommons/other#v5"}},
		{"bases": []string{"hivecommons/hive#../v5"}}, {"bases": []string{"hivecommons/hive#v5.lock"}},
		{"probe_ttl": "-1m"}, {"probe_ttl": "invalid"}, {"bases": []string{"v5"}},
	} {
		before := s.deps.Config.AutoMerge.NativeMergeQueue
		put(invalid, http.StatusBadRequest)
		if !reflect.DeepEqual(before, s.deps.Config.AutoMerge.NativeMergeQueue) {
			t.Fatalf("invalid update mutated config: %+v", invalid)
		}
	}
	put(map[string]any{"enabled": false}, http.StatusOK)
	if s.deps.Config.AutoMerge.NativeMergeQueue.Enabled {
		t.Fatal("disable failed")
	}
	s.deps.Config.AutoMerge.NativeMergeQueue = config.NativeMergeQueueAutoMergeConfig{}
	put(map[string]any{"enabled": true}, http.StatusBadRequest)
}

func TestNativeQueueSettingsOwnerOnlyAndPersistenceFailure(t *testing.T) {
	s := covApiServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/config/auto-merge", strings.NewReader(`{"native_merge_queue":{"enabled":true}}`))
	req.Header.Set("X-Hive-Role", config.RoleReadWrite)
	rec := httptest.NewRecorder()
	s.handleAutoMergePut(rec, req)
	if rec.Code != http.StatusForbidden || s.deps.Config.AutoMerge.NativeMergeQueue.Enabled {
		t.Fatalf("non-owner update: %d", rec.Code)
	}
	// Fail the save guard before any file writes.
	s.deps.Config.SourcePath = "unwritten-native-queue-settings.yaml"
	s.deps.Config.Project.Org = ""
	rec = doPut(s, "/api/config/auto-merge", map[string]any{"native_merge_queue": map[string]any{
		"enabled": true, "repos": []string{"hivecommons/hive"}, "bases": []string{"hivecommons/hive#v5"},
	}})
	if rec.Code != http.StatusInternalServerError || s.deps.Config.AutoMerge.NativeMergeQueue.Enabled {
		t.Fatalf("unpersisted queue change was accepted: %d: %s", rec.Code, rec.Body.String())
	}
}

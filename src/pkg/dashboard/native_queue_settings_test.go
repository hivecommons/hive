package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestAutoMergeResponseNativeQueueConfiguration(t *testing.T) {
	s := covApiServer(t)
	assertQueue := func(enabled, freshness bool, repos int, ttl string) {
		t.Helper()
		rec := doPut(s, "/api/config/auto-merge", map[string]any{})
		if rec.Code != http.StatusOK {
			t.Fatalf("auto-merge PUT: %d: %s", rec.Code, rec.Body.String())
		}

		var body struct {
			Queue struct {
				Enabled   bool     `json:"enabled"`
				Repos     []string `json:"repos"`
				Freshness bool     `json:"skip_current_base_gate"`
				TTL       string   `json:"probe_ttl"`
			} `json:"native_merge_queue"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		q := body.Queue
		if q.Enabled != enabled || q.Freshness != freshness || len(q.Repos) != repos || q.TTL != ttl || q.Repos == nil {
			t.Fatalf("native queue response: %+v", q)
		}
	}
	assertQueue(false, false, 0, config.DefaultNativeMergeQueueProbeTTL.String())
	s.deps.Config.AutoMerge.NativeMergeQueue = config.NativeMergeQueueAutoMergeConfig{
		Enabled: true, Repos: []string{"hivecommons/hive"},
		SkipCurrentBaseGate: true, ProbeTTL: time.Minute,
	}
	assertQueue(true, true, 1, time.Minute.String())
}

package spoke

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/nous"
)

func TestHeartbeatAppliesSelfExperimentFlags(t *testing.T) {
	for _, consent := range []string{"", "false", "TRUE", "true"} {
		t.Run("consent="+consent, func(t *testing.T) {
			t.Setenv("HIVE_SELF_EXPERIMENTS_OPT_IN", consent)
			resetSingleLoopGuard(t)
			collapseReadinessWait(t)
			nous.SelfExperiments.Apply(false, nil)
			t.Cleanup(func() { nous.SelfExperiments.Apply(false, nil) })

			var beats atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload HeartbeatPayload
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if payload.SelfExperimentsOptIn != (consent == "true") {
					t.Error("heartbeat did not report authoritative local opt-in")
				}
				n := beats.Add(1)
				resp := HeartbeatResponse{OK: true}
				if n <= 2 {
					// Even a hub sending a flag without consent cannot enable it.
					resp.SelfExperimentFlag = &nous.SelfFlagAssignment{
						ExperimentID: "exp-1", Flag: "review-v2", Enabled: n == 1,
					}
				}
				if err := json.NewEncoder(w).Encode(resp); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			processed := 0
			onBanner := HubBannerCallback(func(*HubBanner) {
				processed++
				want := consent == "true" && processed == 1
				if got := nous.SelfExperiments.Enabled("review-v2"); got != want {
					t.Errorf("heartbeat %d: flag=%v, want %v", processed, got, want)
				}
				if processed == 2 {
					// Re-enable before the third (old-hub/withdrawal) response to
					// ensure omission really revokes, not merely preserves off.
					nous.SelfExperiments.Apply(true, &nous.SelfFlagAssignment{
						ExperimentID: "exp-1", Flag: "review-v2", Enabled: true,
					})
				}
				if processed == 3 {
					cancel()
				}
			})
			// A stale collector cannot opt the hive in on its own, and the
			// consent overlay must not mutate its reusable payload.
			collected := &HeartbeatPayload{HiveID: "hive-a", SelfExperimentsOptIn: true}
			StartHeartbeat(ctx, srv.URL, func() *HeartbeatPayload {
				return collected
			}, 5*time.Millisecond, nil2Logger(), onBanner)
			if !collected.SelfExperimentsOptIn {
				t.Error("heartbeat mutated collector-owned payload")
			}
			if processed != 3 {
				t.Fatalf("processed %d heartbeats, want 3", processed)
			}
		})
	}
}

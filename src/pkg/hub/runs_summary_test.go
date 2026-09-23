package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSanitizeRunsSummaryClampsAndDropsInvalid(t *testing.T) {
	active, waiting := -1, 2
	oldest := maxRunWaitSeconds + 99
	completed := "2026-09-22T10:00:00Z"
	got := sanitizeRunsSummary(&RunsSummary{
		Active:               &active,
		WaitingOnHuman:       &waiting,
		OldestWaitSeconds:    &oldest,
		LastStageCompletedAt: &completed,
	})
	if got == nil {
		t.Fatal("sanitizeRunsSummary returned nil")
	}
	if got.Active != nil {
		t.Fatalf("active = %+v, want dropped negative", got.Active)
	}
	if got.WaitingOnHuman == nil || *got.WaitingOnHuman != waiting {
		t.Fatalf("waiting = %+v", got.WaitingOnHuman)
	}
	if got.OldestWaitSeconds == nil || *got.OldestWaitSeconds != maxRunWaitSeconds {
		t.Fatalf("oldest = %+v, want cap %d", got.OldestWaitSeconds, maxRunWaitSeconds)
	}
	if got.LastStageCompletedAt == nil || *got.LastStageCompletedAt != completed {
		t.Fatalf("completed = %+v", got.LastStageCompletedAt)
	}
}

func TestMyHiveEntryProjectsRunsWhenKnown(t *testing.T) {
	active, waiting := 3, 1
	row := MyHiveEntry{RegistryEntry: RegistryEntry{ID: "h", Runs: &RunsSummary{
		Active: &active, WaitingOnHuman: &waiting,
	}}}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"runs":{"active":3,"waiting_on_human":1}`) {
		t.Fatalf("json = %s, want runs projection", raw)
	}
	legacy, err := json.Marshal(MyHiveEntry{RegistryEntry: RegistryEntry{ID: "old"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), `"runs"`) {
		t.Fatalf("legacy json = %s, want no runs field", legacy)
	}
}

func TestHandleMyHivesProjectsRunsAndStalledFlag(t *testing.T) {
	const (
		token = "ghp_runs_projection"
		owner = "runs-owner"
	)
	cleanup := helperSetupAuthUser(t, token, owner)
	defer cleanup()
	SetFleetRunWaitAmberSeconds(3600)
	t.Cleanup(func() { SetFleetRunWaitAmberSeconds(0) })

	active, waiting := 4, 2
	oldest := int64(7200)
	srv := newHubServerForTest(t)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{
		ID:            "runs-hive",
		Name:          "acme/runs",
		Owner:         owner,
		Online:        true,
		ACMMLevel:     4,
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
		Runs: &RunsSummary{
			Active:            &active,
			WaitingOnHuman:    &waiting,
			OldestWaitSeconds: &oldest,
		},
	}}
	srv.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/saas/my-hives", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.handleMyHives(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleMyHives = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Hives []MyHiveEntry `json:"hives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Hives) != 1 {
		t.Fatalf("hives = %d, want 1 (%s)", len(resp.Hives), rec.Body.String())
	}
	got := resp.Hives[0]
	if got.Runs == nil || got.Runs.Active == nil || *got.Runs.Active != active ||
		got.Runs.WaitingOnHuman == nil || *got.Runs.WaitingOnHuman != waiting ||
		got.Runs.OldestWaitSeconds == nil || *got.Runs.OldestWaitSeconds != oldest {
		t.Fatalf("runs projection = %+v", got.Runs)
	}
	if !got.StalledRuns {
		t.Fatalf("stalledRuns = false, want true")
	}
}

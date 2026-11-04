package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/governor"
)

func TestGovernorPRModelsIncludesLiveFixerQueue(t *testing.T) {
	s := covApiServer(t)
	s.deps.Governor = &governor.Governor{}
	at := time.Now()
	s.deps.Governor.RecordFixKickDeferred("myorg/repo1#1", at)
	s.deps.Governor.RecordFixKickDeferred("myorg/repo1#2", at.Add(time.Second))

	get := func() governorPRModelsResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleGovernorPRModels(rec, httptest.NewRequest(http.MethodGet, "/api/governor/pr-models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var resp governorPRModelsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.FixerQueue) == 0 && strings.Contains(rec.Body.String(), `"fixer_queue"`) {
			t.Fatal("empty queue must be omitted")
		}
		return resp
	}
	// Queue visibility is independent of attribution and the model window.
	resp := get()
	if resp.Total != 0 || len(resp.FixerQueue) != 2 || resp.FixerQueue["myorg/repo1#2"].FixerQueuePosition != 2 {
		t.Fatalf("response = %+v", resp)
	}
	s.deps.Governor.RecordFixKickDelivered("myorg/repo1#1")
	second := get().FixerQueue["myorg/repo1#2"]
	if second.FixerQueuePosition != 1 || second.FixerQueueAhead == nil || *second.FixerQueueAhead != 0 {
		t.Fatalf("after delivery = %+v", second)
	}
	s.deps.Governor.RecordFixKickDelivered("myorg/repo1#2")
	get()
	s.deps.Governor = nil
	get()
}

func TestReviewQueueFixerFieldsAreOptionalAndKeepGlobalPosition(t *testing.T) {
	s := reviewQueueTestServer(t, filepath.Join(t.TempDir(), "missing.json"))
	s.deps.Governor = &governor.Governor{}
	at := time.Now()
	s.deps.Governor.RecordFixKickDeferred("myorg/other#99", at)
	s.deps.Governor.RecordFixKickDeferred("myorg/repo1#2", at.Add(time.Second))

	get := func() []map[string]json.RawMessage {
		t.Helper()
		rec := doOwnerGet(s, "/api/review/queue?limit=1")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Items
	}
	items := get()
	if len(items) != 1 || string(items[0]["number"]) != "2" || string(items[0]["fixer_queue_position"]) != "2" || string(items[0]["fixer_queue_ahead"]) != "1" {
		t.Fatalf("paged items = %+v", items)
	}
	s.deps.Governor.RecordFixKickDelivered("myorg/other#99")
	items = get()
	if string(items[0]["fixer_queue_position"]) != "1" || string(items[0]["fixer_queue_ahead"]) != "0" {
		t.Fatalf("first queued item must explicitly emit zero ahead: %+v", items)
	}
	s.deps.Governor.RecordFixKickDelivered("myorg/repo1#2")
	items = get()
	if _, ok := items[0]["fixer_queue_position"]; ok {
		t.Fatal("delivered item still emits fixer_queue_position")
	}
	if _, ok := items[0]["fixer_queue_ahead"]; ok {
		t.Fatal("delivered item still emits fixer_queue_ahead")
	}
}

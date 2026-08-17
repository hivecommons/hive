package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleReach_EmptySafe(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/reach", nil)
	rr := httptest.NewRecorder()

	s.handleReach(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	var resp reachResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode reach response: %v", err)
	}

	if resp.HivesReporting != 0 {
		t.Errorf("HivesReporting = %d, want 0", resp.HivesReporting)
	}
	if len(resp.Reports) != 0 {
		t.Errorf("len(Reports) = %d, want 0", len(resp.Reports))
	}
}

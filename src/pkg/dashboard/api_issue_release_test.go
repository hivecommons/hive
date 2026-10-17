package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRepoIssueReleaseUsesRepositoryWriteGate(t *testing.T) {
	s := &Server{}
	request := func(owner bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/repos/o/r/issues/7/release", nil)
		req.SetPathValue("owner", "o")
		req.SetPathValue("repo", "r")
		req.SetPathValue("number", "7")
		if owner {
			markOwnerRequest(req)
		}
		rec := httptest.NewRecorder()
		s.handleRepoIssueRelease(rec, req)
		return rec
	}

	if rec := request(false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous release = %d, want 401", rec.Code)
	}
	if rec := request(true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("verified owner did not pass the write gate: status = %d, want downstream 503", rec.Code)
	}
}

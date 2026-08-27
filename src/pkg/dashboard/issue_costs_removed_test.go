package dashboard

import (
	"bytes"
	"net/http"
	"testing"
)

func TestIssueCostsFeatureIsNotAdvertised(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doGet(s, "/api/issue-costs"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/issue-costs status = %d, want 404", rec.Code)
	}

	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded dashboard: %v", err)
	}
	if bytes.Contains(index, []byte("/api/issue-costs")) {
		t.Fatal("embedded dashboard still fetches the removed per-issue cost endpoint")
	}
}

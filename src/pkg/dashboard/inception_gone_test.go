package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every former /api/inception/* route answers 410 Gone and points to
// Inception (spek) sessions (the inception API was retired).
func TestInceptionRoutesAnswerGone(t *testing.T) {
	s := covApiServer(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/inception/start"},
		{http.MethodPost, "/api/inception/scan"},
		{http.MethodGet, "/api/inception/state"},
		{http.MethodPost, "/api/inception/questions"},
		{http.MethodPost, "/api/inception/answer"},
		{http.MethodPost, "/api/inception/facts"},
		{http.MethodGet, "/api/inception/scaffold"},
		{http.MethodPost, "/api/inception/approve"},
		{http.MethodPost, "/api/inception/reset"},
		{http.MethodGet, "/api/inception/ideation-facts"},
		{http.MethodGet, "/api/inception/download"},
		{http.MethodGet, "/api/inception/has-files"},
		{http.MethodPut, "/api/inception/wiki-name"},
		{http.MethodPost, "/api/inception/import"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, nil)
		markOwnerRequest(req)
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusGone {
			t.Fatalf("%s %s = %d, want 410", c.method, c.path, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s body: %v", c.method, c.path, err)
		}
		if body["error"] != inceptionGoneMessage || body["replacement"] != inceptionGoneReplacement || body["docs"] != inceptionGoneDocs {
			t.Fatalf("%s %s body = %v", c.method, c.path, body)
		}
	}
}

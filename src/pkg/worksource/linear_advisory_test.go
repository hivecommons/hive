package worksource_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/worksource"
)

// advisoryGQL is a fake Linear GraphQL endpoint that serves one issue with a
// configurable comment list and records every mutation it receives.
type advisoryGQL struct {
	t        *testing.T
	mu       sync.Mutex
	comments []map[string]string // id/body pairs returned by the issue query
	issueID  string              // empty => issue lookup returns null
	authSeen string
	creates  []string // bodies passed to commentCreate
	updates  []string // "id|body" passed to commentUpdate
}

func (f *advisoryGQL) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.t.Errorf("decode: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.authSeen = r.Header.Get("Authorization")
		var resp map[string]interface{}
		switch {
		case strings.Contains(req.Query, "commentCreate"):
			f.creates = append(f.creates, req.Variables["body"].(string))
			if req.Variables["issueId"] != f.issueID {
				f.t.Errorf("commentCreate issueId = %v, want %q", req.Variables["issueId"], f.issueID)
			}
			resp = map[string]interface{}{"data": map[string]interface{}{"commentCreate": map[string]interface{}{"success": true, "comment": map[string]string{"id": "new"}}}}
		case strings.Contains(req.Query, "commentUpdate"):
			f.updates = append(f.updates, req.Variables["id"].(string)+"|"+req.Variables["body"].(string))
			resp = map[string]interface{}{"data": map[string]interface{}{"commentUpdate": map[string]interface{}{"success": true}}}
		default:
			var issue interface{}
			if f.issueID != "" {
				nodes := make([]map[string]string, 0, len(f.comments))
				nodes = append(nodes, f.comments...)
				issue = map[string]interface{}{"id": f.issueID, "identifier": req.Variables["id"], "comments": map[string]interface{}{"nodes": nodes}}
			}
			resp = map[string]interface{}{"data": map[string]interface{}{"issue": issue}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestLinearAdvisoryPoster_CreatesWhenNoMarkerComment(t *testing.T) {
	f := &advisoryGQL{t: t, issueID: "uuid-1", comments: []map[string]string{{"id": "c1", "body": "a human comment"}}}
	srv := f.serve()
	defer srv.Close()

	p := worksource.NewLinearAdvisoryPoster("lin_api_xyz", srv.URL, srv.Client())
	if err := p.PostDigest(context.Background(), "ONB-123", "## 🐝 Advisory Digest\n\n- finding"); err != nil {
		t.Fatalf("PostDigest: %v", err)
	}
	if f.authSeen != "lin_api_xyz" {
		t.Errorf("Authorization = %q, want the bare api key exactly as the work source sends it", f.authSeen)
	}
	if len(f.creates) != 1 || len(f.updates) != 0 {
		t.Fatalf("creates=%d updates=%d, want 1 create and 0 updates", len(f.creates), len(f.updates))
	}
	if !strings.Contains(f.creates[0], worksource.LinearAdvisoryMarker) || !strings.HasPrefix(f.creates[0], "## 🐝 Advisory Digest") {
		t.Errorf("created body missing digest or marker: %q", f.creates[0])
	}
}

func TestLinearAdvisoryPoster_UpdatesExistingMarkerComment(t *testing.T) {
	f := &advisoryGQL{t: t, issueID: "uuid-1", comments: []map[string]string{
		{"id": "c1", "body": "a human comment"},
		{"id": "c2", "body": "old digest\n\n" + worksource.LinearAdvisoryMarker},
	}}
	srv := f.serve()
	defer srv.Close()

	p := worksource.NewLinearAdvisoryPoster("k", srv.URL, srv.Client())
	if err := p.PostDigest(context.Background(), "ONB-123", "new digest"); err != nil {
		t.Fatalf("PostDigest: %v", err)
	}
	if len(f.creates) != 0 || len(f.updates) != 1 {
		t.Fatalf("creates=%d updates=%d, want 0 creates and 1 update", len(f.creates), len(f.updates))
	}
	if !strings.HasPrefix(f.updates[0], "c2|new digest") {
		t.Errorf("update = %q, want the marker comment c2 rewritten with the new body", f.updates[0])
	}
}

func TestLinearAdvisoryPoster_SkipsUnchangedBody(t *testing.T) {
	body := "same digest\n\n" + worksource.LinearAdvisoryMarker
	f := &advisoryGQL{t: t, issueID: "uuid-1", comments: []map[string]string{{"id": "c2", "body": body}}}
	srv := f.serve()
	defer srv.Close()

	p := worksource.NewLinearAdvisoryPoster("k", srv.URL, srv.Client())
	if err := p.PostDigest(context.Background(), "ONB-123", "same digest\n"); err != nil {
		t.Fatalf("PostDigest: %v", err)
	}
	if len(f.creates)+len(f.updates) != 0 {
		t.Fatalf("unchanged digest must not write; creates=%d updates=%d", len(f.creates), len(f.updates))
	}
}

// TestLinearAdvisoryPoster_FailsClosed pins that a missing issue identifier,
// a missing key, or an unknown issue is an ERROR — the poster never touches
// the network for the first two and never falls back to anything.
func TestLinearAdvisoryPoster_FailsClosed(t *testing.T) {
	f := &advisoryGQL{t: t} // issueID empty => issue(null)
	srv := f.serve()
	defer srv.Close()

	p := worksource.NewLinearAdvisoryPoster("k", srv.URL, srv.Client())
	err := p.PostDigest(context.Background(), "  ", "digest")
	if !errors.Is(err, worksource.ErrLinearAdvisoryIssueUnset) {
		t.Fatalf("empty identifier: err = %v, want ErrLinearAdvisoryIssueUnset", err)
	}
	if !strings.Contains(err.Error(), "governor.advisory.linear_issue") {
		t.Errorf("error must name the missing key, got %q", err)
	}
	if f.authSeen != "" {
		t.Error("empty identifier must not reach the API")
	}

	noKey := worksource.NewLinearAdvisoryPoster("", srv.URL, srv.Client())
	if err := noKey.PostDigest(context.Background(), "ONB-1", "digest"); !errors.Is(err, worksource.ErrLinearAdvisoryAPIKeyUnset) {
		t.Fatalf("empty api key: err = %v, want ErrLinearAdvisoryAPIKeyUnset", err)
	}

	err = p.PostDigest(context.Background(), "ONB-404", "digest")
	if err == nil || !strings.Contains(err.Error(), "ONB-404 not found") {
		t.Fatalf("unknown issue: err = %v, want a not-found error", err)
	}
	if len(f.creates)+len(f.updates) != 0 {
		t.Error("unknown issue must not create or update anything")
	}
}

func TestLinearAdvisoryPoster_SurfacesGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"errors": []map[string]string{{"message": "Authentication required"}}})
	}))
	defer srv.Close()
	p := worksource.NewLinearAdvisoryPoster("bad", srv.URL, srv.Client())
	err := p.PostDigest(context.Background(), "ONB-1", "digest")
	if err == nil || !strings.Contains(err.Error(), "Authentication required") {
		t.Fatalf("err = %v, want the GraphQL error message surfaced", err)
	}
}

package trajectory

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrajectoryReviewerAcceptsV1Endpoint(t *testing.T) {
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	t.Cleanup(srv.Close)

	for _, endpoint := range []string{srv.URL, srv.URL + "/v1", srv.URL + "/v1/"} {
		lastPath = ""
		r, err := NewReviewer(Config{Endpoint: endpoint, Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = r.Review(context.Background(), "agent", "intent", "transcript")
		if lastPath != "/v1/chat/completions" {
			t.Errorf("endpoint %q: request path %q, want /v1/chat/completions", endpoint, lastPath)
		}
	}
}

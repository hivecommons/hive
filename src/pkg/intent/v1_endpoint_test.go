package intent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAlignmentReviewerAcceptsV1Endpoint(t *testing.T) {
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
		r, err := NewAlignmentReviewer(AlignmentReviewerConfig{Endpoint: endpoint, Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = r.reviewUncached(context.Background(), AlignmentContext{AuthorizedIntent: "intent"})
		if lastPath != "/v1/chat/completions" {
			t.Errorf("endpoint %q: request path %q, want /v1/chat/completions", endpoint, lastPath)
		}
	}
}

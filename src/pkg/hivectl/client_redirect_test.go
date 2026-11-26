package hivectl

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A 307 from the Hive server must not carry a POST body (here a PAT) to
// another host: the client stops at the redirect instead of following it.
func TestClientDoesNotResendBodyOnRedirect(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	var leaked atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		data, _ := io.ReadAll(r.Body)
		leaked.Store(string(data))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/github-token", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := NewClient(origin.URL, "secret", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(context.Background(), http.MethodPut, "/api/github-token", nil, map[string]string{"token": "ghp_redirect"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v, want APIError 307", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("redirect target received %d request(s), body %q", n, leaked.Load())
	}
}

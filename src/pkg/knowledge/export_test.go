package knowledge

import (
	"net/http"
	"net/url"
	"sync"
	"testing"
)

var context7TestMu sync.Mutex

// useContext7TestServer routes production Context7 URLs to an httptest server.
// It serializes client swaps so these tests remain safe if a future subtest is
// marked parallel.
func useContext7TestServer(t testing.TB, rawURL string) {
	t.Helper()
	target, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse Context7 test server URL: %v", err)
	}
	useContext7Client(t, &http.Client{
		Timeout:       context7Timeout,
		CheckRedirect: knowledgeNoRedirectToPrivate,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "context7.com" {
				return http.DefaultTransport.RoundTrip(req)
			}
			clone := req.Clone(req.Context())
			rewritten := *clone.URL
			rewritten.Scheme = target.Scheme
			rewritten.Host = target.Host
			clone.URL = &rewritten
			clone.Host = ""
			return http.DefaultTransport.RoundTrip(clone)
		}),
	})
}

func useContext7Client(t testing.TB, client *http.Client) {
	t.Helper()
	context7TestMu.Lock()
	old := context7Client
	context7Client = client
	t.Cleanup(func() {
		context7Client = old
		context7TestMu.Unlock()
	})
}

package knowledge

// Shared test helpers for the curator test files. These previously lived in
// maturity_extra_test.go and were kept when the dead MaturityDetector code
// was removed.

import (
	"net/http/httptest"

	gh "github.com/google/go-github/v72/github"
)

func ghClientFromServer(server *httptest.Server) *gh.Client {
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")
	return client
}

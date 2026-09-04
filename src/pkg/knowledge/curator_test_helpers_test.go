package knowledge

// Shared test helpers for the curator test files. These previously lived with
// the removed maturity-detector tests.

import (
	"net/http/httptest"

	gh "github.com/google/go-github/v72/github"
)

func ghClientFromServer(server *httptest.Server) *gh.Client {
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")
	return client
}

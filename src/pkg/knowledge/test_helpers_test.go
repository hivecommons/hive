package knowledge

import (
	"net/http/httptest"

	gh "github.com/google/go-github/v72/github"
)

func ghClientFromServer(server *httptest.Server) *gh.Client {
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")
	return client
}

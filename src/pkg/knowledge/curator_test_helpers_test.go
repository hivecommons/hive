package knowledge

// Shared test helpers for the curator test files. These previously lived in
// maturity_extra_test.go and were kept when the dead MaturityDetector code
// was removed.

import (
	"log/slog"
	"net/http/httptest"
	"os"

	gh "github.com/google/go-github/v72/github"
)

func maturityTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func ghClientFromServer(server *httptest.Server) *gh.Client {
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")
	return client
}

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

type integrationReadOnlyKey struct{}

const integrationReadOnlyMaxQuery = 1 << 20

var integrationMutationOperation = regexp.MustCompile(`\bmutation\b`)

// WithIntegrationReadOnly prevents ancillary legacy lane helpers from writing
// comments, approving held runs, changing labels or invoking direct transports.
// The shared REST/GraphQL transport enforces it in addition to the effect guard.
func WithIntegrationReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, integrationReadOnlyKey{}, true)
}

func integrationReadOnlyRequest(request *http.Request) error {
	if guarded, _ := request.Context().Value(integrationReadOnlyKey{}).(bool); !guarded {
		return nil
	}
	if (request.Method == http.MethodGet || request.Method == http.MethodHead) &&
		!strings.HasSuffix(request.URL.Path, "/graphql") {
		return nil
	}
	denied := fmt.Errorf("read-only integration lane probe refuses %s %s", request.Method, request.URL.Path)
	if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/graphql") || request.Body == nil {
		return denied
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, integrationReadOnlyMaxQuery+1))
	if err != nil {
		return err
	}
	if len(body) > integrationReadOnlyMaxQuery {
		return denied
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	var operation struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(body, &operation) != nil {
		return denied
	}
	query := strings.TrimSpace(operation.Query)
	// Conservative rejection of the keyword also rejects harmless occurrences
	// in literals/comments; an uncertain operation must never become a write.
	if query == "" || integrationMutationOperation.MatchString(query) ||
		!strings.HasPrefix(query, "query") && !strings.HasPrefix(query, "{") {
		return denied
	}
	return nil
}

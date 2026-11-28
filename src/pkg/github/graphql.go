package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// graphQLEndpoint maps a REST base URL to its GraphQL endpoint:
// https://api.github.com/ → https://api.github.com/graphql;
// https://ghe.example/api/v3/ → https://ghe.example/api/graphql;
// anything else (a test server) → <base>graphql.
func graphQLEndpoint(base *url.URL) string {
	if base == nil {
		return "https://api.github.com/graphql"
	}
	if strings.EqualFold(base.Host, "api.github.com") {
		return base.Scheme + "://" + base.Host + "/graphql"
	}
	if strings.HasSuffix(strings.TrimSuffix(base.Path, "/"), "/api/v3") {
		return base.Scheme + "://" + base.Host + "/api/graphql"
	}
	return strings.TrimSuffix(base.String(), "/") + "/graphql"
}

// graphQLError is one entry of a GraphQL response's "errors" array.
type graphQLError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
}

// graphQLErrors is the error returned when the endpoint answered 200 with an
// errors array — the GraphQL way of saying "not found", "forbidden", or
// "already resolved". Callers that want a specific verdict inspect Messages.
type graphQLErrors struct {
	Errors []graphQLError
}

func (e *graphQLErrors) Error() string {
	msgs := make([]string, 0, len(e.Errors))
	for _, ge := range e.Errors {
		if ge.Type != "" {
			msgs = append(msgs, ge.Type+": "+ge.Message)
			continue
		}
		msgs = append(msgs, ge.Message)
	}
	return "graphql: " + strings.Join(msgs, "; ")
}

// graphQL posts one query/mutation to the forge's GraphQL endpoint THROUGH
// the go-github client, so it rides the same authenticated, proxy-trusting
// transport (App installation token, GHE base URL, rate-limit accounting) as
// every REST call. go-github has no GraphQL surface of its own; the request
// is built with NewRequest so the auth transport sees it like any other.
// The "data" object is decoded into out; a non-empty "errors" array is
// returned as *graphQLErrors even when data is partially populated.
func (c *Client) graphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	payload := map[string]any{"query": query}
	if len(vars) > 0 {
		payload["variables"] = vars
	}
	req, err := c.client.NewRequest("POST", graphQLEndpoint(c.client.BaseURL), payload)
	if err != nil {
		return fmt.Errorf("building graphql request: %w", err)
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	if _, err := c.client.Do(ctx, req, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		return &graphQLErrors{Errors: resp.Errors}
	}
	if out != nil && len(resp.Data) > 0 && string(resp.Data) != "null" {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return fmt.Errorf("decoding graphql data: %w", err)
		}
	}
	return nil
}

// isGraphQLNotFound reports whether err is a GraphQL "could not resolve to a
// node" style failure — the node id names nothing this token can see.
func isGraphQLNotFound(err error) bool {
	var ge *graphQLErrors
	if !errors.As(err, &ge) {
		return false
	}
	for _, e := range ge.Errors {
		if strings.EqualFold(e.Type, "NOT_FOUND") || strings.Contains(strings.ToLower(e.Message), "could not resolve") {
			return true
		}
	}
	return false
}

// graphQLRefusalTypes are the GraphQL error types GitHub answers with when
// it turned a request down before changing anything: the input did not
// pass GitHub's own rules, the token may not do it, or the node is not
// there.
var graphQLRefusalTypes = map[string]bool{graphQLTypeUnprocessable: true, "FORBIDDEN": true, "NOT_FOUND": true}

// graphQLTypeUnprocessable is the type of a request GitHub understood and
// one of its rules for the object turned down.
const graphQLTypeUnprocessable = "UNPROCESSABLE"

// graphQLErrorsAllTyped reports whether err is a GraphQL errors array, not
// empty, whose every entry carries a type in types.
func graphQLErrorsAllTyped(err error, types map[string]bool) bool {
	var ge *graphQLErrors
	if !errors.As(err, &ge) || len(ge.Errors) == 0 {
		return false
	}
	for _, e := range ge.Errors {
		if !types[strings.ToUpper(strings.TrimSpace(e.Type))] {
			return false
		}
	}
	return true
}

// IsGraphQLRefusal reports whether err is GitHub definitely refusing a
// GraphQL mutation: the endpoint answered with an errors array and every
// entry carries one of graphQLRefusalTypes, so nothing was applied. A
// network failure, a 5xx, an entry with no type (an internal error) and a
// rate limit are not: the caller cannot tell from them whether the mutation
// took effect. The type says that GitHub refused, not why; a caller that
// needs the cause matches the message.
func IsGraphQLRefusal(err error) bool {
	return graphQLErrorsAllTyped(err, graphQLRefusalTypes)
}

// IsGraphQLUnprocessable reports whether err is the narrower refusal: every
// entry of the errors array is typed UNPROCESSABLE, so GitHub turned the
// request down on one of its rules for the object (a branch rule the pull
// request does not meet, say), not because the token may not make it
// (FORBIDDEN) or the node is not there (NOT_FOUND).
func IsGraphQLUnprocessable(err error) bool {
	return graphQLErrorsAllTyped(err, map[string]bool{graphQLTypeUnprocessable: true})
}

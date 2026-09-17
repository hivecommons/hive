package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// graphQLEndpoint derives the GraphQL URL from a go-github REST base URL.
// github.com serves REST at https://api.github.com/ and GraphQL at
// https://api.github.com/graphql; GitHub Enterprise serves REST at
// https://ghe/api/v3/ and GraphQL at https://ghe/api/graphql. Everything
// else (test servers, unusual proxies) gets "<base>graphql".
func graphQLEndpoint(base *url.URL) string {
	if base == nil {
		return "https://api.github.com/graphql"
	}
	u := *base
	switch {
	case strings.HasSuffix(u.Path, "/api/v3/"):
		u.Path = strings.TrimSuffix(u.Path, "v3/") + "graphql"
	default:
		u.Path = strings.TrimSuffix(u.Path, "/") + "/graphql"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
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

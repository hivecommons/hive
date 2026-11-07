package api

import (
	"context"
	"net/http"
)

// CreateRun is POST /v1/runs: the saved record, final unless req.Detach.
func (c *Client) CreateRun(ctx context.Context, req ContainedRunRequest) (ContainedRun, error) {
	var out ContainedRun
	err := c.Do(ctx, http.MethodPost, PathRuns, req, &out)
	return out, err
}

// GetRun is GET /v1/runs/{id}.
func (c *Client) GetRun(ctx context.Context, id string) (ContainedRun, error) {
	var out ContainedRun
	err := c.Do(ctx, http.MethodGet, RunPath(id), nil, &out)
	return out, err
}

// CancelRun is POST /v1/runs/{id}/cancel.
func (c *Client) CancelRun(ctx context.Context, id string) (ContainedRun, error) {
	var out ContainedRun
	err := c.Do(ctx, http.MethodPost, RunCancelPath(id), nil, &out)
	return out, err
}

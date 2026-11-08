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

// ListRuns is GET /v1/runs.
func (c *Client) ListRuns(ctx context.Context, status string, limit int) (ContainedRunList, error) {
	var out ContainedRunList
	err := c.Do(ctx, http.MethodGet, RunsListQuery(status, limit), nil, &out)
	return out, err
}

// RestartRun is POST /v1/runs/{id}/restart: the new run's record, final
// unless req.Detach.
func (c *Client) RestartRun(ctx context.Context, id string, req RunRestartRequest) (ContainedRun, error) {
	var out ContainedRun
	err := c.Do(ctx, http.MethodPost, RunRestartPath(id), req, &out)
	return out, err
}

// RunHistory is GET /v1/runs/{id}/history.
func (c *Client) RunHistory(ctx context.Context, id string) (RunHistory, error) {
	var out RunHistory
	err := c.Do(ctx, http.MethodGet, RunHistoryPath(id), nil, &out)
	return out, err
}

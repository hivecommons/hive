package main

import (
	"context"
	"errors"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/hivectl"
)

// readDiag serves one diagnostic read (hivecommons/hive#11943) from the
// active hive's /api/diag routes. The hive scrubs and bounds each result, so
// CapResult and the shared limit do not apply. A refusal becomes the same
// typed error the in-hive endpoint returns, without the hive's response body.
func (p readProvider) readDiag(ctx context.Context, tool string, args map[string]any) (any, error) {
	path, err := adminmcp.DiagReadPath(tool, args)
	if err != nil {
		return nil, err
	}
	client, _, err := p.roster.activeClient()
	if err != nil {
		return nil, err
	}
	data, err := getPath(ctx, client, path)
	var api *hivectl.APIError
	if errors.As(err, &api) {
		return nil, adminmcp.DiagHTTPError(tool, api.StatusCode)
	}
	return data, err
}

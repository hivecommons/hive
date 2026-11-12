package client

import (
	"context"
	"net/http"

	"github.com/hivecommons/hive/crux/api"
)

// RestartAfterModelChange asks the remote runtime to restart the agent, lifting
// a model-rejected pause if that is what parked it (#17828).
func (r *Runtime) RestartAfterModelChange(ctx context.Context, name, by string) error {
	return r.callCtx(ctx, http.MethodPost, api.RestartPath(esc(name)), api.RestartRequest{ModelChange: true, By: by}, nil)
}

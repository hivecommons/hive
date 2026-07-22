// Package checkpoint provides an optional fail-closed state barrier before
// externally visible mutations. Local execution installs no callback. The
// hosted controller binds one callback that durably commits its portable state
// branch before GitHub API writes or Git pushes can occur.
package checkpoint

import (
	"context"
	"fmt"
	"net/http"
)

type Callback func(context.Context, string) error

type contextKey struct{}

func WithCallback(ctx context.Context, callback Callback) context.Context {
	if callback == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, callback)
}

func BeforeMutation(ctx context.Context, detail string) error {
	callback, _ := ctx.Value(contextKey{}).(Callback)
	if callback == nil {
		return nil
	}
	if err := callback(ctx, detail); err != nil {
		return fmt.Errorf("durable hosted checkpoint before %s: %w", detail, err)
	}
	return nil
}

func Active(ctx context.Context) bool {
	callback, _ := ctx.Value(contextKey{}).(Callback)
	return callback != nil
}

type Transport struct {
	Base http.RoundTripper
}

func (transport Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := transport.Base
	if base == nil {
		base = http.DefaultTransport
	}
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if err := BeforeMutation(request.Context(), "GitHub API "+request.Method); err != nil {
			return nil, err
		}
	}
	return base.RoundTrip(request)
}

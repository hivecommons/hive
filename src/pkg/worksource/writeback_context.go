package worksource

import (
	"context"
	"strings"
)

type solicitedWritebackKey struct{}

type solicitedWriteback struct {
	target string
	actor  string
}

// WithSolicitedWriteback carries a verified request for one source item. Only
// trusted admission paths, or callbacks restoring their persisted provenance,
// may mint it; accepting work or running an agent alone is not solicitation.
func WithSolicitedWriteback(ctx context.Context, ref Ref, actor string) context.Context {
	if ref.Key() == "" || strings.TrimSpace(actor) == "" {
		return ctx
	}
	return context.WithValue(ctx, solicitedWritebackKey{}, solicitedWriteback{target: ref.Key(), actor: actor})
}

// SolicitedWritebackActor returns the verified requester only for the exact
// target carried by WithSolicitedWriteback.
func SolicitedWritebackActor(ctx context.Context, ref Ref) string {
	request, _ := ctx.Value(solicitedWritebackKey{}).(solicitedWriteback)
	if ref.Key() == "" || request.target != ref.Key() {
		return ""
	}
	return request.actor
}

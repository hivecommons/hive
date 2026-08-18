package integrated

import (
	"context"

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

// visualHiveGitHubClients contains the two narrow capabilities delegated to
// the optional Visual Hive GitHub App. All repository reads and every issue,
// branch, commit, pull-request, and repair mutation continue through the core
// Hive client supplied to the lifecycle operation itself.
type visualHiveGitHubClients struct {
	workflow *hivegithub.Client
	status   *hivegithub.Client
}

type visualHiveGitHubClientsContextKey struct{}

// WithVisualHiveGitHubClients binds optional least-privilege clients to one
// lifecycle call. Nil capabilities deliberately fall back to the core client,
// preserving standalone PAT and legacy single-App compatibility.
func WithVisualHiveGitHubClients(ctx context.Context, workflow, status *hivegithub.Client) context.Context {
	if workflow == nil && status == nil {
		return ctx
	}
	return context.WithValue(ctx, visualHiveGitHubClientsContextKey{}, visualHiveGitHubClients{workflow: workflow, status: status})
}

func visualHiveWorkflowClient(ctx context.Context, fallback *hivegithub.Client) *hivegithub.Client {
	if clients, ok := ctx.Value(visualHiveGitHubClientsContextKey{}).(visualHiveGitHubClients); ok && clients.workflow != nil {
		return clients.workflow
	}
	return fallback
}

func hasVisualHiveWorkflowClient(ctx context.Context) bool {
	clients, ok := ctx.Value(visualHiveGitHubClientsContextKey{}).(visualHiveGitHubClients)
	return ok && clients.workflow != nil
}

func visualHiveStatusClient(ctx context.Context, fallback *hivegithub.Client) *hivegithub.Client {
	if clients, ok := ctx.Value(visualHiveGitHubClientsContextKey{}).(visualHiveGitHubClients); ok && clients.status != nil {
		return clients.status
	}
	return fallback
}

func hasVisualHiveStatusClient(ctx context.Context) bool {
	clients, ok := ctx.Value(visualHiveGitHubClientsContextKey{}).(visualHiveGitHubClients)
	return ok && clients.status != nil
}

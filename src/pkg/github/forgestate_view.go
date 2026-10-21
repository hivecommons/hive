package github

import (
	"context"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
)

// ForgeStateView is the attached state cache addressed by "owner/name", the
// spelling webhook payloads and the automerge lanes use, regardless of how
// the repo was configured (bare name under the org, or owner/name). It is
// the automerge OpenPRSource (hivecommons/hive#13377); a repo the store does
// not hold reads as absent.
type ForgeStateView struct {
	store  *forgestate.Store
	client *Client
}

// ForgeStateView returns the owner/name view over the attached cache, nil
// when the client lists GitHub directly.
func (c *Client) ForgeStateView() *ForgeStateView {
	store := c.ForgeState()
	if store == nil {
		return nil
	}
	return &ForgeStateView{store: store, client: c}
}

// Store returns the underlying cache.
func (v *ForgeStateView) Store() *forgestate.Store { return v.store }

func (v *ForgeStateView) key(repo string) string {
	return forgeRepoKey(v.store, v.client, repo)
}

// Snapshot is forgestate.Store.Snapshot keyed by owner/name.
func (v *ForgeStateView) Snapshot(repo string) (forgestate.Snapshot, bool) {
	key := v.key(repo)
	if key == "" {
		return forgestate.Snapshot{}, false
	}
	return v.store.Snapshot(key)
}

// ReadThrough is forgestate.Store.ReadThrough keyed by owner/name.
func (v *ForgeStateView) ReadThrough(ctx context.Context, repo string, k forgestate.Kind, n int, bound time.Duration) (forgestate.Item, bool, error) {
	key := v.key(repo)
	if key == "" {
		return forgestate.Item{}, false, nil
	}
	return v.store.ReadThrough(ctx, key, k, n, bound)
}

// ApplyWebhook is forgestate.Store.ApplyWebhook keyed by owner/name; a repo
// the store does not hold is ignored.
func (v *ForgeStateView) ApplyWebhook(ctx context.Context, repo string, ev forgestate.WebhookEvent) error {
	key := v.key(repo)
	if key == "" {
		return nil
	}
	return v.store.ApplyWebhook(ctx, key, ev)
}

// WaitReady is forgestate.Store.WaitReady.
func (v *ForgeStateView) WaitReady(ctx context.Context) error { return v.store.WaitReady(ctx) }

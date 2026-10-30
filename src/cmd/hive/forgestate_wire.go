package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

// forgeStateOpenPRs returns the automerge lanes' view over the state cache
// (hivecommons/hive#13377) once applyGitHubClientForgeState attached it to
// the App client, and the trigger webhook deliveries notify. nil with the
// cache off: the lanes keep their listing ticker. The view is built once
// because the store is (forgeStateOnce); a rebuilt client re-attaches the
// same store.
func (b *boot) forgeStateOpenPRs() *github.ForgeStateView {
	if b.forgeView != nil {
		return b.forgeView
	}
	if b.ghClient == nil {
		return nil
	}
	view := b.ghClient.ForgeStateView()
	if view == nil {
		// No cache: the lanes run their listing ticker, but a trigger is
		// still wired so a webhook CI-failure nudge runs a targeted pass
		// at once (the legacy loop selects on it too).
		if b.sweepTrigger == nil {
			b.sweepTrigger = automerge.NewSweepTrigger(nil)
		}
		return nil
	}
	b.forgeView = view
	b.sweepTrigger = automerge.NewSweepTrigger(view)
	return view
}

// webhookDeliveryFunc is the dashboard.Dependencies.WebhookDelivery hook:
// patch the forgestate item the delivery names, then schedule an automerge
// pass for the PRs it affected. The cache is resolved per delivery because
// this closure is wired before the App client (and so the store) exists;
// with the cache off every delivery is a no-op.
func (b *boot) webhookDeliveryFunc() func(event, deliveryID string, payload []byte, res github.WebhookEventResult) {
	return func(event, deliveryID string, payload []byte, res github.WebhookEventResult) {
		view := b.forgeView
		trigger := b.sweepTrigger
		if view == nil || trigger == nil {
			return
		}
		go applyForgeStateDelivery(b.ctx, view, trigger, b.logger, event, deliveryID, payload, res)
	}
}

// sweepNudgeFunc is the dashboard.Dependencies.SweepNudge hook: schedule
// an automerge pass for one PR. The trigger is resolved per call because
// it is wired before the App client exists; before it is, the governor
// loop is woken instead, whose eval cycle runs the lanes.
func (b *boot) sweepNudgeFunc() func(repo string, number int) {
	return func(repo string, number int) {
		if trigger := b.sweepTrigger; trigger != nil {
			trigger.NotifyPullRequests(repo, number)
			return
		}
		if wake := b.webhookWakeFunc(); wake != nil {
			wake(fmt.Sprintf("operator-action %s#%d", repo, number))
		}
	}
}

// automergeTriggerEvents are the deliveries that can change whether a PR is
// mergeable: its checks, commit statuses, reviews and the PR itself.
var automergeTriggerEvents = map[string]bool{
	"check_suite":         true,
	"check_run":           true,
	"status":              true,
	"pull_request_review": true,
	"pull_request":        true,
}

// forgeStateDeliverySink is what applyForgeStateDelivery needs from the
// cache view; tests substitute it.
type forgeStateDeliverySink interface {
	ApplyWebhook(ctx context.Context, repo string, ev forgestate.WebhookEvent) error
}

// applyForgeStateDelivery is one delivery's work, off the webhook handler's
// goroutine: the cache patch is a GitHub read and the sweep pass must see
// the patched item.
func applyForgeStateDelivery(ctx context.Context, view forgeStateDeliverySink, trigger *automerge.SweepTrigger, logger *slog.Logger, event, deliveryID string, payload []byte, res github.WebhookEventResult) {
	if !res.KnownRepo {
		return
	}
	// The view and the trigger are keyed by owner/name (res.Repo, the
	// payload's full_name); the configured name may be bare.
	repo := res.Repo
	if _, ev, ok, err := forgestate.ParseWebhook(event, deliveryID, payload); err != nil {
		logger.Warn("forgestate webhook patch skipped", "event", event, "delivery", deliveryID, "error", err)
	} else if ok {
		if err := view.ApplyWebhook(ctx, repo, ev); err != nil {
			logger.Warn("forgestate webhook patch failed", "event", event, "delivery", deliveryID, "repo", repo, "number", ev.Number, "error", err)
		}
	}
	if !automergeTriggerEvents[event] {
		return
	}
	scheduled := trigger.NotifyPullRequests(repo, res.Invalidated...)
	if len(res.Invalidated) == 0 && res.HeadSHA != "" {
		scheduled += trigger.NotifyHead(repo, res.HeadSHA)
	}
	if scheduled > 0 {
		logger.Debug("automerge pass scheduled by webhook", "event", event, "repo", repo, "prs", scheduled)
	}
}

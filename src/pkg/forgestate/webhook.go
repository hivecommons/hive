package forgestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WebhookEvent is the hint a GitHub delivery carries: which item changed.
// ApplyWebhook never trusts the payload's state; it re-fetches the item.
type WebhookEvent struct {
	DeliveryID string
	Event      string
	Action     string
	Kind       Kind
	Number     int
	// UpdatedAt and HeadSHA are the payload's view, used only when the
	// confirming fetch fails on a close (the conservative direction).
	UpdatedAt time.Time
	HeadSHA   string
	// ReceivedAt is when the delivery arrived at the hive, stamped by the
	// webhook handler. When set, ApplyWebhook records the receipt-to-patch
	// latency as webhook.event_to_patch_ms; zero records nothing.
	ReceivedAt time.Time
}

func (ev WebhookEvent) closes() bool { return strings.EqualFold(ev.Action, "closed") }

// ApplyWebhook patches repo's cache for one delivery: it re-fetches only the
// named item (one call through the Gate) and upserts it monotonically.
// Closed or merged items leave the cache at once; a reopened item, carrying
// a newer updated_at, comes back. A delivery id already applied is not
// fetched again. Deliveries that name no issue or PR are recorded and
// otherwise ignored.
func (s *Store) ApplyWebhook(ctx context.Context, repo string, ev WebhookEvent) error {
	rs := s.lookup(repo)
	if rs == nil {
		return fmt.Errorf("%w: %s", ErrUnknownRepo, repo)
	}
	s.mu.Lock()
	if ev.DeliveryID != "" {
		rs.lastDelivery = ev.DeliveryID
		for _, d := range rs.deliveries {
			if d == ev.DeliveryID {
				s.mu.Unlock()
				s.opts.Metrics.WebhookPatch(rs.name, resultIgnored)
				return nil
			}
		}
	}
	s.mu.Unlock()
	if ev.Number <= 0 || (ev.Kind != KindIssue && ev.Kind != KindPR) {
		s.opts.Metrics.WebhookPatch(rs.name, resultIgnored)
		return nil
	}

	it, err := s.fetch(ctx, rs.name, ev.Kind, ev.Number)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	var result string
	switch {
	case errors.Is(err, ErrNotFound):
		s.removeLocked(rs, ev.Kind, ev.Number, ev.UpdatedAt, now, true)
		result = resultRemoved
	case err != nil:
		// The confirming read failed. A close is still applied from the
		// hint — dropping an item can only stop the hive acting on it —
		// but nothing else is trusted without a read.
		if ev.closes() && s.removeLocked(rs, ev.Kind, ev.Number, ev.UpdatedAt, now, false) {
			s.opts.Metrics.WebhookPatch(rs.name, resultRemoved)
		} else {
			s.opts.Metrics.WebhookPatch(rs.name, resultError)
		}
		return fmt.Errorf("forgestate: webhook %s %s#%d: %w", ev.DeliveryID, rs.name, ev.Number, err)
	default:
		result = s.upsertLocked(rs, it, SourceWebhook, now)
	}
	if ev.DeliveryID != "" {
		rs.deliveries = append(rs.deliveries, ev.DeliveryID)
		if len(rs.deliveries) > maxDeliveries {
			rs.deliveries = rs.deliveries[len(rs.deliveries)-maxDeliveries:]
		}
	}
	s.opts.Metrics.WebhookPatch(rs.name, result)
	RecordEventToPatch(ctx, rs.name, WriterForgeState, ev.ReceivedAt, now)
	return nil
}

// Observe applies an item another reader has just fetched from GitHub —
// the dashboard's webhook re-read, a freshness-bound confirming GET before
// an irreversible action — so one call serves both that reader and the
// cache. The upsert is monotonic exactly as ApplyWebhook's is: an older
// read than the cached copy (or a tombstone) is ignored, a closed or merged
// item is removed and leaves a tombstone. src should be SourceWebhook when
// a delivery prompted the read (deliveryID is then recorded for audit and
// dedupe) and SourceReadThrough otherwise. It reports whether the cache
// changed.
func (s *Store) Observe(repo string, it Item, src Source, deliveryID string) (bool, error) {
	rs := s.lookup(repo)
	if rs == nil {
		return false, fmt.Errorf("%w: %s", ErrUnknownRepo, repo)
	}
	if it.Number <= 0 || (it.Kind != KindIssue && it.Kind != KindPR) {
		return false, fmt.Errorf("forgestate: observe needs an issue or pr number")
	}
	if src == "" {
		src = SourceReadThrough
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	result := s.upsertLocked(rs, it, src, now)
	if result == resultIgnored {
		// A re-read that found the item unchanged still proves it as of
		// now: refresh the fetch time (and provenance) so the freshness
		// bound sees the confirmation rather than the older list.
		if cur, ok := rs.items(it.Kind)[it.Number]; ok && cur.UpdatedAt.Equal(it.UpdatedAt) {
			cur.FetchedAt, cur.Source = now, src
			rs.items(it.Kind)[it.Number] = cur
			result = resultOK
		}
	}
	if deliveryID != "" {
		rs.lastDelivery = deliveryID
		dup := false
		for _, d := range rs.deliveries {
			if d == deliveryID {
				dup = true
				break
			}
		}
		if !dup {
			rs.deliveries = append(rs.deliveries, deliveryID)
			if len(rs.deliveries) > maxDeliveries {
				rs.deliveries = rs.deliveries[len(rs.deliveries)-maxDeliveries:]
			}
		}
	}
	switch src {
	case SourceWebhook:
		s.opts.Metrics.WebhookPatch(rs.name, result)
	default:
		s.opts.Metrics.ReadThrough(rs.name, result)
	}
	return result != resultIgnored, nil
}

// webhookPayload is the subset of GitHub's issue/PR event payloads ParseWebhook reads.
type webhookPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue *struct {
		Number      int              `json:"number"`
		UpdatedAt   time.Time        `json:"updated_at"`
		PullRequest *json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	PullRequest *struct {
		Number    int       `json:"number"`
		UpdatedAt time.Time `json:"updated_at"`
		Head      struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

// ParseWebhook extracts the hint from a GitHub delivery (X-GitHub-Event,
// X-GitHub-Delivery, body). ok is false for events that name no issue or
// pull request; err is set only for an undecodable body.
func ParseWebhook(event, deliveryID string, payload []byte) (repo string, ev WebhookEvent, ok bool, err error) {
	switch event {
	case "issues", "issue_comment", "pull_request", "pull_request_review",
		"pull_request_review_comment", "pull_request_review_thread":
	default:
		return "", WebhookEvent{}, false, nil
	}
	var p webhookPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", WebhookEvent{}, false, fmt.Errorf("forgestate: decode %s webhook: %w", event, err)
	}
	ev = WebhookEvent{DeliveryID: deliveryID, Event: event, Action: p.Action}
	switch {
	case p.PullRequest != nil && p.PullRequest.Number > 0:
		ev.Kind, ev.Number = KindPR, p.PullRequest.Number
		ev.UpdatedAt, ev.HeadSHA = p.PullRequest.UpdatedAt, p.PullRequest.Head.SHA
	case p.Issue != nil && p.Issue.Number > 0:
		ev.Kind, ev.Number, ev.UpdatedAt = KindIssue, p.Issue.Number, p.Issue.UpdatedAt
		if p.Issue.PullRequest != nil {
			ev.Kind = KindPR
		}
	default:
		return p.Repository.FullName, WebhookEvent{}, false, nil
	}
	return p.Repository.FullName, ev, true, nil
}

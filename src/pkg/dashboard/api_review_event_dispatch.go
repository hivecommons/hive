package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/review/eventdispatch"
	"github.com/hivecommons/hive/pkg/review/pipeline"
)

const reviewEventDispatchPath = "/api/review/dispatch/events"

func (s *Server) registerReviewEventDispatchRoutes() {
	s.mux.HandleFunc("GET "+reviewEventDispatchPath, s.handleReviewEventDispatch)
}

// reviewEventDispatchResponse is GET /api/review/dispatch/events.
type reviewEventDispatchResponse struct {
	Enabled            bool `json:"enabled"`
	WebhooksConfigured bool `json:"webhooks_configured"`
	DebounceS          int  `json:"debounce_s"`
	eventdispatch.Status
}

func githubWebhooksConfigured() bool {
	return strings.TrimSpace(os.Getenv(githubWebhookSecretEnvVar)) != ""
}

func (s *Server) reviewEvents() *eventdispatch.Dispatcher {
	if s == nil || s.deps == nil {
		return nil
	}
	return s.deps.ReviewEvents
}

// reviewEventSettings reports whether webhook review dispatch is on and its
// debounce in seconds, read from the live config.
func (s *Server) reviewEventSettings() (bool, int) {
	if s == nil || s.deps == nil || s.deps.Config == nil || s.deps.ReviewEvents == nil {
		return false, 0
	}
	rc := s.deps.Config.Review
	return rc.ReviewEventDrivenEnabled(githubWebhooksConfigured()), int(rc.ReviewEventDebounce().Seconds())
}

// handleReviewEventDispatch serves the webhook review-dispatch queue
// (hivecommons/hive#11091): whether it is on, the debounce, pending PRs
// (soonest first), recent dispatches (newest first) and lifetime counters.
// Read-only and in-memory, so it never calls GitHub.
func (s *Server) handleReviewEventDispatch(w http.ResponseWriter, _ *http.Request) {
	enabled, debounce := s.reviewEventSettings()
	jsonResponse(w, reviewEventDispatchResponse{
		Enabled:            enabled,
		WebhooksConfigured: githubWebhooksConfigured(),
		DebounceS:          debounce,
		Status:             s.reviewEvents().Snapshot(),
	})
}

type reviewEventPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest *struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Draft  bool   `json:"draft"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}

// enqueueReviewEvent queues an early review dispatch for a signature-verified
// pull_request delivery on a managed repo and returns the queue outcome, or
// "" when the event does not ask for review or event dispatch is off.
func (s *Server) enqueueReviewEvent(event string, body []byte) string {
	enabled, debounceS := s.reviewEventSettings()
	if !enabled || event != "pull_request" {
		return ""
	}
	var p reviewEventPayload
	if err := json.Unmarshal(body, &p); err != nil || p.PullRequest == nil {
		return ""
	}
	pr := p.PullRequest
	req, ok := eventdispatch.RequestFromWebhook(event, p.Action, p.Repository.FullName, pr.Number, pr.Head.SHA, pr.State, pr.Draft)
	if !ok {
		return ""
	}
	debounce := s.deps.Config.Review.ReviewEventDebounce()
	outcome := s.deps.ReviewEvents.Enqueue(req, debounce)
	s.webhookLogger().Info("review event dispatch", "repo", req.Repo, "pr", req.Number,
		"action", req.Action, "head_sha", req.HeadSHA, "outcome", outcome, "debounce_s", debounceS)
	return outcome
}

// reviewCardTrigger labels how a pipeline card's current head reached
// review: event (webhook dispatch fired), event_pending (queued), or cadence
// when the head has reviewers but no event dispatch fired for it.
func (s *Server) reviewCardTrigger(card pipeline.Card) string {
	if t := s.reviewEvents().Trigger(card.Repo, card.Number, card.HeadSHA); t != "" {
		return t
	}
	if len(card.Reviewers) > 0 {
		return eventdispatch.TriggerCadence
	}
	return ""
}

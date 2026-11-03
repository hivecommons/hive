package dashboard

import (
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github/automerge"
)

// Preparation is lifecycle evidence, not a green check, review, or train seat.
type StageTrackerPreparation struct {
	Role        string    `json:"role"`
	Behind      int       `json:"behind,omitempty"`
	Since       time.Time `json:"since"`
	HeadSHA     string    `json:"head_sha"`
	State       string    `json:"state"`
	Detail      string    `json:"detail"`
	Number      int       `json:"number"`
	CurrentHead string    `json:"current_head,omitempty"`
	Pipeline    string    `json:"pipeline,omitempty"`
}

type StageTrackerPreparationQueue struct {
	Repo       string                    `json:"repo"`
	BaseRef    string                    `json:"base_ref"`
	ObservedAt time.Time                 `json:"observed_at"`
	State      string                    `json:"state"`
	Message    string                    `json:"message,omitempty"`
	Owner      *StageTrackerPreparation  `json:"owner,omitempty"`
	Waiting    []StageTrackerPreparation `json:"waiting,omitempty"`
}

func (s *Server) stageTrackerPreparation(items []StageTrackerItem, now time.Time, state ...func(string, int) string) []StageTrackerPreparationQueue {
	if s.deps == nil || s.deps.Config == nil {
		return nil
	}
	type destination struct{ repo, base string }
	var destinations []destination
	for i := range items {
		it := &items[i]
		if it.Kind != "pr" || !s.deps.Config.AutoMerge.RequireCurrentBaseFor(it.Repo) {
			continue
		}
		d := destination{strings.ToLower(strings.TrimSpace(it.Repo)), it.BaseRef}
		if !slices.Contains(destinations, d) {
			destinations = append(destinations, d)
		}
	}
	var queues []StageTrackerPreparationQueue
	for _, d := range destinations {
		q := StageTrackerPreparationQueue{Repo: d.repo, BaseRef: d.base, ObservedAt: now, State: "unavailable"}
		if d.base == "" {
			q.Message = "Preparation unknown: base ref unavailable in the current snapshot"
			queues = append(queues, q)
			continue
		}
		if s.deps.BasePreparation == nil {
			q.Message = "Preparation unknown: reader unavailable"
			queues = append(queues, q)
			continue
		}
		snapshot, err := s.deps.BasePreparation(d.repo, d.base)
		if err != nil {
			q.Message = "Preparation unknown: durable state could not be read"
			queues = append(queues, q)
			continue
		}
		if !snapshot.Enabled {
			continue
		}
		if !snapshot.Present {
			q.Message = "Preparation unknown: no durable snapshot recorded yet"
			queues = append(queues, q)
			continue
		}
		q.State, q.ObservedAt = "observed", snapshot.ObservedAt
		front := 0
		project := func(m automerge.BasePreparationMember, role string, behind int) *StageTrackerPreparation {
			p := &StageTrackerPreparation{Role: role, Behind: behind, Number: m.Number, HeadSHA: m.HeadSHA, Since: m.Since, State: "unverified", Detail: "PR absent from current open snapshot"}
			if len(state) > 0 && state[0] != nil && stageTrackerLinkedPRFinished(state[0](d.repo, m.Number)) {
				p.State, p.Detail = "inactive", "PR closed or merged; stale ticket awaiting scheduler reconciliation"
				return p
			}
			if !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt) {
				p.State, p.Detail = "inactive", "Ticket expired; awaiting scheduler reconciliation"
			}
			for i := range items {
				it := &items[i]
				if it.Kind != "pr" || !strings.EqualFold(it.Repo, d.repo) || it.Number != m.Number {
					continue
				}
				p.CurrentHead = it.Head
				switch {
				case it.BaseRef != d.base:
					p.State, p.Detail = "inactive", "PR retargeted"
				case it.OnHold || slices.Contains(it.Modifiers, modHold) || slices.Contains(it.Modifiers, "draft") || it.Bucket == "draft" || it.Superseded != nil:
					p.State, p.Detail = "inactive", "PR held, draft, or superseded"
				case m.Since.IsZero() || m.ExpiresAt.IsZero() || m.Since.After(now):
					p.Detail = "Ticket age unknown"
				case !now.Before(m.ExpiresAt):
					p.State, p.Detail = "inactive", "Ticket expired; awaiting scheduler reconciliation"
				case it.Head == "" || m.HeadSHA == "":
					p.Detail = "Current head unknown"
				case it.Head != m.HeadSHA:
					p.Detail = "Head changed; current preparation unverified"
					if role == "owner" {
						front = m.Number // ownership survives refresh; head-specific proofs do not
					}
				default:
					p.State, p.Detail = "current", "Ticket matches the current open PR snapshot"
					p.Pipeline = it.Caption
					if role == "owner" {
						front = m.Number
					}
				}
				if p.State == "current" || (role == "owner" && p.State == "unverified") {
					it.Preparation = p
				}
				return p
			}
			return p
		}
		if snapshot.Owner != nil {
			q.Owner = project(*snapshot.Owner, "owner", 0)
		}
		for _, m := range snapshot.Waiting {
			p := project(m, "waiting", front)
			q.Waiting = append(q.Waiting, *p)
		}
		if q.Owner == nil && len(q.Waiting) == 0 {
			q.Message = "No recorded preparation tickets"
		}
		queues = append(queues, q)
	}
	return queues
}

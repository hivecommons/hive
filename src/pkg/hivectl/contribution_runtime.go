package hivectl

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Runtime overlay for the contribution tree (RFC #13153 §6).
//
// The read model BuildContributionTree produces is consent only: what the
// operator saved. This file adds what the local relay is doing with that
// consent, from two deliberately separate sources:
//
//   - live: relay-status.json, trusted only when it belongs to the relay the
//     PID record names and that relay is still running (#13290);
//   - durable: the local terminal-attempt history store (#13293), which
//     survives relay restarts and is pruned by retention.
//
// Both are labels over the tree, never claims. When a source is unavailable
// the overlay says so with a reason; it never renders "idle", "0 completed"
// or "connected" without evidence. Nothing here reads a token.

// Runtime vocabulary shared by every surface.
const (
	TreeRuntimeUnavailable = "unavailable"
	// DurableActivityNotShipped is the reason shown while this build has no
	// local activity store to read.
	DurableActivityNotShipped = "no local activity store on this build (#13297)"
	treeReadinessNotAdopted   = "not adopted by relay"
	treeActivityIdle          = "idle"
	treeActivityWorking       = "working on "
	treeActivityInProgress    = "in progress"
	treeHistoryNone           = "no recorded activity"
)

// TreeLive is the live relay overlay: readiness and acknowledgement per Hive
// plus the current task, valid only while Available.
type TreeLive struct {
	Available bool `json:"available" yaml:"available"`
	// Reason explains an unavailable overlay. Credential-free.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
	// Summary is Label() precomputed so JSON consumers show the same line.
	Summary             string `json:"summary" yaml:"summary"`
	PID                 int    `json:"pid,omitempty" yaml:"pid,omitempty"`
	AdoptedScopeVersion uint64 `json:"adopted_scope_version" yaml:"adopted_scope_version"`
	SavedScopeVersion   uint64 `json:"saved_scope_version" yaml:"saved_scope_version"`
	// Acknowledged is acknowledged, pending or unknown for the saved
	// generation across every enabled subscription.
	Acknowledged  string                          `json:"acknowledged" yaml:"acknowledged"`
	IdleReason    string                          `json:"idle_reason,omitempty" yaml:"idle_reason,omitempty"`
	CurrentTask   *RelayCurrentTask               `json:"current_task,omitempty" yaml:"current_task,omitempty"`
	Subscriptions map[string]TreeLiveSubscription `json:"subscriptions,omitempty" yaml:"subscriptions,omitempty"`
}

// TreeLiveSubscription is one Hive's live state, keyed by Hive origin.
type TreeLiveSubscription struct {
	Readiness    string `json:"readiness" yaml:"readiness"`
	Acknowledged string `json:"acknowledged" yaml:"acknowledged"`
	ObservedAt   string `json:"observed_at" yaml:"observed_at"`
}

// TreeDurable is the durable history overlay rolled up per repository.
type TreeDurable struct {
	Available    bool                             `json:"available" yaml:"available"`
	Reason       string                           `json:"reason,omitempty" yaml:"reason,omitempty"`
	Summary      string                           `json:"summary" yaml:"summary"`
	Retention    string                           `json:"retention,omitempty" yaml:"retention,omitempty"`
	Attempts     int                              `json:"attempts" yaml:"attempts"`
	Completed    int                              `json:"completed" yaml:"completed"`
	LastActivity time.Time                        `json:"last_activity,omitempty" yaml:"last_activity,omitempty"`
	Repositories map[string]TreeDurableRepository `json:"repositories,omitempty" yaml:"repositories,omitempty"`
}

// TreeDurableRepository is one repository's retained history.
type TreeDurableRepository struct {
	Attempts     int            `json:"attempts" yaml:"attempts"`
	Completed    int            `json:"completed" yaml:"completed"`
	Outcomes     map[string]int `json:"outcomes,omitempty" yaml:"outcomes,omitempty"`
	LastActivity time.Time      `json:"last_activity,omitempty" yaml:"last_activity,omitempty"`
}

// TreeRuntime pairs the two overlays.
type TreeRuntime struct {
	Live    TreeLive    `json:"live" yaml:"live"`
	Durable TreeDurable `json:"durable" yaml:"durable"`
}

// DurableActivitySource supplies the durable overlay for a store. A nil
// source, or an error, yields an unavailable overlay with a reason.
type DurableActivitySource func(store *ProfileStore) (TreeDurable, error)

// UnavailableDurable is the durable overlay for a build without a store.
func UnavailableDurable(reason string) TreeDurable {
	if strings.TrimSpace(reason) == "" {
		reason = DurableActivityNotShipped
	}
	d := TreeDurable{Reason: reason}
	d.Summary = d.Label()
	return d
}

// LiveTreeRuntime reads the live overlay for the saved set. It is never an
// error: an unreadable, stale or dead relay is an unavailable overlay with a
// reason.
func (s *ProfileStore) LiveTreeRuntime(set *ProfileSet) TreeLive {
	live := TreeLive{Acknowledged: "unknown"}
	if set != nil {
		live.SavedScopeVersion = set.RepoScopeVersion
	}
	status, reason := s.liveRelayStatus()
	if status == nil {
		live.Reason = reason
		live.Summary = live.Label()
		return live
	}
	live.Available = true
	live.PID = status.PID
	live.AdoptedScopeVersion = status.AdoptedScopeVersion
	live.CurrentTask = status.CurrentTask
	if status.IdleReason != nil {
		live.IdleReason = *status.IdleReason
	}
	live.Subscriptions = make(map[string]TreeLiveSubscription, len(status.Subscriptions))
	for _, sub := range status.Subscriptions {
		ack := "pending"
		if status.AdoptedScopeVersion >= live.SavedScopeVersion && sub.AcknowledgedScopeVersion != nil && *sub.AcknowledgedScopeVersion >= live.SavedScopeVersion {
			ack = "acknowledged"
		}
		live.Subscriptions[sub.Hive] = TreeLiveSubscription{Readiness: sub.Readiness, Acknowledged: ack, ObservedAt: sub.ObservedAt}
	}
	live.Acknowledged = "acknowledged"
	if set != nil {
		for _, p := range set.Profiles {
			if p.Disabled {
				continue
			}
			origin, err := NormalizeHiveID(p.Hub)
			if err != nil {
				continue
			}
			if entry, ok := live.Subscriptions[origin]; !ok || entry.Acknowledged != "acknowledged" {
				live.Acknowledged = "pending"
			}
		}
	}
	live.Summary = live.Label()
	return live
}

// DurableTreeRuntime reads the durable overlay through source, which may be
// nil on builds without a store.
func (s *ProfileStore) DurableTreeRuntime(source DurableActivitySource) TreeDurable {
	if source == nil {
		return UnavailableDurable(DurableActivityNotShipped)
	}
	durable, err := source(s)
	if err != nil {
		return UnavailableDurable(err.Error())
	}
	if !durable.Available {
		return UnavailableDurable(durable.Reason)
	}
	durable.Summary = durable.Label()
	return durable
}

// Label is the one-line live column.
func (l TreeLive) Label() string {
	if !l.Available {
		return TreeRuntimeUnavailable + " — " + l.Reason
	}
	label := fmt.Sprintf("relay pid %d · scope v%d %s", l.PID, l.AdoptedScopeVersion, l.Acknowledged)
	switch {
	case l.CurrentTask != nil:
		label += " · " + treeActivityWorking + l.CurrentTask.Repository
		if l.CurrentTask.Draining {
			label += " (draining)"
		}
	case l.IdleReason != "":
		label += " · " + treeActivityIdle + " — " + l.IdleReason
	default:
		label += " · " + treeActivityIdle
	}
	return label
}

// Label is the one-line durable column.
func (d TreeDurable) Label() string {
	if !d.Available {
		return TreeRuntimeUnavailable + " — " + d.Reason
	}
	if d.Attempts == 0 {
		label := treeHistoryNone
		if d.Retention != "" {
			label += " within " + d.Retention
		}
		return label
	}
	label := fmt.Sprintf("%d attempt(s) · %d completed", d.Attempts, d.Completed)
	if !d.LastActivity.IsZero() {
		label += " · last " + d.LastActivity.UTC().Format(time.RFC3339)
	}
	if d.Retention != "" {
		label += " · retention " + d.Retention
	}
	return label
}

// ApplyRuntime decorates a tree with both overlays. Readiness and Activity
// come from the live source, History from the durable one; each stays at its
// unavailable label when its source is. Disabled subscriptions keep their
// "disabled" readiness because the relay must not be working them at all.
func (t *ContributionTree) ApplyRuntime(rt TreeRuntime) {
	if t == nil {
		return
	}
	for i := range t.Subscriptions {
		sub := &t.Subscriptions[i]
		sub.History = TreeRuntimeUnavailable
		if rt.Live.Available {
			entry, adopted := rt.Live.Subscriptions[sub.HiveID]
			switch {
			case sub.Disabled:
				sub.Readiness = "disabled"
			case adopted:
				sub.Readiness = entry.Readiness
			default:
				sub.Readiness = treeReadinessNotAdopted
			}
			sub.Activity = liveSubscriptionActivity(rt.Live, sub)
		}
		if rt.Durable.Available {
			sub.History = durableSubscriptionHistory(rt.Durable, sub)
		}
		for j := range sub.Repositories {
			repo := &sub.Repositories[j]
			repo.Activity, repo.History = TreeRuntimeUnavailable, TreeRuntimeUnavailable
			if rt.Live.Available {
				repo.Activity = treeActivityIdle
				if task := rt.Live.CurrentTask; task != nil && task.Hive == sub.HiveID && task.Repository == repo.ID {
					repo.Activity = treeActivityInProgress
				}
			}
			if rt.Durable.Available {
				repo.History = treeHistoryNone
				if h, ok := rt.Durable.Repositories[repo.ID]; ok && h.Attempts > 0 {
					repo.History = fmt.Sprintf("%d attempt(s) · %d completed", h.Attempts, h.Completed)
					if !h.LastActivity.IsZero() {
						repo.History += " · last " + h.LastActivity.UTC().Format(time.RFC3339)
					}
				}
			}
		}
	}
}

func liveSubscriptionActivity(live TreeLive, sub *TreeSubscription) string {
	if task := live.CurrentTask; task != nil && task.Hive == sub.HiveID {
		label := treeActivityWorking + task.Repository
		if task.Draining {
			label += " (draining)"
		}
		return label
	}
	if live.IdleReason != "" {
		return treeActivityIdle + " — " + live.IdleReason
	}
	return treeActivityIdle
}

func durableSubscriptionHistory(durable TreeDurable, sub *TreeSubscription) string {
	attempts, completed := 0, 0
	var last time.Time
	ids := make([]string, 0, len(sub.Repositories))
	for _, r := range sub.Repositories {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		h, ok := durable.Repositories[id]
		if !ok {
			continue
		}
		attempts += h.Attempts
		completed += h.Completed
		if h.LastActivity.After(last) {
			last = h.LastActivity
		}
	}
	if attempts == 0 {
		return treeHistoryNone
	}
	label := fmt.Sprintf("%d attempt(s) · %d completed", attempts, completed)
	if !last.IsZero() {
		label += " · last " + last.UTC().Format(time.RFC3339)
	}
	return label
}

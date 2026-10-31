package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	forgequeue "github.com/hivecommons/hive/pkg/forgestate"
)

const (
	nativeQueueObservationInterval = 3 * time.Minute
	nativeQueueObservationTimeout  = 45 * time.Second
	nativeQueueObservationScopes   = 2
	nativeQueueObservationHistory  = 32
	nativeQueueObservationHints    = 16
	nativeQueueObservationCache    = 32
)

type nativeQueueHint struct {
	repo, base string
	at         time.Time
	candidate  forgequeue.QueueCandidate
}

type nativeQueueTrackerRuntime struct {
	mu           sync.Mutex
	lastAttempt  time.Time
	inFlight     bool
	scopeOffset  int
	observations map[string]forgequeue.QueueObservation
	hints        map[string]nativeQueueHint
}

func nativeQueueScopeKey(repo, base string) string { return strings.ToLower(repo) + "#" + base }

func (s *Server) nativeQueueScopeAllowed(repo, base string) bool {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return false
	}
	q := s.deps.Config.AutoMerge.NativeMergeQueue
	bases := q.BaseSet()
	return q.EnabledFor(repo) && (bases == nil || bases[nativeQueueScopeKey(repo, base)])
}

// ReconcileNativeQueues reuses the governor's background cycle, never a render
// or per-PR poller. Off means no native observation network calls.
func (s *Server) ReconcileNativeQueues(ctx context.Context) error {
	if s == nil || s.deps == nil || s.deps.Config == nil || !s.deps.Config.AutoMerge.NativeMergeQueue.Enabled {
		return nil
	}
	runtime := &s.nativeQueueTracker
	runtime.mu.Lock()
	now := time.Now()
	if runtime.inFlight || now.Sub(runtime.lastAttempt) < nativeQueueObservationInterval {
		runtime.mu.Unlock()
		return nil
	}
	runtime.lastAttempt, runtime.inFlight = now, true
	scopes := make(map[string][2]string)
	for _, scope := range s.deps.Config.AutoMerge.NativeMergeQueue.Bases {
		repo, base, valid := strings.Cut(scope, "#")
		if valid && s.nativeQueueScopeAllowed(repo, base) {
			scopes[nativeQueueScopeKey(repo, base)] = [2]string{repo, base}
		}
	}
	for id, hint := range runtime.hints {
		if now.Sub(hint.at) >= 2*time.Hour {
			delete(runtime.hints, id)
			continue
		}
		if s.nativeQueueScopeAllowed(hint.repo, hint.base) {
			scopes[nativeQueueScopeKey(hint.repo, hint.base)] = [2]string{hint.repo, hint.base}
		}
	}
	keys := make([]string, 0, len(scopes))
	for key := range scopes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	start := runtime.scopeOffset
	if len(keys) > 0 {
		start %= len(keys)
		runtime.scopeOffset = (start + nativeQueueObservationScopes) % len(keys)
	}
	runtime.mu.Unlock()
	defer func() {
		runtime.mu.Lock()
		runtime.inFlight = false
		runtime.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, nativeQueueObservationTimeout)
	defer cancel()
	if s.deps.GHClient == nil || !s.deps.GHClient.NativeMergeQueueAvailable() {
		runtime.mu.Lock()
		if runtime.observations == nil {
			runtime.observations = make(map[string]forgequeue.QueueObservation)
		}
		for key, scope := range scopes {
			observation := runtime.observations[key]
			observation.Provider, observation.Repo, observation.Base = "github-native", scope[0], scope[1]
			observation.State, observation.Stale = "unavailable", true
			observation.Reason = "GitHub App installation authentication is unavailable; queue evidence cannot be refreshed."
			runtime.storeObservation(key, observation)
		}
		runtime.mu.Unlock()
		return fmt.Errorf("native queue observation requires GitHub App installation authentication")
	}
	for offset := 0; offset < len(keys) && offset < nativeQueueObservationScopes; offset++ {
		key := keys[(start+offset)%len(keys)]
		scope := scopes[key]
		runtime.mu.Lock()
		orderedHints := []nativeQueueHint{}
		for _, hint := range runtime.hints {
			if nativeQueueScopeKey(hint.repo, hint.base) == key && now.Sub(hint.at) < 2*time.Hour {
				orderedHints = append(orderedHints, hint)
			}
		}
		sort.Slice(orderedHints, func(i, j int) bool { return orderedHints[i].at.After(orderedHints[j].at) })
		hints := make([]forgequeue.QueueCandidate, 0, len(orderedHints))
		for _, hint := range orderedHints {
			hints = append(hints, hint.candidate)
		}
		runtime.mu.Unlock()
		observation, err := s.deps.GHClient.ReadNativeQueueObservation(ctx, scope[0], scope[1], hints)
		runtime.mu.Lock()
		if runtime.observations == nil {
			runtime.observations = make(map[string]forgequeue.QueueObservation)
		}
		prior, exists := runtime.observations[key]
		if err != nil {
			if exists {
				observation = prior
			}
			observation.Provider, observation.Repo, observation.Base = "github-native", scope[0], scope[1]
			observation.State, observation.Stale = "unavailable", true
			observation.Reason = "Refresh unavailable; retained observations are not current queue evidence."
		} else if exists {
			observation = nativeQueueObservationDelta(prior, observation, now)
		}
		runtime.storeObservation(key, observation)
		runtime.mu.Unlock()
	}
	return nil
}

// Caller holds mu; old scopes cannot accumulate across Settings changes.
func (runtime *nativeQueueTrackerRuntime) storeObservation(key string, observation forgequeue.QueueObservation) {
	if runtime.observations == nil {
		runtime.observations = make(map[string]forgequeue.QueueObservation)
	}
	if _, exists := runtime.observations[key]; !exists && len(runtime.observations) >= nativeQueueObservationCache {
		oldest := ""
		for scope, prior := range runtime.observations {
			if oldest == "" || prior.ObservedAt.Before(runtime.observations[oldest].ObservedAt) {
				oldest = scope
			}
		}
		delete(runtime.observations, oldest)
	}
	runtime.observations[key] = observation
}

func nativeQueueObservationDelta(prior, next forgequeue.QueueObservation, now time.Time) forgequeue.QueueObservation {
	next.Outcomes = append([]forgequeue.QueueOutcome{}, prior.Outcomes...)
	if !prior.EntriesComplete || !next.EntriesComplete {
		return next
	}
	for _, entry := range prior.Entries {
		found := false
		for _, current := range next.Entries {
			found = found || entry.ID == current.ID
		}
		if !found {
			next.Outcomes = appendNativeQueueOutcome(next.Outcomes, forgequeue.QueueOutcome{
				Kind: "entry-removed", PR: entry.PR, Head: entry.Head, ObservedAt: now, Source: "complete-queue-snapshot-difference",
			})
		}
	}
	return next
}

func appendNativeQueueOutcome(out []forgequeue.QueueOutcome, next forgequeue.QueueOutcome) []forgequeue.QueueOutcome {
	for _, prior := range out {
		if prior.Kind == next.Kind && prior.PR == next.PR && prior.Head == next.Head &&
			prior.CandidateSHA == next.CandidateSHA && prior.MergeSHA == next.MergeSHA {
			return out
		}
	}
	out = append(out, next)
	if len(out) > nativeQueueObservationHistory {
		out = out[len(out)-nativeQueueObservationHistory:]
	}
	return out
}

// Called only after the existing authenticated webhook path verifies the
// delivery and repository. No API read or merge side effect occurs here.
func (s *Server) observeNativeQueueEvent(event string, body []byte, at time.Time) {
	if s == nil || s.deps == nil || s.deps.Config == nil || !s.deps.Config.AutoMerge.NativeMergeQueue.Enabled {
		return
	}
	var payload integrationWebhookPayload
	if json.Unmarshal(body, &payload) != nil {
		return
	}
	repo := payload.Repository.FullName
	base := strings.TrimPrefix(payload.MergeGroup.BaseRef, "refs/heads/")
	if payload.PullRequest != nil {
		base = payload.PullRequest.GetBase().GetRef()
	}
	if !s.nativeQueueScopeAllowed(repo, base) {
		return
	}
	runtime := &s.nativeQueueTracker
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.observations == nil {
		runtime.observations = make(map[string]forgequeue.QueueObservation)
	}
	key := nativeQueueScopeKey(repo, base)
	observation := runtime.observations[key]
	observation.Provider, observation.Repo, observation.Base = "github-native", repo, base
	if observation.State == "" {
		observation.State, observation.Reason = "unknown", "Waiting for a bounded authoritative queue snapshot."
	}
	group := payload.MergeGroup
	if event == "merge_group" && group.HeadSHA != "" {
		if runtime.hints == nil {
			runtime.hints = make(map[string]nativeQueueHint)
		}
		hintKey := key + "/" + group.HeadSHA
		switch payload.Action {
		case "checks_requested":
			if _, exists := runtime.hints[hintKey]; !exists && len(runtime.hints) >= nativeQueueObservationHints {
				oldest := ""
				for id, hint := range runtime.hints {
					if oldest == "" || hint.at.Before(runtime.hints[oldest].at) {
						oldest = id
					}
				}
				delete(runtime.hints, oldest)
			}
			runtime.hints[hintKey] = nativeQueueHint{repo: repo, base: base, at: at,
				candidate: forgequeue.QueueCandidate{SHA: group.HeadSHA, Ref: group.HeadRef, BaseSHA: group.BaseSHA}}
		case "destroyed":
			delete(runtime.hints, hintKey)
			observation.Outcomes = appendNativeQueueOutcome(observation.Outcomes, forgequeue.QueueOutcome{
				Kind: "candidate-destroyed", CandidateSHA: group.HeadSHA, ObservedAt: at, Source: "signed-merge-group-webhook",
			})
			for i := range observation.Candidates {
				if observation.Candidates[i].SHA == group.HeadSHA {
					observation.Candidates[i].CI = "destroyed"
				}
			}
		}
	}
	if event == "pull_request" && payload.PullRequest != nil && payload.PullRequest.GetMerged() {
		pr := payload.PullRequest
		observedMember := false
		for _, entry := range observation.Entries {
			observedMember = observedMember || entry.PR == pr.GetNumber() && entry.Head == pr.GetHead().GetSHA()
		}
		for _, candidate := range observation.Candidates {
			if candidate.Membership == "proven" {
				for _, member := range candidate.Members {
					observedMember = observedMember || member.PR == pr.GetNumber() && member.Head == pr.GetHead().GetSHA()
				}
			}
		}
		if observedMember && pr.GetMergeCommitSHA() != "" {
			observation.Outcomes = appendNativeQueueOutcome(observation.Outcomes, forgequeue.QueueOutcome{
				Kind: "merged", PR: pr.GetNumber(), Head: pr.GetHead().GetSHA(), MergeSHA: pr.GetMergeCommitSHA(),
				ObservedAt: at, Source: "signed-pull-request-merged-webhook",
			})
		}
	}
	runtime.storeObservation(key, observation)
}

// Cached projection only; it never consults GitHub from a tracker request.
func (s *Server) stageTrackerNativeQueues(now time.Time) []forgequeue.QueueObservation {
	if s == nil || s.deps == nil || s.deps.Config == nil || !s.deps.Config.AutoMerge.NativeMergeQueue.Enabled {
		return nil
	}
	runtime := &s.nativeQueueTracker
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	result := []forgequeue.QueueObservation{}
	for _, original := range runtime.observations {
		if !s.nativeQueueScopeAllowed(original.Repo, original.Base) {
			continue
		}
		// Copy nested slices: event writes must not mutate an encoded response.
		observation := original
		observation.Entries = append([]forgequeue.QueueEntry{}, original.Entries...)
		for i := range observation.Entries {
			if original.Entries[i].EnqueuedAt != nil {
				at := *original.Entries[i].EnqueuedAt
				observation.Entries[i].EnqueuedAt = &at
			}
		}
		observation.Outcomes = append([]forgequeue.QueueOutcome{}, original.Outcomes...)
		observation.Candidates = append([]forgequeue.QueueCandidate{}, original.Candidates...)
		for i := range observation.Candidates {
			observation.Candidates[i].Members = append([]forgequeue.QueueMember{}, original.Candidates[i].Members...)
			observation.Candidates[i].Checks = append([]forgequeue.QueueCheck{}, original.Candidates[i].Checks...)
		}
		observation.Stale = observation.Stale || observation.ObservedAt.IsZero() || now.Sub(observation.ObservedAt) > 2*nativeQueueObservationInterval
		observation.FreshForS = int64((2 * nativeQueueObservationInterval).Seconds())
		result = append(result, observation)
	}
	for _, scope := range s.deps.Config.AutoMerge.NativeMergeQueue.Bases {
		repo, base, valid := strings.Cut(scope, "#")
		if !valid || !s.nativeQueueScopeAllowed(repo, base) {
			continue
		}
		exists := false
		for _, observation := range result {
			exists = exists || nativeQueueScopeKey(repo, base) == nativeQueueScopeKey(observation.Repo, observation.Base)
		}
		if !exists {
			result = append(result, forgequeue.QueueObservation{Provider: "github-native", Repo: repo, Base: base, State: "unknown", Stale: true,
				Reason: "No authoritative snapshot for this scope yet; bounded background reconciliation rotates across scopes."})
		}
	}
	if len(result) == 0 {
		result = append(result, forgequeue.QueueObservation{Provider: "github-native", State: "unknown", Stale: true,
			Reason: "No authoritative queue snapshot yet. Select exact repo/base scopes in Settings; authenticated events and background reconciliation supply observations."})
	}
	sort.Slice(result, func(i, j int) bool {
		return nativeQueueScopeKey(result[i].Repo, result[i].Base) < nativeQueueScopeKey(result[j].Repo, result[j].Base)
	})
	return result
}

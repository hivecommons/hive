package integration

import (
	"fmt"
	"sort"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type Cost struct {
	RunnerMinutes *float64 `json:"runner_minutes"`
	Jobs          int      `json:"jobs"`
	Running       int      `json:"running"`
	Complete      bool     `json:"complete"`
	Reason        string   `json:"reason"`
}

func cost(jobs map[string]Job, coverage Coverage, repo, base string, from, through time.Time) Cost {
	out := Cost{Complete: coverage.Complete && !coverage.From.After(from) && !coverage.Through.Before(through), Reason: coverage.Reason}
	minutes := 0.0
	for _, j := range jobs {
		if j.Repo != repo || j.Base != base || j.RunnerID == 0 || j.StartedAt.IsZero() {
			continue
		}
		end := j.CompletedAt
		if end.IsZero() {
			end = j.ObservedAt
			if end.After(from) {
				out.Running++
			}
			// A live job's unobserved future is unknown, not zero execution.
			if end.Before(through) {
				out.Complete = false
			}
		}
		start := j.StartedAt
		if start.Before(from) {
			start = from
		}
		if end.After(through) {
			end = through
		}
		if end.After(start) {
			minutes += end.Sub(start).Minutes()
			out.Jobs++
		}
	}
	if out.Complete {
		out.RunnerMinutes = &minutes
	} else {
		out.Reason = "workflow job coverage incomplete or stale; admission growth frozen"
	}
	return out
}

func (s *Store) Cost(repo, base string, from, through time.Time) Cost {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cost(s.jobs, s.coverage[scopeKey(repo, base)], repo, base, from, through)
}

type Dependency struct {
	Known     bool
	Satisfied bool
}

// Input consists of authoritative observations obtained outside Store's lock.
// The approved DAG and live lease, not an intent author's assertion, authorize
// the corresponding constraints.
type Input struct {
	Key             string
	Repo            string
	Base            string
	PlanApproved    bool
	PlanRevision    int
	Dependencies    map[string]Dependency
	Claim           string
	ClaimGeneration uint64
	ClaimExpiresAt  time.Time
	Reserve         bool
	Head            string
	Now             time.Time
}

type Decision struct {
	Allowed    bool     `json:"allowed"`
	WouldBlock bool     `json:"would_block"`
	Reason     string   `json:"reason"`
	Hints      []string `json:"hints"`
	Cost       Cost     `json:"cost"`
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func (s *Store) Decide(raw config.IntegrationOrchestrationConfig, in Input) (Decision, error) {
	cfg := raw.Effective()
	out := Decision{Allowed: true, Hints: []string{}}
	if !cfg.Allows(in.Repo, in.Base) {
		return out, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	intent, found := s.intents[in.Key]
	block := func(reason string) {
		if !out.WouldBlock {
			out.WouldBlock, out.Reason = true, reason
		}
	}
	if !found {
		out.Hints = append(out.Hints, "scope unknown: no versioned intent; independence not established")
		block("qualified change intent unavailable")
	} else {
		if !intent.ScopeKnown {
			out.Hints = append(out.Hints, "anticipated scope unknown; not evidence of independence")
			block("anticipated scope unknown; qualified cohort unavailable")
		}
		if len(intent.Prerequisites) > 0 {
			if !in.PlanApproved || in.PlanRevision != intent.PlanRevision {
				block("approved plan generation unavailable or changed")
			}
			for _, p := range intent.Prerequisites {
				d := in.Dependencies[p.Key]
				if !d.Known {
					block("validated prerequisite unknown: " + p.Key)
				} else if !d.Satisfied {
					block("waiting for prerequisite: " + p.Key)
				}
			}
		}
		for key, r := range s.reservations {
			if key == in.Key || !r.ExpiresAt.After(in.Now) {
				continue
			}
			other, ok := s.intents[key]
			if !ok || other.Repo != intent.Repo || other.Base != intent.Base {
				continue
			}
			if intersects(intent.Contracts, other.Contracts) || intersects(intent.Components, other.Components) || intersects(intent.Generated, other.Generated) {
				out.Hints = append(out.Hints, "semantic interaction with "+key+" (advisory; not an exclusive lock)")
			} else if intersects(intent.Paths, other.Paths) {
				out.Hints = append(out.Hints, "same-file overlap with "+key+" (advisory; not an exclusive lock)")
			}
		}
	}
	coverage := s.coverage[scopeKey(in.Repo, in.Base)]
	through := coverage.Through
	out.Cost = cost(s.jobs, coverage, in.Repo, in.Base, in.Now.Add(-cfg.Window), through)
	if through.IsZero() || through.After(in.Now) || in.Now.Sub(through) > cfg.MaxObservationAge {
		out.Cost.Complete, out.Cost.RunnerMinutes, out.Cost.Reason = false, nil, "workflow coverage stale or unknown; admission growth frozen"
	}
	if !out.Cost.Complete {
		block(out.Cost.Reason)
	} else if *out.Cost.RunnerMinutes >= cfg.MaxRunnerMinutes || out.Cost.Jobs >= cfg.MaxJobs {
		block("fixed runner-minute/job admission budget exhausted")
	}
	active := 0
	reservedMinutes, reservedJobs := 0.0, 0
	for key, r := range s.reservations {
		i := s.intents[key]
		if key != in.Key && i.Repo == in.Repo && i.Base == in.Base && r.ExpiresAt.After(in.Now) {
			active++
			reservedMinutes += r.RunnerMinutes
			reservedJobs += r.Jobs
		}
	}
	if out.Cost.RunnerMinutes != nil && (*out.Cost.RunnerMinutes+reservedMinutes+cfg.AdmissionRunnerMinutes > cfg.MaxRunnerMinutes ||
		out.Cost.Jobs+reservedJobs+cfg.AdmissionJobs > cfg.MaxJobs) {
		block("scoped runner-minute/job allocations exhausted")
	}
	head := in.Head
	if head == "" {
		head = intent.Head
	}
	for _, job := range s.jobs {
		if head != "" && job.Repo == in.Repo && job.Base == in.Base && job.Head == head && job.Attempt >= cfg.MaxAttempts && job.Status == "completed" &&
			(job.Conclusion == "failure" || job.Conclusion == "cancelled" || job.Conclusion == "timed_out") {
			block("bounded CI attempts exhausted for exact head; owner repair required")
		}
	}
	if active >= cfg.MaxActive {
		block("scoped active admission limit")
	}
	if existing, ok := s.reservations[in.Key]; ok && existing.ExpiresAt.After(in.Now) &&
		(existing.Claim != in.Claim || existing.ClaimGeneration != in.ClaimGeneration) && in.Reserve {
		block("work already reserved by another live claim")
	}
	if cfg.Mode == config.IntegrationEnforce && out.WouldBlock {
		out.Allowed = false
	}
	if !out.Allowed || !in.Reserve || !found || cfg.Mode != config.IntegrationEnforce {
		return out, nil
	}
	if in.Claim == "" || in.ClaimGeneration == 0 || !in.ClaimExpiresAt.After(in.Now) {
		return out, fmt.Errorf("reservation requires a live authoritative claim generation")
	}
	expires := in.Now.Add(cfg.LeaseTTL)
	if in.ClaimExpiresAt.Before(expires) {
		expires = in.ClaimExpiresAt
	}
	r := Reservation{Key: in.Key, Claim: in.Claim, ClaimGeneration: in.ClaimGeneration, IntentGeneration: intent.Generation, ExpiresAt: expires,
		RunnerMinutes: cfg.AdmissionRunnerMinutes, Jobs: cfg.AdmissionJobs}
	err := s.appendLocked(Event{Version: SchemaVersion, ID: fmt.Sprintf("reserve/%s/%s/%d/%d", in.Key, in.Claim, in.ClaimGeneration, in.Now.UnixNano()), At: in.Now, Reservation: &r})
	return out, err
}

// Order is a stable authoring-admission cohort. It never reorders native queue
// entries. Oldest intents receive service first, independently per repo/base.
func (s *Store) Order(keys []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), keys...)
	sort.SliceStable(out, func(a, b int) bool {
		i, j := s.intents[out[a]], s.intents[out[b]]
		if i.ObservedAt.IsZero() {
			return false
		}
		if j.ObservedAt.IsZero() {
			return true
		}
		return i.ObservedAt.Before(j.ObservedAt)
	})
	return out
}

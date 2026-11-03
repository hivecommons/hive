package integration

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func (s *Store) executionData() *executionState {
	if s.execution == nil {
		s.execution = &executionState{
			plans: map[string]ExecutionPlan{}, proofs: map[string]ExecutionProof{},
			invalidations: map[string]ExecutionInvalidation{}, promotions: map[string]ExecutionPromotion{},
			statuses:       map[string]ExecutionStatus{},
			eligibility:    map[string]ExecutionEligibility{},
			qualifications: map[string]ExecutionQualification{},
		}
	}
	return s.execution
}

func validExecutionState(state State) bool {
	switch state {
	case Unknown, Waiting, Running, Passed, Rejected, Superseded:
		return true
	default:
		return false
	}
}

func (s *Store) validateExecution(e ExecutionEvent) error {
	if e.Version != ExecutionSchemaVersion {
		return fmt.Errorf("unknown candidate execution schema")
	}
	count := 0
	for _, present := range []bool{e.Plan != nil, e.Proof != nil, e.Invalidate != nil, e.Promotion != nil, e.Status != nil, e.Eligibility != nil, e.Qualification != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("execution event requires exactly one typed observation")
	}
	if q := e.Qualification; q != nil {
		if q.ID == "" || q.Repo == "" || q.Base == "" || q.Base == q.ProbeBase ||
			!strings.HasPrefix(q.ProbeBase, "hive-integration-qualification/") ||
			!executionHash(q.ExecutorID) || !executionObject(q.AuthorityCommit) ||
			q.RequestActorID <= 0 || !executionHash(q.Policy) || !executionObject(q.WorkflowTree) ||
			!executionHash(q.ProtectionDigest) || !executionHash(q.ProfileDigest) || !executionHash(q.SubjectID) ||
			!executionObject(q.TestedSHA) || q.MutationKey == "" || q.ObservedAt.IsZero() {
			return fmt.Errorf("qualification requires an observed exact protected isolated promotion, not a configured boolean")
		}
		if s.execution == nil {
			return fmt.Errorf("qualification has no observed promotion/proof lineage")
		}
		promotion, exists := s.execution.promotions[q.MutationKey]
		if !exists || promotion.State != "applied" || !promotion.ProviderAcknowledged || promotion.Repo != q.Repo ||
			promotion.Base != q.ProbeBase || promotion.SubjectID != q.SubjectID ||
			promotion.TestedSHA != q.TestedSHA || promotion.AuthorityCommit != q.AuthorityCommit {
			return fmt.Errorf("qualification is not bound to its actual applied isolated promotion")
		}
		verified := false
		for _, proof := range s.execution.proofs {
			if proof.Subject.ID == q.SubjectID && proof.Subject.Repo == q.Repo && proof.Subject.Base == q.ProbeBase &&
				proof.Subject.Policy == q.Policy && proof.Subject.WorkflowTree == q.WorkflowTree &&
				proof.State == Passed && proof.Archive != nil &&
				proof.Subject.Prefixes[len(proof.Subject.Prefixes)-1].Commit == q.TestedSHA {
				verified = true
				break
			}
		}
		if !verified {
			return fmt.Errorf("qualification lacks exact trusted producer evidence")
		}
	}
	if eligible := e.Eligibility; eligible != nil {
		if eligible.Repo == "" || eligible.Base == "" || eligible.PR <= 0 || !executionObject(eligible.Head) ||
			eligible.ReadyAt.IsZero() || eligible.ObservedAt.Before(eligible.ReadyAt) ||
			eligible.Source != "normal-automerge-lane-v1" && eligible.Source != "normal-lane-preparation-v1" {
			return fmt.Errorf("invalid exact normal-lane eligibility observation")
		}
	}
	if p := e.Plan; p != nil {
		if err := p.Recipe.Validate(); err != nil {
			return err
		}
		if p.Generation == 0 || p.CreatedAt.IsZero() || p.ObservedAt.Before(p.CreatedAt) ||
			p.AttemptAt.Before(p.CreatedAt) || p.AttemptAt.After(p.ObservedAt) ||
			!validExecutionState(p.State) || p.Priority < 0 || p.Priority > config.MaxIntegrationExecutionPriority ||
			p.Attempt < 1 || p.Attempt > config.DefaultIntegrationMaxAttempts ||
			!executionHash(p.ExecutorID) || !executionObject(p.AuthorityCommit) || len(p.WorkKeys) != len(p.Recipe.Members) ||
			len(p.WorkBindings) != len(p.WorkKeys) || p.RequestDigest != "" && !executionHash(p.RequestDigest) {
			return fmt.Errorf("invalid bounded execution plan")
		}
		keys := map[string]bool{}
		for index, key := range p.WorkKeys {
			if key == "" || keys[key] {
				return fmt.Errorf("execution requires unique canonical member work keys")
			}
			keys[key] = true
			if p.WorkBindings[index].Key != key || p.WorkBindings[index].IntentGeneration == 0 ||
				p.WorkBindings[index].PlanRevision < 0 {
				return fmt.Errorf("invalid exact approved-work generation binding")
			}
		}
		if p.Materialization != nil {
			if p.Materialization.Role != "materializer" {
				return fmt.Errorf("candidate receipt cannot attest materialization")
			}
			if err := validateArchivedIdentity(*p.Materialization, p.Recipe.Repo, p.MaterializerRunID, p.MaterializerAttempt); err != nil {
				return err
			}
			if err := validateMaterializationArchive(*p); err != nil {
				return err
			}
		} else if len(p.Prepared) != 0 {
			return fmt.Errorf("published subjects require private verified materializer history")
		}
		if s.execution != nil {
			if old, exists := s.execution.plans[p.Recipe.ID()]; exists && old.Generation == p.Generation {
				a, _ := json.Marshal(old)
				b, _ := json.Marshal(p)
				if string(a) != string(b) {
					return fmt.Errorf("execution plan generation reused")
				}
			}
		}
	}
	if p := e.Proof; p != nil {
		subject := p.Subject
		recipe := ExecutionRecipe{Repo: subject.Repo, Base: subject.Base, BaseSHA: subject.BaseSHA,
			Policy: subject.Policy, WorkflowTree: subject.WorkflowTree, Members: subject.Members}
		if err := recipe.Validate(); err != nil {
			return err
		}
		if subject.Schema != 1 || subject.Protocol != "integration-prefix-v1" || subject.Engine != "hive" ||
			!executionHash(subject.ID) ||
			len(subject.Prefixes) != len(subject.Members) || p.RunID <= 0 || p.Attempt < 1 ||
			p.ObservedAt.IsZero() || !validExecutionState(p.State) || p.Source != "github-actions-oidc-v1" {
			return fmt.Errorf("invalid exact execution proof identity")
		}
		for _, prefix := range subject.Prefixes {
			if !executionObject(prefix.Commit) || !executionObject(prefix.Tree) {
				return fmt.Errorf("invalid tested execution objects")
			}
		}
		if p.Archive != nil {
			if err := validateExecutionArchive(*p); err != nil {
				return err
			}
		} else if p.State == Passed {
			return fmt.Errorf("passed execution requires durable verified provenance")
		}
		if s.execution != nil {
			for _, old := range s.execution.proofs {
				if strings.EqualFold(old.Subject.Repo, subject.Repo) && old.RunID == p.RunID &&
					old.Attempt == p.Attempt && !old.Subject.SameCore(subject) {
					return fmt.Errorf("workflow attempt changed immutable candidate core")
				}
			}
		}
	}
	if i := e.Invalidate; i != nil && (i.Repo == "" || i.Base == "" || i.Revision == 0 || i.Reason == "") {
		return fmt.Errorf("invalid execution invalidation lineage")
	}
	if p := e.Promotion; p != nil {
		if p.Repo == "" || p.Base == "" || p.SubjectID == "" || p.MutationKey == "" ||
			p.PR <= 0 || !executionObject(p.Head) || p.Attempt < 1 || p.Attempt > config.DefaultIntegrationMaxAttempts || p.ObservedAt.IsZero() ||
			!executionObject(p.ExpectedSHA) || !executionObject(p.TestedSHA) || !executionObject(p.AuthorityCommit) ||
			p.State != "requested" && p.State != "applied" && p.State != "unknown" && p.State != "rejected" {
			return fmt.Errorf("invalid fenced promotion observation")
		}
	}
	if status := e.Status; status != nil && (status.Repo == "" || status.Base == "" || status.ObservedAt.IsZero() || status.Reason == "") {
		return fmt.Errorf("invalid execution runtime status")
	}
	return nil
}

func executionInvalidationKey(i ExecutionInvalidation) string {
	return fmt.Sprintf("%s/%d", scopeKey(i.Repo, i.Base), i.PR)
}

func invalidationAffects(i ExecutionInvalidation, p ExecutionProof) bool {
	if !strings.EqualFold(i.Repo, p.Subject.Repo) || i.Base != p.Subject.Base {
		return false
	}
	if i.PR == 0 {
		return i.Policy == "" || i.Policy != p.Subject.Policy
	}
	for _, member := range p.Subject.Members {
		if member.PR == i.PR {
			return i.Head == "" || i.Head != member.Head
		}
	}
	return false
}

func (s *Store) applyExecution(event Event) {
	state := s.executionData()
	e := event.Execution
	if q := e.Qualification; q != nil {
		if old, exists := state.qualifications[q.ID]; !exists || q.ObservedAt.After(old.ObservedAt) {
			state.qualifications[q.ID] = *q
		}
	}
	if eligible := e.Eligibility; eligible != nil {
		if old, exists := state.eligibility[eligible.Key()]; !exists || !eligible.ObservedAt.Before(old.ObservedAt) {
			next := *eligible
			if exists && old.Head == next.Head && old.WorkKey == next.WorkKey {
				next.ReadyAt = old.ReadyAt
			}
			state.eligibility[next.Key()] = next
		}
	}
	if p := e.Plan; p != nil {
		id := p.Recipe.ID()
		if old, exists := state.plans[id]; !exists || p.Generation > old.Generation {
			state.plans[id] = cloneExecution(*p)
		}
	}
	if p := e.Proof; p != nil {
		next := cloneExecution(*p)
		next.ObservedState = p.State
		for _, old := range state.proofs {
			if strings.EqualFold(old.Subject.Repo, next.Subject.Repo) && old.RunID == next.RunID && old.Attempt > next.Attempt {
				next.State, next.InvalidatedBy, next.Reason = Superseded, old.Key(), "newer workflow attempt already observed"
			}
		}
		for key, old := range state.proofs {
			if strings.EqualFold(old.Subject.Repo, next.Subject.Repo) && old.RunID == next.RunID && old.Attempt < next.Attempt {
				old.State, old.InvalidatedBy, old.Reason = Superseded, event.ID, "authoritative workflow attempt superseded"
				state.proofs[key] = old
			}
		}
		for key, invalidation := range state.invalidations {
			if invalidationAffects(invalidation, next) && next.EligibilityRevision < invalidation.Revision {
				next.State, next.InvalidatedBy, next.Reason = Superseded, key, invalidation.Reason
			}
		}
		old, exists := state.proofs[next.Key()]
		if !exists || next.ObservedAt.After(old.ObservedAt) ||
			next.ObservedAt.Equal(old.ObservedAt) && evidenceStateRank(next.State) > evidenceStateRank(old.State) {
			if exists && old.State == Superseded && next.EligibilityRevision <= old.EligibilityRevision {
				next.State, next.InvalidatedBy, next.Reason = Superseded, old.InvalidatedBy, old.Reason
			}
			state.proofs[next.Key()] = next
		}
	}
	if i := e.Invalidate; i != nil {
		key := executionInvalidationKey(*i)
		old, exists := state.invalidations[key]
		if !exists || i.Revision > old.Revision {
			state.invalidations[key] = *i
			for id, proof := range state.proofs {
				if invalidationAffects(*i, proof) && proof.EligibilityRevision < i.Revision {
					proof.State, proof.InvalidatedBy, proof.Reason = Superseded, event.ID, i.Reason
					state.proofs[id] = proof
				}
			}
		}
	}
	if p := e.Promotion; p != nil {
		old, exists := state.promotions[p.MutationKey]
		if !exists || old.State != "applied" {
			state.promotions[p.MutationKey] = *p
		}
	}
	if status := e.Status; status != nil {
		key := scopeKey(status.Repo, status.Base)
		if old, exists := state.statuses[key]; !exists || !status.ObservedAt.Before(old.ObservedAt) {
			state.statuses[key] = *status
		}
	}
}

func cloneExecution[T any](value T) T {
	data, _ := json.Marshal(value)
	var out T
	_ = json.Unmarshal(data, &out)
	return out
}

func (s *Store) AppendExecution(id string, at time.Time, observation ExecutionEvent) error {
	return s.Append(Event{Version: SchemaVersion, ID: id, At: at, Execution: &observation})
}

// ExecutionSnapshot omits private receipt bytes/tokens/keys from API projections.
func (s *Store) ExecutionSnapshot() ExecutionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ExecutionSnapshot{Version: ExecutionSchemaVersion, Plans: []ExecutionPlan{}, Proofs: []ExecutionProof{},
		Invalidations: []ExecutionInvalidation{}, Promotions: []ExecutionPromotion{}, Statuses: []ExecutionStatus{}, Eligibility: []ExecutionEligibility{}, Qualifications: []ExecutionQualification{}}
	if s.execution == nil {
		return out
	}
	for _, plan := range s.execution.plans {
		plan = cloneExecution(plan)
		plan.Materialization = nil
		out.Plans = append(out.Plans, plan)
	}
	for _, proof := range s.execution.proofs {
		proof = cloneExecution(proof)
		proof.Archive = nil
		out.Proofs = append(out.Proofs, proof)
	}
	for _, invalidation := range s.execution.invalidations {
		out.Invalidations = append(out.Invalidations, invalidation)
	}
	for _, promotion := range s.execution.promotions {
		out.Promotions = append(out.Promotions, promotion)
	}
	for _, status := range s.execution.statuses {
		out.Statuses = append(out.Statuses, status)
	}
	for _, eligible := range s.execution.eligibility {
		out.Eligibility = append(out.Eligibility, eligible)
	}
	for _, qualified := range s.execution.qualifications {
		out.Qualifications = append(out.Qualifications, qualified)
	}
	sort.Slice(out.Plans, func(i, j int) bool { return out.Plans[i].Recipe.ID() < out.Plans[j].Recipe.ID() })
	sort.Slice(out.Proofs, func(i, j int) bool { return out.Proofs[i].Key() < out.Proofs[j].Key() })
	sort.Slice(out.Statuses, func(i, j int) bool {
		return scopeKey(out.Statuses[i].Repo, out.Statuses[i].Base) < scopeKey(out.Statuses[j].Repo, out.Statuses[j].Base)
	})
	sort.Slice(out.Eligibility, func(i, j int) bool { return out.Eligibility[i].Key() < out.Eligibility[j].Key() })
	sort.Slice(out.Qualifications, func(i, j int) bool { return out.Qualifications[i].ID < out.Qualifications[j].ID })
	return out
}

func (s *Store) ExecutionProof(subject string, runID int64, attempt int) (ExecutionProof, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execution != nil {
		for _, proof := range s.execution.proofs {
			if proof.Subject.ID == subject && proof.RunID == runID && proof.Attempt == attempt {
				return cloneExecution(proof), true
			}

		}
	}
	return ExecutionProof{}, false
}

// ExecutionPlan returns private producer history for reconciliation, never an
// HTTP projection. Public snapshots deliberately strip that history.
func (s *Store) ExecutionPlan(id string) (ExecutionPlan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execution != nil {
		plan, exists := s.execution.plans[id]
		if exists {
			return cloneExecution(plan), true
		}
	}
	return ExecutionPlan{}, false
}

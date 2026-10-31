package forgestate

import "time"

// IntegrationCandidateSubject identifies one immutable, ordered tested prefix.
// Generation and producer/dispatch metadata do not change its immutable core.
type IntegrationCandidateSubject struct {
	Schema       int                          `json:"schema"`
	Protocol     string                       `json:"protocol"`
	Engine       string                       `json:"engine"`
	Repo         string                       `json:"repo"`
	Base         string                       `json:"base"`
	BaseSHA      string                       `json:"baseSHA"`
	Policy       string                       `json:"policy"`
	WorkflowTree string                       `json:"workflowTree"`
	Generation   int64                        `json:"generation"`
	Members      []IntegrationCandidateMember `json:"members"`
	Prefixes     []IntegrationCandidatePrefix `json:"prefixes"`
	ID           string                       `json:"id"`
	PreparedBy   IntegrationPreparationSource `json:"preparedBy"`
	Claim        *IntegrationDispatchClaim    `json:"dispatchClaim,omitempty"`
}

type IntegrationCandidateMember struct {
	PR   int    `json:"pr"`
	Head string `json:"head"`
}

type IntegrationCandidatePrefix struct {
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
}

type IntegrationPreparationSource struct {
	SourceSHA     string `json:"sourceSHA"`
	RequestDigest string `json:"requestDigest"`
	GitVersion    string `json:"gitVersion"`
}

type IntegrationDispatchClaim struct {
	State  string                   `json:"state"`
	Key    string                   `json:"key"`
	Ref    string                   `json:"ref"`
	Commit string                   `json:"commit"`
	Owner  IntegrationDispatchOwner `json:"owner"`
}

type IntegrationDispatchOwner struct {
	RunID   string `json:"runID"`
	Attempt string `json:"attempt"`
	Nonce   string `json:"nonce"`
}

// SameCore never transfers evidence between different base/member/parent/tree
// identities. Reobserving an identical prefix with a new lease is not a change.
func (s IntegrationCandidateSubject) SameCore(other IntegrationCandidateSubject) bool {
	if s.Schema != other.Schema || s.Protocol != other.Protocol || s.Engine != other.Engine ||
		s.Repo != other.Repo || s.Base != other.Base || s.BaseSHA != other.BaseSHA ||
		s.Policy != other.Policy || s.WorkflowTree != other.WorkflowTree || s.ID != other.ID ||
		len(s.Members) != len(other.Members) || len(s.Prefixes) != len(other.Prefixes) {
		return false
	}
	for i := range s.Members {
		if s.Members[i] != other.Members[i] {
			return false
		}
	}
	for i := range s.Prefixes {
		if s.Prefixes[i] != other.Prefixes[i] {
			return false
		}
	}
	return true
}

// IntegrationRunnerJob is an actual assigned-runner observation, not an
// estimate from local execution or queued/run-duration timestamps.
type IntegrationRunnerJob struct {
	Repo        string    `json:"repo"`
	RunID       int64     `json:"run_id"`
	Attempt     int       `json:"attempt"`
	JobID       int64     `json:"job_id"`
	RunnerID    int64     `json:"runner_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	ObservedAt  time.Time `json:"observed_at"`
}

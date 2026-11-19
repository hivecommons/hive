package sandbox

import "fmt"

// Failure reasons a launcher reports in Result.FailReason (hivecommons/hive#18811).
// Both mean the run never reached the agent through no fault of the agent, so
// the executor and the manager do not count them as agent failures.
const (
	// FailReasonNodeUnhealthy: the node preflight (sandbox.preflight) failed
	// on the node the Job landed on.
	FailReasonNodeUnhealthy = "node_unhealthy"
	// FailReasonWaitingForNode: the Job's pod stayed Unschedulable past
	// job.schedule_timeout_s, so hive deleted the Job.
	FailReasonWaitingForNode = "waiting_for_node"
	// ExitCodeNodeUnhealthy is the exit code of a Job container whose node
	// preflight failed (EX_TEMPFAIL).
	ExitCodeNodeUnhealthy = 75
)

// FailError is the error a launcher returns with a Result whose FailReason is
// set. Message is the preflight failure line or the scheduler's message.
type FailError struct {
	Reason  string
	Node    string
	Message string
}

func (e *FailError) Error() string {
	if e.Node != "" {
		return fmt.Sprintf("sandbox: %s on node %s: %s", e.Reason, e.Node, e.Message)
	}
	return fmt.Sprintf("sandbox: %s: %s", e.Reason, e.Message)
}

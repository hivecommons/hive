package sandbox

import "fmt"

// Failure reasons a launcher reports in Result.FailReason (hivecommons/hive#18811).
// node_unhealthy and waiting_for_node mean the run never reached the agent
// through no fault of the agent, so the manager does not count them as agent
// failures; setup_failed (#19574) also never reached the agent but is a
// failed kick.
const (
	// FailReasonNodeUnhealthy: the node preflight (sandbox.preflight), or a
	// sandbox.setup health command, failed on the node the Job landed on.
	FailReasonNodeUnhealthy = "node_unhealthy"
	// FailReasonWaitingForNode: the Job's pod stayed Unschedulable past
	// job.schedule_timeout_s, so hive deleted the Job.
	FailReasonWaitingForNode = "waiting_for_node"
	// ExitCodeNodeUnhealthy is the exit code of a Job container whose node
	// preflight failed (EX_TEMPFAIL).
	ExitCodeNodeUnhealthy = 75

	// FailReasonSetupFailed: a sandbox.setup command failed before the agent
	// started (hivecommons/hive#19574). The agent never ran, but unlike the
	// two above the cause is the repo's setup script or what it installs
	// from, so the kick fails and is not retried on another node.
	FailReasonSetupFailed = "setup_failed"
	// ExitCodeSetupFailed is the exit code of a Job container whose setup
	// command failed (EX_SOFTWARE). RelayPreflightExitCode already takes 78.
	ExitCodeSetupFailed = 70
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

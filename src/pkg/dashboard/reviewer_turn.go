package dashboard

import (
	"fmt"
	"os/exec"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/pkg/config"
)

// The one-shot reviewer turns (Let hive decide, grounded chat) can run
// through the Claude CLI when the hive has no reviewer endpoint
// (governor.reviewer_backend, config.ResolveReviewerTurn; the runner is
// cmd/hive/reviewer_cli_runner.go). That CLI runs the same way the
// Spektacular hub executor runs its stage CLI: as its own UID-map user via
// su-exec, with the egress-proxy settings, in its own process group. These
// exported wrappers hand the runner those helpers, and readiness below uses
// the same checks, so the dashboard reports ready only when the turn can run.

// ReviewerTurnIdentity is the UID-map identity the reviewer CLI turn runs as.
// The entrypoint provisions it (user hive-hive-turn) beside the per-agent
// users, without a workspace or a token group.
const ReviewerTurnIdentity = "hive-turn"

// reviewerTurnExecUserSpec is a seam for tests.
var reviewerTurnExecUserSpec = func() (string, error) {
	return spekHubExecUserSpec(ReviewerTurnIdentity)
}

// ReviewerTurnExecUser returns the su-exec user spec the reviewer CLI turn
// runs as, or "" when this deployment has no per-agent UID isolation (the
// CLI then runs as the hive uid). With isolation on and no such user, it is
// an error rather than a fallback to the hive uid.
func ReviewerTurnExecUser() (string, error) {
	spec, err := reviewerTurnExecUserSpec()
	if err != nil {
		return "", fmt.Errorf("reviewer turn user: %w", err)
	}
	return spec, nil
}

// ReviewerTurnExecEnv adds the egress-proxy settings for the reviewer turn's
// own user (spekHubExecUserEnv).
func ReviewerTurnExecEnv(env []string) []string {
	return spekHubExecUserEnv(env, ReviewerTurnIdentity)
}

// ReviewerTurnRunAs wraps argv to run as spec with HOME=home and umask 002
// (spekHubRunAsCommand).
func ReviewerTurnRunAs(argv []string, spec, home string) []string {
	return spekHubRunAsCommand(argv, spec, home)
}

// ReviewerTurnConfigureProcess puts the turn in its own process group that a
// timeout kills, as spec when it runs as its own user.
func ReviewerTurnConfigureProcess(c *exec.Cmd, spec string) {
	spekHubConfigureProcessGroup(c, spec)
}

// ReviewerTurnReap kills what is left of the turn's process group.
func ReviewerTurnReap(c *exec.Cmd, spec string) {
	spekHubKillProcessGroup(c, spec)
}

// claudeSignedIn is the Claude login check GET /api/claude-auth/status
// reports as logged_in: the operator's long-lived token, or an unexpired
// dashboard sign-in. The reviewer CLI runner reads the same two sources.
func claudeSignedIn() bool {
	return claude.AuthMode() == claude.AuthModeLongLivedToken || claudeReadToken(claudeCredPath) != ""
}

// reviewerTurnRoute resolves where a reviewer turn with modelOverride goes,
// and whether it can run there: on the Claude CLI path that also needs the
// turn's own user when UID isolation is on.
func reviewerTurnRoute(cfg *config.Config, modelOverride string) config.ReviewerTurnRoute {
	route := cfg.Governor.ResolveReviewerTurn(modelOverride, claudeSignedIn())
	if route.Path == config.ReviewerPathClaudeCLI && route.Ready {
		if _, err := ReviewerTurnExecUser(); err != nil {
			route.Ready = false
			route.Reason = err.Error()
		}
	}
	return route
}

package kubejob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/sandbox"
)

// retryJobSuffix names the re-created Job after a node_unhealthy kick.
const retryJobSuffix = "-r"

// runWithRetry runs the kick as a Job and, when its node preflight fails, runs
// it once more on a different node (#18811). Only an agent kick (Relays set)
// gets a preflight: the publish.verify launches share this launcher and must
// not be gated on the agent's node health.
func (l *Launcher) runWithRetry(ctx context.Context, spec sandbox.LaunchSpec, ns, subPath string, relays *relayPlan) (sandbox.Result, error) {
	if !spec.Relays && len(l.Options.Preflight) > 0 {
		cp := *l
		cp.Options.Preflight = nil
		l = &cp
	}
	first := jobName(spec.Name, l.now())
	var avoid []string
	var earlier strings.Builder
	for attempt := 0; ; attempt++ {
		name := first
		if attempt > 0 {
			name = retryJobName(first, attempt)
		}
		res, err := l.runAttempt(ctx, spec, ns, subPath, name, relays, avoid)
		if res.FailReason != sandbox.FailReasonNodeUnhealthy || attempt >= maxPreflightRetries || res.Node == "" || ctx.Err() != nil {
			if earlier.Len() > 0 {
				res.Stdout = earlier.String() + res.Stdout
			}
			return res, err
		}
		detail := ""
		var fe *sandbox.FailError
		if errors.As(err, &fe) {
			detail = fe.Message
		}
		fmt.Fprintf(&earlier, "hive: attempt %d on node %s failed the node preflight; retrying on another node\n%s\n\n", attempt+1, res.Node, res.Stdout)
		l.emit(Event{Kind: EventNodeRetry, Node: res.Node, Detail: detail})
		avoid = append(avoid, res.Node)
		if d := l.Options.PreflightRetryDelay; d > 0 {
			select {
			case <-ctx.Done():
				res.Stdout = earlier.String()
				return res, fmt.Errorf("kubejob: %w", ctx.Err())
			case <-time.After(d):
			}
		}
	}
}

// retryJobName is the Job name for the n-th re-creation, kept within jobNameMax.
func retryJobName(first string, n int) string {
	suffix := fmt.Sprintf("%s%d", retryJobSuffix, n)
	if room := jobNameMax - len(suffix); len(first) > room {
		first = strings.TrimRight(first[:room], "-")
	}
	return first + suffix
}

// runAttempt creates one Job, waits for it, collects the pod's log and exit
// code, and deletes the Job on success. A Job that failed is left in place
// until its TTL so the operator can inspect it; one that never got a node is
// deleted. avoid lists nodes the pod must not run on (a failed preflight's).
func (l *Launcher) runAttempt(ctx context.Context, spec sandbox.LaunchSpec, ns, subPath, name string, relays *relayPlan, avoid []string) (sandbox.Result, error) {
	var res sandbox.Result
	manifest := l.jobManifest(ns, name, subPath, spec, relays)
	setPullSecrets(manifest, l.resolvePullSecrets(ctx, ns, relays))
	if jobSpec, ok := manifest["spec"].(map[string]any); ok {
		jobSpec["activeDeadlineSeconds"] = l.activeDeadline(ctx)
	}
	avoidNodes(manifest, l.Options.Affinity, avoid)
	body, err := json.Marshal(manifest)
	if err != nil {
		return res, err
	}
	jobsPath := fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs", ns)
	policyPath := ""
	if l.Options.network() == NetworkNone {
		// The policy exists before any pod can: a Job created first could
		// start its pod in the window before the policy lands.
		policiesPath := fmt.Sprintf("/apis/networking.k8s.io/v1/namespaces/%s/networkpolicies", ns)
		policy, err := json.Marshal(denyAllPolicy(ns, name))
		if err != nil {
			return res, err
		}
		if _, err := l.do(ctx, http.MethodPost, policiesPath, policy); err != nil {
			return res, fmt.Errorf("kubejob: network %q: creating the deny-all NetworkPolicy for %s: %w", NetworkNone, name, err)
		}
		policyPath = policiesPath + "/" + name
	}
	if _, err := l.do(ctx, http.MethodPost, jobsPath, body); err != nil {
		if policyPath != "" {
			_ = l.delete(ctx, policyPath)
		}
		return res, fmt.Errorf("kubejob: creating job %s: %w", name, err)
	}
	jobPath := jobsPath + "/" + name

	done, waitErr := l.waitJob(ctx, ns, name, jobPath)
	// Collect whatever the pod produced BEFORE deciding anything: a timed-out
	// or failed kick with no transcript is the worst outcome for an operator.
	// The caller's ctx may be dead already (kick timeout) or die between any
	// two of these requests (cancel during log collection), so every step
	// re-checks it and falls back to a fresh, request-bounded context.
	// Otherwise a readable, Complete pod would report exitCodeUnknown merely
	// because ctx was cancelled after the log was read.
	var cancels []context.CancelFunc
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	live := func() context.Context {
		if ctx.Err() == nil {
			return ctx
		}
		fresh, cancel := context.WithTimeout(context.Background(), requestTimeout)
		cancels = append(cancels, cancel)
		return fresh
	}
	if waitErr == nil && done.failReason == sandbox.FailReasonWaitingForNode {
		// No node took the pod in time: there is no log to read and nothing
		// to inspect, so the Job goes and the agent's slot is released.
		_ = l.delete(live(), jobPath)
		l.deletePolicy(live(), policyPath)
		res.ExitCode = exitCodeUnknown
		res.FailReason = done.failReason
		res.Stderr = done.reason
		msg := fmt.Sprintf("no node took the pod within %s: %s", l.Options.scheduleTimeout(), done.reason)
		return res, &sandbox.FailError{Reason: done.failReason, Message: msg}
	}
	pod, podErr := l.findPod(live(), ns, name)
	if podErr == nil && pod != "" {
		res.Stdout = l.podLog(live(), ns, pod)
		info := l.podDetails(live(), ns, pod)
		res.ExitCode, res.Node = info.exitCode, info.node
	} else {
		res.ExitCode = exitCodeUnknown
	}
	if res.Node == "" {
		res.Node = done.node
	}
	if waitErr != nil {
		// Context ended (kick timeout or cancel). The Job would otherwise
		// keep running the workload on the accelerator node; stop it.
		_ = l.delete(live(), jobPath)
		l.deletePolicy(live(), policyPath)
		return res, waitErr
	}
	if !done.succeeded {
		// The Job stays for inspection; its pods are finished, so the
		// policy has nothing left to isolate and goes.
		l.deletePolicy(live(), policyPath)
		res.Stderr = done.reason
		if msg, bad := l.Options.preflightFailure(res); bad {
			res.FailReason = sandbox.FailReasonNodeUnhealthy
			return res, &sandbox.FailError{Reason: res.FailReason, Node: res.Node, Message: msg}
		}
		return res, fmt.Errorf("kubejob: job %s failed: %s", name, done.reason)
	}
	// When the Job reports Complete but the pod could not be read back
	// (already GC'd, or RBAC on pods missing), res.ExitCode stays
	// exitCodeUnknown rather than a guessed 0; callers treat a nil error with
	// an unknown code as completed-without-code.
	//
	// Collection may have expired the context even on success; the fresh
	// context ensures the finished Job is still removed immediately.
	_ = l.delete(live(), jobPath)
	l.deletePolicy(live(), policyPath)
	return res, nil
}

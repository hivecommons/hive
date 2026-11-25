package kubejob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/sandbox"
)

// retryJobSuffix names the re-created Job after a node_unhealthy kick.
const retryJobSuffix = "-r"

// runWithRetry runs the kick as a Job and, when its node preflight or a setup
// health command fails, runs it once more on a different node (#18811,
// #19574). Only an agent kick (Relays set) gets a preflight or a setup step:
// the publish.verify launches share this launcher and must not be gated on
// the agent's node health or re-run its install.
func (l *Launcher) runWithRetry(ctx context.Context, spec sandbox.LaunchSpec, ns, subPath string, relays *relayPlan) (sandbox.Result, error) {
	if !spec.Relays && (len(l.Options.Preflight) > 0 || l.Options.setupEnabled()) {
		cp := *l
		cp.Options.Preflight = nil
		cp.Options.Setup, cp.Options.SetupHealth = nil, nil
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
		fmt.Fprintf(&earlier, "hive: attempt %d on node %s failed the node preflight or a setup health check; retrying on another node\n%s\n\n", attempt+1, res.Node, res.Stdout)
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
	pullFrom := relays
	if l.copyPlan != nil {
		if err := l.copyPlan.apply(manifest, name); err != nil {
			return res, err
		}
		pullFrom = l.copyPlan.image
	}
	setPullSecrets(manifest, l.resolvePullSecrets(ctx, ns, pullFrom))
	if jobSpec, ok := manifest["spec"].(map[string]any); ok {
		jobSpec["activeDeadlineSeconds"] = l.activeDeadline(ctx)
	}
	avoidNodes(manifest, l.Options.Affinity, avoid)
	body, err := json.Marshal(manifest)
	if err != nil {
		return res, err
	}
	jobsPath := fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs", ns)
	jobPath := jobsPath + "/" + name
	var policyPath, secretPath string
	cleanup := func(c context.Context) {
		l.deleteQuiet(c, policyPath)
		l.deleteQuiet(c, secretPath)
	}
	if l.copyPlan != nil {
		// The Job is created suspended, then its Secret and NetworkPolicy,
		// which carry an ownerReference to it, then it is resumed.
		var err error
		if secretPath, policyPath, err = l.createPerJobObjects(ctx, ns, name, jobsPath, body); err != nil {
			return res, err
		}
	} else {
		policiesPath := fmt.Sprintf("/apis/networking.k8s.io/v1/namespaces/%s/networkpolicies", ns)
		if l.Options.network() == NetworkNone {
			// The policy exists before any pod can: a Job created first could
			// start its pod in the window before the policy lands.
			policy, err := json.Marshal(denyAllPolicy(ns, name))
			if err != nil {
				return res, err
			}
			if _, err := l.do(ctx, http.MethodPost, policiesPath, policy); err != nil {
				return res, fmt.Errorf("kubejob: creating the NetworkPolicy for %s: %w", name, err)
			}
			policyPath = policiesPath + "/" + name
		}
		if _, err := l.do(ctx, http.MethodPost, jobsPath, body); err != nil {
			cleanup(ctx)
			return res, fmt.Errorf("kubejob: creating job %s: %w", name, err)
		}
	}

	waitCtx, abortWait := context.WithCancel(ctx)
	defer abortWait()
	var copying *copyRun
	if l.copyPlan != nil {
		copying = l.startCopy(ctx, abortWait, ns, name, spec.Workspace)
	}
	done, waitErr := l.waitJob(waitCtx, ns, name, jobPath)
	copyErr := copying.finish()
	if waitErr != nil && ctx.Err() == nil && copyErr != nil {
		// The copy failed and stopped the wait: that is the error.
		waitErr = copyErr
	}
	var kept *pullError
	if waitErr != nil && ctx.Err() != nil && copying != nil {
		// Kick timeout or cancel: the agent's work is in the Job's emptyDir
		// and ctx is dead. Stop the command and pull on a fresh context
		// before the Job is deleted below.
		if err := l.rescueWorkspace(copying, name, spec.Workspace); err != nil {
			slog.Warn("kubejob: could not pull the workspace after the kick ended; it is lost with the Job", "job", name, "err", err)
		}
	}
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
		cleanup(live())
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
		if errors.As(waitErr, &kept) {
			// The workspace could not be pulled: it exists only in the Job,
			// so the Job stays, as a failed kick's does. Its Secret and
			// NetworkPolicy are owned by it and go with it.
			return res, waitErr
		}
		// Context ended (kick timeout or cancel). The Job would otherwise
		// keep running the workload on the accelerator node; stop it.
		_ = l.delete(live(), jobPath)
		cleanup(live())
		return res, waitErr
	}
	if !done.succeeded {
		// The Job stays for inspection; its pods are finished, so the
		// policy has nothing left to isolate and goes.
		cleanup(live())
		res.Stderr = done.reason
		if msg, bad := l.Options.preflightFailure(res); bad {
			res.FailReason = sandbox.FailReasonNodeUnhealthy
			return res, &sandbox.FailError{Reason: res.FailReason, Node: res.Node, Message: msg}
		}
		if msg, bad := l.Options.setupFailure(res); bad {
			// Not the node's fault: no retry elsewhere (#19574).
			res.FailReason = sandbox.FailReasonSetupFailed
			return res, &sandbox.FailError{Reason: res.FailReason, Node: res.Node, Message: msg}
		}
		return res, fmt.Errorf("kubejob: job %s failed: %s", name, done.reason)
	}
	if copyErr != nil {
		// The kick finished but its workspace never came back. If it was in
		// the Job, the Job stays (its Secret and NetworkPolicy are owned by
		// it); otherwise nothing ran, and it goes.
		if !errors.As(copyErr, &kept) {
			_ = l.delete(live(), jobPath)
			cleanup(live())
		}
		return res, copyErr
	}
	// When the Job reports Complete but the pod could not be read back
	// (already GC'd, or RBAC on pods missing), res.ExitCode stays
	// exitCodeUnknown rather than a guessed 0; callers treat a nil error with
	// an unknown code as completed-without-code.
	//
	// Collection may have expired the context even on success; the fresh
	// context ensures the finished Job is still removed immediately.
	_ = l.delete(live(), jobPath)
	cleanup(live())
	return res, nil
}

// createPerJobObjects creates a copy-transport Job suspended, then its token
// Secret and its NetworkPolicy, each with an ownerReference to the Job so the
// garbage collector removes them with it even after a hive restart or crash,
// then resumes the Job. It returns the Secret's and the policy's paths. A
// failure removes what it created; a Job left suspended by a crash is an
// unfinished Job the boot sweep deletes once it is old enough.
func (l *Launcher) createPerJobObjects(ctx context.Context, ns, name, jobsPath string, jobBody []byte) (secretPath, policyPath string, err error) {
	created, err := l.do(ctx, http.MethodPost, jobsPath, jobBody)
	if err != nil {
		return "", "", fmt.Errorf("kubejob: creating job %s: %w", name, err)
	}
	jobPath := jobsPath + "/" + name
	fail := func(err error) (string, string, error) {
		l.deleteQuiet(ctx, policyPath)
		l.deleteQuiet(ctx, secretPath)
		_ = l.delete(ctx, jobPath)
		return "", "", err
	}
	uid := jobUID(created)
	if uid == "" {
		return fail(fmt.Errorf("kubejob: job %s: the API server returned no uid, so its Secret and NetworkPolicy cannot be owned by it", name))
	}
	secretBody := l.copyPlan.tokenSecret(ns, name)
	ownedBy(secretBody, name, uid)
	secret, err := json.Marshal(secretBody)
	if err != nil {
		return fail(err)
	}
	secretsPath := fmt.Sprintf("/api/v1/namespaces/%s/secrets", ns)
	if _, err := l.do(ctx, http.MethodPost, secretsPath, secret); err != nil {
		return fail(fmt.Errorf("kubejob: creating the bridge token Secret for %s: %w", name, err))
	}
	secretPath = secretsPath + "/" + name

	var policyBody map[string]any
	if l.Options.network() == NetworkNone {
		policyBody = denyAllPolicy(ns, name)
		l.copyPlan.allowBridgeIngress(policyBody)
	} else {
		policyBody = l.copyPlan.ingressOnlyPolicy(ns, name)
	}
	ownedBy(policyBody, name, uid)
	policy, err := json.Marshal(policyBody)
	if err != nil {
		return fail(err)
	}
	policiesPath := fmt.Sprintf("/apis/networking.k8s.io/v1/namespaces/%s/networkpolicies", ns)
	if _, err := l.do(ctx, http.MethodPost, policiesPath, policy); err != nil {
		return fail(fmt.Errorf("kubejob: creating the NetworkPolicy for %s: %w", name, err))
	}
	policyPath = policiesPath + "/" + name

	resume, _ := json.Marshal(map[string]any{"spec": map[string]any{"suspend": false}})
	if err := l.patch(ctx, jobPath, resume); err != nil {
		return fail(fmt.Errorf("kubejob: resuming job %s (needs the patch verb on jobs): %w", name, err))
	}
	return secretPath, policyPath, nil
}

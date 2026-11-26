package kubejob

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sweep reasons, as reported on each SweptJob.
const (
	SweepReasonPendingPastDeadline = "pending past its deadline"
	SweepReasonOrphaned            = "not tied to a live kick and older than the kick timeout"
)

// Launch-name shapes of the Jobs the agent sandbox creates: a kick is
// hive-<agent>-sandbox, a publish preflight hive-<agent>-verify. Contained
// runs (crux/runs) share the managed-by label but are named crux-run-<id>
// and belong to the runs service, so the sweep leaves them alone.
const (
	sandboxJobAgentPrefix  = "hive-"
	sandboxJobKickSuffix   = "-sandbox"
	sandboxJobVerifySuffix = "-verify"
)

// SweepOptions bounds an orphan sweep.
type SweepOptions struct {
	// MaxAge is the kick timeout: an unfinished Job older than this that no
	// live kick owns is an orphan. Non-positive uses DefaultKickTimeout.
	MaxAge time.Duration
	// Live reports whether a live kick owns the Job with this agent label
	// (hive-<agent>-sandbox / -verify). Nil treats nothing as live, which is
	// the truth at boot: the kicks of the previous hive process are gone.
	Live func(agentLabel string) bool
}

// SweptJob is one Job the sweep deleted.
type SweptJob struct {
	Name   string
	Agent  string
	Age    time.Duration
	Reason string
}

type jobList struct {
	Items []struct {
		Metadata struct {
			Name              string            `json:"name"`
			CreationTimestamp time.Time         `json:"creationTimestamp"`
			Labels            map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			Active     int `json:"active"`
			Ready      int `json:"ready"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// SweepOrphans deletes sandbox Jobs hive lost track of (hivecommons/hive#18525):
// a Job's in-memory kick timeout dies with the hive process, and
// ttlSecondsAfterFinished only reaps a Job that finished, so a Job left
// Pending by a restart could sit there for days. It lists Jobs labelled
// app.kubernetes.io/managed-by=hive-kubejob in the hive's namespace and
// deletes each unfinished agent-sandbox Job that no live kick owns and that
// is older than the kick timeout, and each one that never got a ready pod
// and is past the kick deadline (timeout plus DeadlineMargin). Finished Jobs
// are left to their TTL so an operator can still inspect a failure; Jobs
// without the label, and contained runs, are never touched. Deletion errors do not stop the sweep; the first
// is returned with whatever was deleted.
func (l *Launcher) SweepOrphans(ctx context.Context, opts SweepOptions) ([]SweptJob, error) {
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultKickTimeout
	}
	pendingDeadline := time.Duration(ActiveDeadlineSeconds(maxAge)) * time.Second
	ns, err := l.namespace()
	if err != nil {
		return nil, err
	}
	jobsPath := fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs", ns)
	selector := url.QueryEscape(fmt.Sprintf(labelSelectorFmt, managedByLabel, managedByValue))
	raw, err := l.do(ctx, http.MethodGet, jobsPath+"?labelSelector="+selector, nil)
	if err != nil {
		return nil, fmt.Errorf("kubejob: listing sandbox jobs: %w", err)
	}
	var list jobList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("kubejob: decoding sandbox job list: %w", err)
	}
	now := l.now()
	var swept []SweptJob
	var firstErr error
	for _, item := range list.Items {
		md := item.Metadata
		// The selector already asked for this; check again so a server that
		// ignored it can never get an unlabelled Job deleted.
		if md.Labels[managedByLabel] != managedByValue || md.Name == "" {
			continue
		}
		agent := md.Labels[agentLabel]
		if !isAgentSandboxJob(agent) {
			continue
		}
		finished := false
		for _, c := range item.Status.Conditions {
			if (c.Type == condComplete || c.Type == condFailed) && c.Status == condStatusTrue {
				finished = true
			}
		}
		if finished {
			continue
		}
		if md.CreationTimestamp.IsZero() {
			continue
		}
		age := now.Sub(md.CreationTimestamp)
		var reason string
		switch {
		case item.Status.Ready == 0 && age > pendingDeadline:
			// Never got a ready pod and outlived any kick's deadline: no
			// kick can still be waiting on it.
			reason = SweepReasonPendingPastDeadline
		case age > maxAge && (opts.Live == nil || !opts.Live(agent)):
			reason = SweepReasonOrphaned
		default:
			continue
		}
		if err := l.delete(ctx, jobsPath+"/"+md.Name); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("kubejob: deleting orphaned job %s: %w", md.Name, err)
			}
			continue
		}
		// Per-Job Secrets and NetworkPolicies are owned by the Job and go
		// with it, including for a Job that finished while hive was down:
		// the sweep sends no Secret delete, and hive holds no delete verb on
		// Secrets. The policy delete is best effort, for a network-none Job
		// an older hive created with an unowned policy of the same name.
		l.deleteQuiet(ctx, fmt.Sprintf("/apis/networking.k8s.io/v1/namespaces/%s/networkpolicies/%s", ns, md.Name))
		swept = append(swept, SweptJob{Name: md.Name, Agent: agent, Age: age, Reason: reason})
	}
	return swept, firstErr
}

// isAgentSandboxJob reports whether an agent label names an agent-sandbox
// kick or publish-preflight Job, as opposed to a contained run.
func isAgentSandboxJob(agent string) bool {
	if !strings.HasPrefix(agent, sandboxJobAgentPrefix) {
		return false
	}
	return strings.HasSuffix(agent, sandboxJobKickSuffix) || strings.HasSuffix(agent, sandboxJobVerifySuffix)
}

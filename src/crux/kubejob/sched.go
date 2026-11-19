package kubejob

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/sandbox"
)

// Fail-fast on an unschedulable Job (#18811). A Job whose pod can never be
// placed (no node matches, a device is exhausted) used to hold the agent's slot
// until the kick timeout. wait now also reads the pod while it has no node, and
// once PodScheduled has been False/Unschedulable for the schedule timeout it
// gives up with sandbox.FailReasonWaitingForNode and the scheduler's message.

const (
	podScheduledCond = "PodScheduled"
	unschedulable    = "Unschedulable"
	// scheduleMessageMax bounds the scheduler's message quoted in the error.
	scheduleMessageMax = 500
)

// podState is what the schedule watch reads off the Job's pod.
type podState struct {
	name          string
	node          string
	unschedulable bool
	message       string
}

// scheduleWatch is the watch's state across polls.
type scheduleWatch struct {
	scheduled bool
	node      string
	since     time.Time
}

func (w *scheduleWatch) waiting() bool { return !w.since.IsZero() }

// waitJob polls the Job until a Complete or Failed condition, the schedule
// timeout, or ctx ending.
func (l *Launcher) waitJob(ctx context.Context, ns, name, jobPath string) (jobOutcome, error) {
	interval := l.Options.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var watch scheduleWatch
	for {
		raw, err := l.do(ctx, http.MethodGet, jobPath, nil)
		if err != nil {
			if ctx.Err() != nil {
				return jobOutcome{node: watch.node}, fmt.Errorf("kubejob: %w", ctx.Err())
			}
			// Transient API error: keep polling; the context still bounds us.
		} else if finished, ok := parseJobStatus(raw); ok {
			finished.node = watch.node
			return finished, nil
		} else if tripped, ok := l.watchSchedule(ctx, ns, name, &watch); ok {
			return tripped, nil
		}
		select {
		case <-ctx.Done():
			return jobOutcome{node: watch.node}, fmt.Errorf("kubejob: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// watchSchedule reads the pod while it has no node. It reports the outcome and
// true once the pod has been Unschedulable for the schedule timeout.
func (l *Launcher) watchSchedule(ctx context.Context, ns, job string, w *scheduleWatch) (jobOutcome, bool) {
	if w.scheduled {
		return jobOutcome{}, false
	}
	st, err := l.podState(ctx, ns, job)
	if err != nil || st.name == "" {
		return jobOutcome{}, false
	}
	if st.node != "" {
		w.scheduled, w.node = true, st.node
		if w.waiting() {
			w.since = time.Time{}
			l.emit(Event{Kind: EventScheduled, Node: st.node})
		}
		return jobOutcome{}, false
	}
	if !st.unschedulable {
		if w.waiting() {
			w.since = time.Time{}
			l.emit(Event{Kind: EventScheduled})
		}
		return jobOutcome{}, false
	}
	now := l.now()
	if !w.waiting() {
		w.since = now
		l.emit(Event{Kind: EventWaitingForNode, Detail: st.message})
	}
	if now.Sub(w.since) >= l.Options.scheduleTimeout() {
		return jobOutcome{failReason: sandbox.FailReasonWaitingForNode, reason: st.message}, true
	}
	return jobOutcome{}, false
}

// podState reads the Job's pod: where it landed, or why it cannot.
func (l *Launcher) podState(ctx context.Context, ns, job string) (podState, error) {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s", ns, fmt.Sprintf(labelSelectorFmt, jobLabelKey, job))
	raw, err := l.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return podState{}, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return podState{}, err
	}
	if len(list.Items) == 0 {
		return podState{}, nil
	}
	// backoffLimit is 0, so there is at most one pod; take the last.
	item := list.Items[len(list.Items)-1]
	st := podState{name: item.Metadata.Name, node: item.Spec.NodeName}
	for _, c := range item.Status.Conditions {
		if c.Type == podScheduledCond && c.Status == "False" && c.Reason == unschedulable {
			st.unschedulable = true
			st.message = strings.TrimSpace(c.Message)
			if r := []rune(st.message); len(r) > scheduleMessageMax {
				st.message = string(r[:scheduleMessageMax]) + "..."
			}
		}
	}
	return st, nil
}

// podDetails is the kick container's exit code and the node the pod ran on.
type podDetails struct {
	exitCode int
	node     string
}

// podDetails reads both off the pod, with exitCodeUnknown and "" when it cannot.
func (l *Launcher) podDetails(ctx context.Context, ns, pod string) podDetails {
	out := podDetails{exitCode: exitCodeUnknown}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, pod)
	raw, err := l.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return out
	}
	var p struct {
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State map[string]struct {
					ExitCode int `json:"exitCode"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return out
	}
	out.node = p.Spec.NodeName
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		if term, ok := cs.State[stateTerminated]; ok {
			out.exitCode = term.ExitCode
		}
	}
	return out
}

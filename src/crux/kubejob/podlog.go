package kubejob

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Read-only observation of a kick's pod, for the dashboard's terminal pane
// (hivecommons/hive#14798). A sandbox.runtime: job agent has no tmux session
// in the hive pod, so the only thing an operator can watch is the Job pod's
// log. These helpers reuse the launcher's in-cluster client, token and
// namespace discovery, and need nothing beyond the pods / pods/log get+list
// the launcher already requires (deploy/k8s/sandbox-job-rbac.yaml). They
// never create, delete or exec.

// Pod phases as the API reports them in status.phase.
const (
	PodPhasePending   = "Pending"
	PodPhaseRunning   = "Running"
	PodPhaseSucceeded = "Succeeded"
	PodPhaseFailed    = "Failed"
)

// ExitCodeUnknown is reported for a container that has not terminated yet
// or whose status could not be read.
const ExitCodeUnknown = exitCodeUnknown

// PodInfo is the subset of a kick pod's status the terminal pane shows.
type PodInfo struct {
	Name    string
	Job     string
	Phase   string
	Created time.Time
	// Terminated is true once the kick container has exited; ExitCode is
	// then its exit code, else ExitCodeUnknown.
	Terminated bool
	ExitCode   int
	// Waiting carries the kick container's waiting reason (ImagePullBackOff,
	// ContainerCreating, …) while it has not started, else "".
	Waiting string
}

// Finished reports whether the pod can produce no more log output.
func (p PodInfo) Finished() bool {
	return p.Terminated || p.Phase == PodPhaseSucceeded || p.Phase == PodPhaseFailed
}

// AgentLabelValue returns the hive.kubestellar.io/agent label the launcher
// stamps on a Job and its pod for the given LaunchSpec.Name. The sandbox
// executor names its launches "hive-<agent>-sandbox"; callers that only know
// the agent name build the launch name the same way and pass it here.
func AgentLabelValue(launchName string) string {
	return sanitizeLabel(launchName)
}

// AgentLabelSelector is the selector that finds every pod this launcher
// created for a launch name — the managed-by label keeps out any other
// workload an operator may have labelled with the same agent key.
func AgentLabelSelector(launchName string) string {
	return fmt.Sprintf("%s=%s,%s=%s", managedByLabel, managedByValue, agentLabel, AgentLabelValue(launchName))
}

// LatestPod returns the most recently created pod the launcher made for
// launchName, or ok=false when there is none in the namespace. A successful
// Job is deleted by Run as soon as it completes, so "none" is the normal
// state between kicks; a failed Job's pod stays until its TTL.
func (l *Launcher) LatestPod(ctx context.Context, launchName string) (PodInfo, bool, error) {
	ns, err := l.namespace()
	if err != nil {
		return PodInfo{}, false, err
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods?labelSelector=%s", ns, url.QueryEscape(AgentLabelSelector(launchName)))
	raw, err := l.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return PodInfo{}, false, err
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return PodInfo{}, false, fmt.Errorf("kubejob: decoding pod list: %w", err)
	}
	var latest PodInfo
	found := false
	for _, item := range list.Items {
		info, err := parsePod(item)
		if err != nil {
			continue
		}
		if !found || info.Created.After(latest.Created) {
			latest, found = info, true
		}
	}
	return latest, found, nil
}

// PodStatus re-reads one pod, typically after a followed log stream has
// ended, to learn the kick container's exit code.
func (l *Launcher) PodStatus(ctx context.Context, pod string) (PodInfo, error) {
	ns, err := l.namespace()
	if err != nil {
		return PodInfo{}, err
	}
	raw, err := l.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, url.PathEscape(pod)), nil)
	if err != nil {
		return PodInfo{}, err
	}
	info, err := parsePod(raw)
	if err != nil {
		return PodInfo{}, fmt.Errorf("kubejob: decoding pod: %w", err)
	}
	return info, nil
}

// parsePod reads the fields PodInfo needs off a core/v1 Pod object.
func parsePod(raw []byte) (PodInfo, error) {
	var p struct {
		Metadata struct {
			Name              string            `json:"name"`
			Labels            map[string]string `json:"labels"`
			CreationTimestamp string            `json:"creationTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State map[string]struct {
					ExitCode int    `json:"exitCode"`
					Reason   string `json:"reason"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return PodInfo{}, err
	}
	if p.Metadata.Name == "" {
		return PodInfo{}, errors.New("pod has no name")
	}
	info := PodInfo{
		Name:     p.Metadata.Name,
		Job:      p.Metadata.Labels[jobLabelKey],
		Phase:    p.Status.Phase,
		ExitCode: ExitCodeUnknown,
	}
	if ts, err := time.Parse(time.RFC3339, p.Metadata.CreationTimestamp); err == nil {
		info.Created = ts
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		if term, ok := cs.State[stateTerminated]; ok {
			info.Terminated = true
			info.ExitCode = term.ExitCode
		}
		if wait, ok := cs.State["waiting"]; ok {
			info.Waiting = wait.Reason
		}
	}
	return info, nil
}

// StreamLog copies the kick container's log to w as the API server sends it.
// With follow the connection stays open until the container exits or ctx
// ends; without it the log read so far is returned and the call ends. Output
// is forwarded a line at a time so the caller can filter it (the dashboard
// scrubs credentials before anything reaches a browser). Bounded by ctx, not
// by the launcher's per-request timeout: a followed stream is expected to
// outlive any single API call.
func (l *Launcher) StreamLog(ctx context.Context, pod string, follow bool, w io.Writer) error {
	ns, err := l.namespace()
	if err != nil {
		return err
	}
	client, err := l.client()
	if err != nil {
		return err
	}
	server := l.APIServer
	if server == "" {
		server = DefaultAPIServer
	}
	q := url.Values{}
	q.Set("container", containerName)
	q.Set("limitBytes", fmt.Sprint(maxLogBytes))
	if follow {
		q.Set("follow", "true")
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?%s", ns, url.PathEscape(pod), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+path, nil)
	if err != nil {
		return err
	}
	if tok := l.token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, apiMessage(body))
	}
	rd := bufio.NewReader(resp.Body)
	for {
		line, err := rd.ReadString('\n')
		if line != "" {
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// FlushWriter is implemented by writers that can push buffered output to
// their consumer; StreamLog's callers use it to deliver each line as it
// arrives rather than when the response ends.
type FlushWriter interface {
	io.Writer
	Flush()
}

// LineFilterWriter applies fn to each complete line written through it and
// flushes the destination after every line, so a followed log reaches a
// browser as it is produced. Partial trailing lines are buffered until their
// newline arrives (or Close is called).
type LineFilterWriter struct {
	Dst io.Writer
	Fn  func(string) string
	buf strings.Builder
}

func (w *LineFilterWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		s := w.buf.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := s[:i+1]
		w.buf.Reset()
		w.buf.WriteString(s[i+1:])
		if err := w.emit(line); err != nil {
			return 0, err
		}
	}
}

// Close writes any buffered partial line.
func (w *LineFilterWriter) Close() error {
	if w.buf.Len() == 0 {
		return nil
	}
	line := w.buf.String()
	w.buf.Reset()
	return w.emit(line)
}

func (w *LineFilterWriter) emit(line string) error {
	if w.Fn != nil {
		line = w.Fn(line)
	}
	if _, err := io.WriteString(w.Dst, line); err != nil {
		return err
	}
	if f, ok := w.Dst.(FlushWriter); ok {
		f.Flush()
	}
	return nil
}

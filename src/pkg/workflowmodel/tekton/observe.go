package tekton

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Post-merge PipelineRun observer (hivecommons/hive#18356, #18646).
//
// The observer lists the PipelineRuns a pipeline's TektonObserve config
// names, reads the revision each one ran against and its conclusion, and
// hands them, oldest first, to workflowmodel.AttributeRuns. The revision
// comes from the configured annotation (default
// pipelinesascode.tekton.dev/sha), then the configured spec.params entry
// (default "revision"). A run whose revision cannot be read as a full
// commit SHA keeps an empty Revision, so attribution leaves the PRs it
// might cover unknown, and the run carries the reason.

// ObservedRun is one PipelineRun as the observer saw it.
type ObservedRun struct {
	workflowmodel.PostMergeRun
	// Reason says why Revision is empty; empty when it was determined.
	Reason string
}

// Lister returns the PipelineRuns cfg selects, as decoded Kubernetes
// objects.
type Lister interface {
	ListPipelineRuns(ctx context.Context, cfg workflowmodel.TektonObserve) ([]map[string]any, error)
}

// Observe lists the PipelineRuns cfg selects and returns them oldest
// first with their revision and conclusion resolved.
func Observe(ctx context.Context, l Lister, cfg workflowmodel.TektonObserve) ([]ObservedRun, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	items, err := l.ListPipelineRuns(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("tekton observe %s/%s: %w", cfg.Namespace, cfg.LabelSelector, err)
	}
	return ObserveRuns(items, cfg), nil
}

// ObserveRuns resolves already-listed PipelineRuns. Documents that are not
// PipelineRuns are skipped. Runs are ordered by start time (creation time
// when not started), then name.
func ObserveRuns(items []map[string]any, cfg workflowmodel.TektonObserve) []ObservedRun {
	type keyed struct {
		run ObservedRun
		at  time.Time
	}
	var ks []keyed
	for _, it := range items {
		// List items of a CRD carry kind and apiVersion; skip anything
		// that says it is not a Tekton PipelineRun.
		if k := stringAt(it, "kind"); k != "" && k != "PipelineRun" {
			continue
		}
		if stringAt(it, "apiVersion") != "" && !IsTektonDocument(it) {
			continue
		}
		rev, reason := RunRevision(it, cfg)
		ks = append(ks, keyed{
			run: ObservedRun{
				PostMergeRun: workflowmodel.PostMergeRun{ID: metadataName(it), Revision: rev, Conclusion: RunConclusion(it)},
				Reason:       reason,
			},
			at: runTime(it),
		})
	}
	sort.SliceStable(ks, func(i, j int) bool {
		if !ks[i].at.Equal(ks[j].at) {
			return ks[i].at.Before(ks[j].at)
		}
		return ks[i].run.ID < ks[j].run.ID
	})
	out := make([]ObservedRun, len(ks))
	for i, k := range ks {
		out[i] = k.run
	}
	return out
}

// Attribute feeds observed runs for one column through the range
// attribution core.
func Attribute(column string, runs []ObservedRun, history []string, enabledAt string, prs []workflowmodel.MergedPR) map[int]map[string]workflowmodel.Attribution {
	pm := make([]workflowmodel.PostMergeRun, len(runs))
	for i, r := range runs {
		pm[i] = r.PostMergeRun
	}
	return workflowmodel.AttributeRuns(workflowmodel.AttributionInput{
		History:   history,
		EnabledAt: enabledAt,
		Columns:   map[string][]workflowmodel.PostMergeRun{column: pm},
		PRs:       prs,
	})
}

// commitSHA matches a full SHA-1 or SHA-256 object name. Anything else (a
// branch name, a short SHA, an unresolved $(params.x) reference) is not a
// revision attribution can trust.
var commitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// RunRevision returns the commit a PipelineRun ran against, or "" and why
// it could not be determined.
func RunRevision(pr map[string]any, cfg workflowmodel.TektonObserve) (string, string) {
	ann, param := cfg.Annotation(), cfg.Param()
	var tried []string
	if v, ok := stringMap(mapAt(mapAt(pr, "metadata"), "annotations"))[ann]; ok {
		v = strings.ToLower(strings.TrimSpace(v))
		if commitSHA.MatchString(v) {
			return v, ""
		}
		tried = append(tried, fmt.Sprintf("annotation %s=%q is not a full commit SHA", ann, v))
	} else {
		tried = append(tried, fmt.Sprintf("no annotation %s", ann))
	}
	found := false
	for _, p := range mapsAt(mapAt(pr, "spec"), "params") {
		if stringAt(p, "name") != param {
			continue
		}
		found = true
		s, ok := p["value"].(string)
		if !ok {
			tried = append(tried, fmt.Sprintf("param %s is not a string", param))
			break
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if commitSHA.MatchString(s) {
			return s, ""
		}
		tried = append(tried, fmt.Sprintf("param %s=%q is not a full commit SHA", param, s))
		break
	}
	if !found {
		tried = append(tried, fmt.Sprintf("no param %s", param))
	}
	return "", "revision undeterminable: " + strings.Join(tried, "; ")
}

// cancelReasons are Succeeded=False reasons that mean the run was stopped,
// not that it failed: such a run neither credits nor fails a PR.
var cancelReasons = map[string]bool{
	"Cancelled":            true,
	"PipelineRunCancelled": true,
	"CancelledRunFinally":  true,
	"StoppedRunFinally":    true,
	"PipelineRunStopping":  true,
}

// RunConclusion maps a PipelineRun's Succeeded condition to a conclusion:
// True is success, False is failure unless the reason is a cancellation,
// anything else (running, pending, cancelled) is neither.
func RunConclusion(pr map[string]any) workflowmodel.RunConclusion {
	for _, c := range mapsAt(mapAt(pr, "status"), "conditions") {
		if stringAt(c, "type") != "Succeeded" {
			continue
		}
		switch stringAt(c, "status") {
		case "True":
			return workflowmodel.RunSuccess
		case "False":
			if cancelReasons[stringAt(c, "reason")] {
				return "cancelled"
			}
			return workflowmodel.RunFailure
		}
		return "running"
	}
	return "queued"
}

func runTime(pr map[string]any) time.Time {
	for _, s := range []string{stringAt(mapAt(pr, "status"), "startTime"), stringAt(mapAt(pr, "metadata"), "creationTimestamp")} {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// NewLister returns the Lister cfg asks for: the in-cluster API with the
// pod's ServiceAccount, or kubectl against the named context.
func NewLister(cfg workflowmodel.TektonObserve) Lister {
	if cfg.InCluster {
		return &InClusterLister{}
	}
	return &KubectlLister{}
}

// KubectlLister lists PipelineRuns with kubectl against cfg.Context.
type KubectlLister struct {
	// Kubectl is the binary; empty means "kubectl" on PATH.
	Kubectl string
	// run executes the command; tests replace it.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ListPipelineRuns implements Lister.
func (k *KubectlLister) ListPipelineRuns(ctx context.Context, cfg workflowmodel.TektonObserve) ([]map[string]any, error) {
	bin := k.Kubectl
	if bin == "" {
		bin = "kubectl"
	}
	run := k.run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			var stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
			}
			return out, nil
		}
	}
	out, err := run(ctx, bin, "--context", cfg.Context, "get", "pipelineruns.tekton.dev",
		"--namespace", cfg.Namespace, "--selector", cfg.LabelSelector, "--output", "json")
	if err != nil {
		return nil, err
	}
	return decodeList(out)
}

// In-cluster ServiceAccount defaults.
const (
	inClusterAPIServer = "https://kubernetes.default.svc"
	inClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// InClusterLister lists PipelineRuns through the in-cluster API with the
// pod's ServiceAccount. Zero fields take the in-cluster defaults.
type InClusterLister struct {
	APIServer string
	TokenPath string
	CAPath    string
	Client    *http.Client
}

// ListPipelineRuns implements Lister.
func (l *InClusterLister) ListPipelineRuns(ctx context.Context, cfg workflowmodel.TektonObserve) ([]map[string]any, error) {
	server, tokenPath := l.APIServer, l.TokenPath
	if server == "" {
		server = inClusterAPIServer
	}
	if tokenPath == "" {
		tokenPath = inClusterTokenPath
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read serviceaccount token: %w", err)
	}
	client := l.Client
	if client == nil {
		client, err = inClusterClient(l.CAPath)
		if err != nil {
			return nil, err
		}
	}
	u := strings.TrimRight(server, "/") + "/apis/tekton.dev/v1/namespaces/" + url.PathEscape(cfg.Namespace) +
		"/pipelineruns?labelSelector=" + url.QueryEscape(cfg.LabelSelector)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list pipelineruns: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return decodeList(body)
}

func inClusterClient(caPath string) (*http.Client, error) {
	if caPath == "" {
		caPath = inClusterCAPath
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read serviceaccount CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("serviceaccount CA has no certificates")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

func decodeList(b []byte) ([]map[string]any, error) {
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("decode pipelinerun list: %w", err)
	}
	return list.Items, nil
}

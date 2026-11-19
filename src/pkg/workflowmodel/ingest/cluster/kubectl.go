package cluster

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// KubectlTimeout bounds one kubectl invocation.
const KubectlTimeout = 60 * time.Second

// maxContexts caps the context list returned to the UI.
const maxContexts = 100

// Seams for tests. Production always uses the real exec, PATH and files.
var (
	execCommandContext = exec.CommandContext
	lookPath           = exec.LookPath
	statFile           = os.Stat
	getenv             = os.Getenv
	userHomeDir        = os.UserHomeDir
)

// inClusterTokenPath is the ServiceAccount token a pod gets.
const inClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Kubectl runs the kubectl binary on PATH. Kubeconfig, when set, is passed
// as --kubeconfig.
type Kubectl struct {
	Kubeconfig string
}

// Run executes kubectl with args and returns stdout; stderr is folded into
// the error so Fetch can classify it.
func (k Kubectl) Run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, KubectlTimeout)
	defer cancel()
	full := args
	if k.Kubeconfig != "" {
		full = append([]string{"--kubeconfig", k.Kubeconfig}, args...)
	}
	cmd := execCommandContext(ctx, "kubectl", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Available reports whether this process can reach a cluster at all:
// kubectl is on PATH and either a kubeconfig (KUBECONFIG or
// ~/.kube/config) or an in-cluster ServiceAccount token exists. The reason
// explains a false answer in one sentence for the UI.
func Available() (bool, string) {
	if _, err := lookPath("kubectl"); err != nil {
		return false, "kubectl is not installed on the hive"
	}
	if kc := getenv("KUBECONFIG"); kc != "" {
		for _, p := range filepath.SplitList(kc) {
			if _, err := statFile(p); err == nil {
				return true, ""
			}
		}
	}
	if home, err := userHomeDir(); err == nil {
		if _, err := statFile(filepath.Join(home, ".kube", "config")); err == nil {
			return true, ""
		}
	}
	if _, err := statFile(inClusterTokenPath); err == nil {
		return true, ""
	}
	return false, "no kubeconfig (KUBECONFIG or ~/.kube/config) and no in-cluster ServiceAccount token"
}

// Contexts lists the kubeconfig context names, capped at maxContexts. An
// empty list with no error means kubectl has a config with no contexts
// (in-cluster, typically).
func Contexts(ctx context.Context, runner Runner) ([]string, error) {
	if runner == nil {
		return nil, ErrNoRunner
	}
	raw, err := runner.Run(ctx, "config", "get-contexts", "-o", "name")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
		if len(out) >= maxContexts {
			break
		}
	}
	return out, nil
}

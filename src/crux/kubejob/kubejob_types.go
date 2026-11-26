package kubejob

import "context"

// Toleration mirrors the subset of corev1.Toleration an operator writes in YAML.
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

// Resources mirrors corev1.ResourceRequirements with string quantities, so a
// device limit like `<vendor>.com/<device>: "2"` passes through untouched.
type Resources struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

// VolumeMount is an additional PVC to mount into the Job.
type VolumeMount struct {
	Name      string `json:"name"`
	Claim     string `json:"claim"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// Network modes, Options.Network.
const (
	// NetworkDefault leaves the pod on the cluster network with whatever
	// policies the namespace already has. The empty string means this.
	NetworkDefault = "default"
	// NetworkNone creates a deny-all NetworkPolicy selecting the Job's pods
	// before the Job and removes it with the Job; the run is refused when
	// the policy cannot be created. Enforcement is the CNI's: a cluster
	// whose network plugin ignores NetworkPolicy isolates nothing, and the
	// launcher cannot tell.
	NetworkNone = "none"
	// NetworkHost runs the pod with hostNetwork.
	NetworkHost = "host"
)

// deleteQuiet deletes the object at path (the claim transport's per-Job
// NetworkPolicy), ignoring errors; an empty path means none was created.
func (l *Launcher) deleteQuiet(ctx context.Context, path string) {
	if path != "" {
		_ = l.delete(ctx, path)
	}
}

// denyAllPolicy is a networking.k8s.io/v1 NetworkPolicy named after the
// Job that selects its pods and allows no ingress and no egress.
func denyAllPolicy(ns, job string) map[string]any {
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      job,
			"namespace": ns,
			"labels":    map[string]string{managedByLabel: managedByValue, jobLabel: job},
		},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{jobLabel: job}},
			"policyTypes": []string{"Ingress", "Egress"},
		},
	}
}

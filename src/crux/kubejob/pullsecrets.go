package kubejob

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Pull secrets on the Job pod (#18819).
//
// Kubernetes' ServiceAccount admission adds the service account's
// imagePullSecrets only to a pod that has none of its own, so the launcher
// setting the hive pod's list (for the relay init image) hid the service
// account's. The pod now carries the union, in a stable order: hive's own,
// then the service account's, then job.image_pull_secrets. Only Secret NAMES
// are ever read, logged or written.

const (
	// maxPullSecrets bounds the merged list.
	maxPullSecrets = 64
	// maxConfiguredPullSecrets bounds job.image_pull_secrets.
	maxConfiguredPullSecrets = 32
	maxPullSecretNameLen     = 253
)

var pullSecretNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// validatePullSecretNames refuses anything but a bounded list of names.
func validatePullSecretNames(names []string) error {
	if len(names) > maxConfiguredPullSecrets {
		return fmt.Errorf("kubejob: image_pull_secrets takes at most %d names, got %d", maxConfiguredPullSecrets, len(names))
	}
	for _, n := range names {
		if len(n) > maxPullSecretNameLen || !pullSecretNameRE.MatchString(n) {
			return fmt.Errorf("kubejob: image_pull_secrets takes Secret names, not values: %q is not a valid name", n)
		}
	}
	return nil
}

// mergePullSecretNames concatenates the lists, dropping blanks and repeats and
// keeping first-seen order.
func mergePullSecretNames(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, n := range list {
			n = strings.TrimSpace(n)
			if n == "" || seen[n] || len(out) >= maxPullSecrets {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// serviceAccountPullSecrets reads the names of the Job service account's
// imagePullSecrets. A launcher whose RBAC lacks `get serviceaccounts` (or any
// other failure) yields a warning and no names, never an error.
func (l *Launcher) serviceAccountPullSecrets(ctx context.Context, ns string) []string {
	sa := strings.TrimSpace(l.Options.ServiceAccount)
	if sa == "" {
		return nil
	}
	raw, err := l.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/namespaces/%s/serviceaccounts/%s", ns, url.PathEscape(sa)), nil)
	if err != nil {
		slog.Warn("kubejob: cannot read the Job service account's imagePullSecrets; set sandbox.job.image_pull_secrets if the sandbox image is in a private registry",
			"serviceAccount", sa, "error", err.Error())
		return nil
	}
	var body struct {
		ImagePullSecrets []struct {
			Name string `json:"name"`
		} `json:"imagePullSecrets"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		slog.Warn("kubejob: the Job service account did not decode; its imagePullSecrets are skipped", "serviceAccount", sa)
		return nil
	}
	names := make([]string, 0, len(body.ImagePullSecrets))
	for _, s := range body.ImagePullSecrets {
		names = append(names, s.Name)
	}
	return names
}

// resolvePullSecrets is the union for one Job pod.
func (l *Launcher) resolvePullSecrets(ctx context.Context, ns string, relays *relayPlan) []string {
	var hive []string
	if relays != nil {
		for _, s := range relays.pullSecrets {
			hive = append(hive, s["name"])
		}
	}
	return mergePullSecretNames(hive, l.serviceAccountPullSecrets(ctx, ns), l.Options.ImagePullSecrets)
}

// setPullSecrets writes names into the manifest's pod spec; no names leaves it
// untouched.
func setPullSecrets(manifest map[string]any, names []string) {
	if len(names) == 0 {
		return
	}
	spec, _ := manifest["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	pod, _ := tmpl["spec"].(map[string]any)
	if pod == nil {
		return
	}
	refs := make([]map[string]string, 0, len(names))
	for _, n := range names {
		refs = append(refs, map[string]string{"name": n})
	}
	pod["imagePullSecrets"] = refs
}

package config

import (
	"fmt"
	"regexp"
)

// MaxJobImagePullSecrets bounds image_pull_secrets.
const MaxJobImagePullSecrets = 32

// maxPullSecretNameLen is the Kubernetes object-name length limit.
const maxPullSecretNameLen = 253

var pullSecretNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// ValidateImagePullSecrets refuses anything but a bounded list of Kubernetes
// Secret names, so a pasted credential never reaches the config file.
func ValidateImagePullSecrets(names []string) error {
	if len(names) > MaxJobImagePullSecrets {
		return fmt.Errorf("sandbox.job.image_pull_secrets takes at most %d names, got %d", MaxJobImagePullSecrets, len(names))
	}
	for _, n := range names {
		if len(n) > maxPullSecretNameLen || !pullSecretNameRE.MatchString(n) {
			return fmt.Errorf("sandbox.job.image_pull_secrets takes Secret names, not values: %q is not a valid name", n)
		}
	}
	return nil
}

// inheritPullSecrets keeps a per-agent list and falls back to the global one:
// registry credentials are a cluster fact like the workspace claim, so a
// per-agent block that names none inherits them. The pod gets the union of
// hive's own pod secrets, the Job service account's and this list (#18819).
func inheritPullSecrets(out, global SandboxJobConfig) SandboxJobConfig {
	if len(out.ImagePullSecrets) == 0 {
		out.ImagePullSecrets = global.ImagePullSecrets
	}
	return out
}

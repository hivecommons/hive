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

package config

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	maxSandboxPodMetadataEntries = 128
	maxSandboxPodAnnotationBytes = 256 * 1024
)

// These expressions implement Kubernetes qualified-name, DNS-1123 subdomain,
// and label-value validation without adding a Kubernetes dependency to crux.
var (
	sandboxMetadataName   = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	sandboxMetadataPrefix = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

func validSandboxMetadataKey(key string, annotation bool) bool {
	if annotation {
		key = strings.ToLower(key)
	}
	parts := strings.Split(key, "/")
	if len(parts) > 2 {
		return false
	}
	name := parts[len(parts)-1]
	if len(name) == 0 || len(name) > 63 || !sandboxMetadataName.MatchString(name) {
		return false
	}
	return len(parts) == 1 || (len(parts[0]) <= 253 && sandboxMetadataPrefix.MatchString(parts[0]))
}

// ValidateSandboxJobMetadata validates operator-provided pod labels and
// annotations. Call it on the effective job as well as each raw job block,
// since individually valid maps can exceed limits after inheritance.
func ValidateSandboxJobMetadata(job *SandboxJobConfig) error {
	if job == nil {
		return nil
	}
	for _, field := range []struct {
		name       string
		values     map[string]string
		annotation bool
	}{
		{"pod_labels", job.PodLabels, false},
		{"pod_annotations", job.PodAnnotations, true},
	} {
		if len(field.values) > maxSandboxPodMetadataEntries {
			return fmt.Errorf("job.%s exceeds the limit of %d entries", field.name, maxSandboxPodMetadataEntries)
		}
		size := 0
		for key, value := range field.values {
			lower := strings.ToLower(key)
			if lower == "app.kubernetes.io/managed-by" || lower == "job-name" || strings.HasPrefix(lower, "hive.kubestellar.io/") {
				return fmt.Errorf("job.%s key %q is reserved", field.name, key)
			}
			if !validSandboxMetadataKey(key, field.annotation) {
				return fmt.Errorf("job.%s key %q is not a valid Kubernetes metadata key", field.name, key)
			}
			if !field.annotation && (len(value) > 63 || (value != "" && !sandboxMetadataName.MatchString(value))) {
				return fmt.Errorf("job.%s value for %q is not a valid Kubernetes label value", field.name, key)
			}
			size += len(key) + len(value)
		}
		if field.annotation && size > maxSandboxPodAnnotationBytes {
			return fmt.Errorf("job.%s exceeds %d bytes", field.name, maxSandboxPodAnnotationBytes)
		}
	}
	return nil
}

func mergeSandboxPodMetadata(global, agent map[string]string) map[string]string {
	if len(global) == 0 && len(agent) == 0 {
		return nil
	}
	merged := make(map[string]string, len(global)+len(agent))
	for key, value := range global {
		merged[key] = value
	}
	for key, value := range agent {
		merged[key] = value
	}
	return merged
}

package config

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/intent"
)

// PinOnlyException is policy, not authorization to merge. The automerge sweep
// lanes and /approve apply it (#16266) through Engine.SetHumanMergePinOnly /
// Options.HumanMergePinOnly: before consulting the purely syntactic
// classifier they prove the PR opener is a trusted author and that the
// fetched changed-file list is complete, and the merge itself is bound to
// the evaluated head SHA. They do not prove review of the referenced
// upstream changes; review stays with the lanes' independent review gates.
// The hive-merge relay does not apply the exception yet (#16265).
type PinOnlyException struct {
	Enabled        bool     `yaml:"enabled" json:"enabled"`
	Paths          []string `yaml:"paths" json:"paths"`
	Kinds          []string `yaml:"kinds" json:"kinds"`
	TrustedAuthors []string `yaml:"trusted_authors" json:"trusted_authors"`
}

func (p PinOnlyException) validate() error {
	for _, kind := range p.Kinds {
		switch kind {
		case "action-sha", "image-digest", "version-pin":
		default:
			return fmt.Errorf("unknown kind %q", kind)
		}
	}
	if !p.Enabled {
		return nil
	}
	for name, values := range map[string][]string{"paths": p.Paths, "kinds": p.Kinds, "trusted_authors": p.TrustedAuthors} {
		if len(values) == 0 {
			return fmt.Errorf("enabled policy requires nonempty %s", name)
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s contains an empty value", name)
			}
		}
	}
	return nil
}

// Spec returns classifier input only; it deliberately contains no merge grant.
func (p PinOnlyException) Spec() intent.PinOnlySpec {
	return intent.PinOnlySpec{Paths: append([]string(nil), p.Paths...), Kinds: append([]string(nil), p.Kinds...)}
}

// ValidateHumanMergePinOnly rejects ambiguous repository keys and invalid policy.
// Paths are checked against actual human-merge matches by the classifier, rather
// than attempting to infer containment between arbitrary glob languages.
func (a AutoMergeConfig) ValidateHumanMergePinOnly() error {
	seen := make(map[string]bool)
	for repo, policy := range a.HumanMergePinOnly {
		key := strings.ToLower(strings.TrimSpace(repo))
		parts := strings.Split(key, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(key, " \t\n\r*") || seen[key] {
			return fmt.Errorf("auto_merge.human_merge_pin_only: invalid or duplicate repository %q", repo)
		}
		seen[key] = true
		if err := policy.validate(); err != nil {
			return fmt.Errorf("auto_merge.human_merge_pin_only[%s]: %w", repo, err)
		}
	}
	return nil
}

// HumanMergePinOnlyFor returns a defensive copy of an enabled, valid policy.
// Ambiguous case-insensitive keys fail closed even before config validation.
func (a AutoMergeConfig) HumanMergePinOnlyFor(repo string) (PinOnlyException, bool) {
	var result PinOnlyException
	found := false
	for key, policy := range a.HumanMergePinOnly {
		if !strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(repo)) {
			continue
		}
		if found {
			return PinOnlyException{}, false
		}
		found = true
		result = policy
	}
	if !found || !result.Enabled || result.validate() != nil {
		return PinOnlyException{}, false
	}
	result.Paths = append([]string(nil), result.Paths...)
	result.Kinds = append([]string(nil), result.Kinds...)
	result.TrustedAuthors = append([]string(nil), result.TrustedAuthors...)
	return result, true
}

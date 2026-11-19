package config

import (
	"fmt"
	"strconv"
	"strings"
)

// SandboxJobAffinity is the supported node-affinity subset of Kubernetes affinity.
type SandboxJobAffinity struct {
	NodeAffinity *SandboxJobNodeAffinity `yaml:"nodeAffinity,omitempty" json:"nodeAffinity,omitempty"`
}

type SandboxJobNodeAffinity struct {
	RequiredDuringSchedulingIgnoredDuringExecution  *SandboxJobNodeSelector             `yaml:"requiredDuringSchedulingIgnoredDuringExecution,omitempty" json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
	PreferredDuringSchedulingIgnoredDuringExecution []SandboxJobPreferredSchedulingTerm `yaml:"preferredDuringSchedulingIgnoredDuringExecution,omitempty" json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

type SandboxJobNodeSelector struct {
	NodeSelectorTerms []SandboxJobNodeSelectorTerm `yaml:"nodeSelectorTerms" json:"nodeSelectorTerms"`
}

type SandboxJobNodeSelectorTerm struct {
	MatchExpressions []SandboxJobNodeSelectorRequirement `yaml:"matchExpressions,omitempty" json:"matchExpressions,omitempty"`
}

type SandboxJobNodeSelectorRequirement struct {
	Key      string   `yaml:"key" json:"key"`
	Operator string   `yaml:"operator" json:"operator"`
	Values   []string `yaml:"values,omitempty" json:"values,omitempty"`
}

type SandboxJobPreferredSchedulingTerm struct {
	Weight     int32                      `yaml:"weight" json:"weight"`
	Preference SandboxJobNodeSelectorTerm `yaml:"preference" json:"preference"`
}

// MaxJobAffinityTerms and MaxJobAffinityValues bound operator-supplied lists.
const (
	MaxJobAffinityTerms  = 32
	MaxJobAffinityValues = 32
)

// ValidateSandboxJobAffinity rejects invalid or unbounded node affinity before
// it reaches the Kubernetes API. A nil affinity leaves placement unchanged.
func ValidateSandboxJobAffinity(affinity *SandboxJobAffinity) error {
	if affinity == nil {
		return nil
	}
	if affinity.NodeAffinity == nil {
		return fmt.Errorf("affinity.nodeAffinity is required")
	}
	node := affinity.NodeAffinity
	if node.RequiredDuringSchedulingIgnoredDuringExecution == nil && len(node.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		return fmt.Errorf("affinity.nodeAffinity requires requiredDuringSchedulingIgnoredDuringExecution or preferredDuringSchedulingIgnoredDuringExecution")
	}
	if required := node.RequiredDuringSchedulingIgnoredDuringExecution; required != nil {
		path := "affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms"
		if len(required.NodeSelectorTerms) == 0 || len(required.NodeSelectorTerms) > MaxJobAffinityTerms {
			return fmt.Errorf("%s must contain 1 to %d terms", path, MaxJobAffinityTerms)
		}
		for i, term := range required.NodeSelectorTerms {
			if err := validateAffinityTerm(term, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	preferred := node.PreferredDuringSchedulingIgnoredDuringExecution
	path := "affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution"
	if len(preferred) > MaxJobAffinityTerms {
		return fmt.Errorf("%s must contain at most %d terms", path, MaxJobAffinityTerms)
	}
	for i, term := range preferred {
		termPath := fmt.Sprintf("%s[%d]", path, i)
		if term.Weight < 1 || term.Weight > 100 {
			return fmt.Errorf("%s.weight must be between 1 and 100", termPath)
		}
		if err := validateAffinityTerm(term.Preference, termPath+".preference"); err != nil {
			return err
		}
	}
	return nil
}

func validateAffinityTerm(term SandboxJobNodeSelectorTerm, path string) error {
	path += ".matchExpressions"
	if len(term.MatchExpressions) == 0 || len(term.MatchExpressions) > MaxJobAffinityTerms {
		return fmt.Errorf("%s must contain 1 to %d expressions", path, MaxJobAffinityTerms)
	}
	for i, expr := range term.MatchExpressions {
		p := fmt.Sprintf("%s[%d]", path, i)
		if strings.TrimSpace(expr.Key) == "" {
			return fmt.Errorf("%s.key must not be empty", p)
		}
		if len(expr.Values) > MaxJobAffinityValues {
			return fmt.Errorf("%s.values must contain at most %d values", p, MaxJobAffinityValues)
		}
		switch expr.Operator {
		case "In", "NotIn":
			if len(expr.Values) == 0 {
				return fmt.Errorf("%s.values must not be empty for %s", p, expr.Operator)
			}
		case "Exists", "DoesNotExist":
			if expr.Values != nil {
				return fmt.Errorf("%s.values must be empty for %s", p, expr.Operator)
			}
		case "Gt", "Lt":
			if len(expr.Values) != 1 {
				return fmt.Errorf("%s.values must contain one integer for %s", p, expr.Operator)
			}
			if _, err := strconv.ParseInt(expr.Values[0], 10, 64); err != nil {
				return fmt.Errorf("%s.values must contain one integer for %s", p, expr.Operator)
			}
		default:
			return fmt.Errorf("%s.operator %q is invalid", p, expr.Operator)
		}
	}
	return nil
}

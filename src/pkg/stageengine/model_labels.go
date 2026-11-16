package stageengine

import (
	"strings"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// modelDesired supplies only explicitly state-bound labels. Labels without a
// state are repository vocabulary, not labels the state writer may remove.
func modelDesired(want *stagelabels.Desired, p *workflowmodel.Profile, state string) {
	if p == nil || state == "" || p.States[state] == nil {
		return
	}
	want.ModelPreserve = p.InputLabels
	for _, entry := range p.Labels.Entries {
		if entry.State == "" || strings.TrimSpace(entry.Name) == "" {
			continue
		}
		if entry.State == state {
			guarded := false
			for _, pattern := range p.GuardedLabels {
				name := strings.ToLower(strings.TrimSpace(entry.Name))
				pattern = strings.ToLower(strings.TrimSpace(pattern))
				if name == pattern || (strings.HasSuffix(pattern, "*") && strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))) {
					guarded = true
					break
				}
			}
			if !guarded {
				want.ModelLabels = append(want.ModelLabels, entry.Name)
			}
		} else {
			want.ModelManaged = append(want.ModelManaged, entry.Name)
		}
	}
}

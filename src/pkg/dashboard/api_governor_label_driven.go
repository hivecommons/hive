package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// labelDrivenRepoBody is one repo's entry in PUT /api/config/governor/repos
// repoLabelDriven (hivecommons/hive#11313). A nil entry clears the opt-in.
type labelDrivenRepoBody struct {
	Enabled        bool      `json:"enabled"`
	ApprovalLabels *[]string `json:"approvalLabels,omitempty"`
}

// labelDrivenApprovalBody carries the project-wide label-driven trust knobs.
// Every field is optional; nil means "leave as is". An empty approvalLabels
// or trustedBots list restores the built-in default rather than disabling
// the feature, since an empty list cannot round-trip through yaml omitempty.
type labelDrivenApprovalBody struct {
	ApprovalLabels  *[]string `json:"approvalLabels,omitempty"`
	TrustOrgMembers *bool     `json:"trustOrgMembers,omitempty"`
	TrustedBots     *[]string `json:"trustedBots,omitempty"`
}

func (b *labelDrivenApprovalBody) applyTo(cfg *config.Config) {
	if b == nil || cfg == nil {
		return
	}
	if b.ApprovalLabels != nil {
		cfg.Project.LabelDrivenApprovalLabels = labelDrivenListOrDefault(*b.ApprovalLabels)
	}
	if b.TrustOrgMembers != nil {
		if *b.TrustOrgMembers {
			cfg.Project.LabelDrivenTrustOrgMembers = nil
		} else {
			v := false
			cfg.Project.LabelDrivenTrustOrgMembers = &v
		}
	}
	if b.TrustedBots != nil {
		cfg.Project.LabelDrivenTrustedBots = labelDrivenListOrDefault(*b.TrustedBots)
	}
}

// labelDrivenListOrDefault trims and de-blanks a list; an empty result is
// returned as nil so the config falls back to its built-in default.
func labelDrivenListOrDefault(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func labelDrivenRepoSettings(in map[string]*labelDrivenRepoBody) map[string]*config.LabelDrivenRepoSetting {
	out := make(map[string]*config.LabelDrivenRepoSetting, len(in))
	for repo, b := range in {
		if b == nil {
			out[repo] = nil
			continue
		}
		out[repo] = &config.LabelDrivenRepoSetting{Enabled: b.Enabled, ApprovalLabels: b.ApprovalLabels}
	}
	return out
}

// labelDrivenRepoPayload renders one repo's label-driven state for the Repos
// tab: whether it opted in, its own approval-label override (if any) and the
// labels actually in force (nil when the repo is not label-driven).
func labelDrivenRepoPayload(cfg *config.Config, repo string) map[string]any {
	entry := map[string]any{"enabled": cfg.RepoLabelDriven(repo)}
	if rp, ok := cfg.RepoPolicyFor(repo); ok && len(rp.LabelDrivenApprovalLabels) > 0 {
		entry["approvalLabels"] = append([]string{}, rp.LabelDrivenApprovalLabels...)
	}
	if labels := cfg.LabelDrivenApprovalLabels(repo); labels != nil {
		entry["effectiveLabels"] = labels
	}
	return entry
}

package scheduler

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/housestyle"
	"github.com/hivecommons/hive/pkg/ioscan"
)

// auditActionHouseStyleSkip is the audit action for a house-style candidate
// that was present but not applied (blocked by ioscan, over the cap,
// unreadable). One line per skipped candidate.
const auditActionHouseStyleSkip = "house_style_skip"

// addHouseStyle prepends the resolved house style directly under the kick
// header, ahead of every other static seam, so it is the first rule an agent
// reads on every backend. The repo file is untrusted: it is scanned with the
// issue-body policy and refused whole on a block.
func (s *Scheduler) addHouseStyle(agent, message string) string {
	if message == "" {
		return message
	}
	res := s.resolveHouseStyle(agent)
	return insertAfterKickHeader(message, res.Section())
}

// resolveHouseStyle gathers the inputs for housestyle.Resolve from config and
// the agent's primary-repo checkout, and audits what was skipped.
func (s *Scheduler) resolveHouseStyle(agent string) housestyle.Resolution {
	in := housestyle.Input{}
	if s.cfg != nil {
		in.RepoRoot = s.agentsRepoRoot(s.cfg.PrimaryRepoForAgent(agent))
		in.HiveInline = s.cfg.Project.Style
		in.HiveFile = s.cfg.Project.StyleFile
		in.MaxBytes = s.cfg.Project.StyleMaxBytes
	}
	if s.ioscanEnabled() {
		policy := s.ioscanPolicy()
		in.Scan = func(text string) (bool, string) {
			v := ioscan.ScanInputFrom(text, ioscan.Source{Surface: ioscan.SurfaceIssueBody}, policy)
			if !v.Blocked {
				return false, ""
			}
			s.recordIoscanBlock(v)
			return true, ioscanFindingSummary(v)
		}
	}
	res := housestyle.Resolve(in)
	if log := s.auditLogger(); log != nil {
		for _, sk := range res.Skipped {
			log(auditActionHouseStyleSkip,
				fmt.Sprintf("agent=%s, source=%s, path=%s, reason=%s", agent, sk.Source, sk.Path, sk.Reason),
				ioscanAuditUser)
		}
	}
	if len(res.Skipped) > 0 && s.logger != nil {
		s.logger.Warn("house style: candidate skipped", "agent", agent, "applied", res.Source, "skipped", res.Skipped)
	}
	return res
}

// ioscanFindingSummary names the first finding's rule and severity for the
// audit line; the full list is already recorded by recordIoscanBlock.
func ioscanFindingSummary(v ioscan.Verdict) string {
	if len(v.Findings) == 0 {
		return "blocked"
	}
	f := v.Findings[0]
	return fmt.Sprintf("rule=%s, severity=%s", f.Rule, f.Severity.String())
}

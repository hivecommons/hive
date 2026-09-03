package scheduler

import (
	"strings"

	"github.com/hivecommons/hive/pkg/policies"
)

const formalQualityPolicyPath = "defaults/formal-quality-capability.md"

// addFormalQualityCapability prepends the formal-model authoring contract to
// every quality-role kick when the operator opt-in and ACMM floor both allow
// it. This is deliberately outside template resolution: an operator-authored
// or remotely sourced quality prompt may customize the lane, but cannot make
// quality.formal silently mean something weaker than the artifact/reporting
// contract documented by Hive.
func (s *Scheduler) addFormalQualityCapability(agentName, message string) string {
	if message == "" || s.cfg == nil || s.agentRole(agentName) != "quality" {
		return message
	}
	level := 0
	if s.cfg.ACMMLevel != nil {
		level = *s.cfg.ACMMLevel
	}
	if !s.cfg.Quality.FormalEnabled(level) {
		return message
	}

	data, err := policies.DefaultPolicies.ReadFile(formalQualityPolicyPath)
	if err != nil {
		// The file is embedded at build time, so this can only indicate a broken
		// binary. Fail closed: do not launch a formal-enabled quality kick with
		// an incomplete safety/maintenance contract.
		if s.logger != nil {
			s.logger.Error("formal quality capability contract unavailable", "path", formalQualityPolicyPath, "error", err)
		}
		return ""
	}

	section := strings.TrimSpace(string(data)) + "\n\n"
	if newline := strings.IndexByte(message, '\n'); newline >= 0 {
		return message[:newline+1] + "\n" + section + message[newline+1:]
	}
	return section + message
}

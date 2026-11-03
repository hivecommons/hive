package compliance

import "strings"

// FrameworkDescription returns the one-line summary the Settings →
// Compliance tab shows under a framework in its picker, or "" when none is
// registered. Kept in code rather than in the profile YAML so the strict
// profile schema stays unchanged.
func FrameworkDescription(id string) string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "soc2-type2":
		return "AICPA 2017 Trust Services Criteria: all common criteria plus availability, " +
			"confidentiality, processing integrity and privacy criteria."
	case "fedramp-moderate":
		return "NIST SP 800-53 Rev. 5 Moderate baseline controls required for U.S. federal cloud authorisations."
	case "iso27001-annex-a":
		return "ISO/IEC 27001:2022 Annex A information-security controls (organisational, people, " +
			"physical and technological)."
	}
	return ""
}

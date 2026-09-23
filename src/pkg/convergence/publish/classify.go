package publish

import "strings"

// Sensitivity is the classifier's verdict on one finding.
type Sensitivity string

const (
	// SensitivityPublic: the finding may be filed as a public issue.
	SensitivityPublic Sensitivity = "public"
	// SensitivitySensitive: the finding goes only to the private channel.
	SensitivitySensitive Sensitivity = "security-sensitive"
)

// Classifier decides whether a finding is security-sensitive. It is a named
// seam so an operator can swap the policy; the publisher never files a
// sensitive finding publicly whatever the classifier is.
type Classifier func(Finding) Sensitivity

// securityLexicon is the vocabulary the conservative classifier matches, in
// lowercase, against the finding's predicate, labels, and title. It errs on
// the side of privacy: one hit anywhere routes the finding privately.
var securityLexicon = []string{
	"security", "vuln", "cve", "secret", "credential", "token leak",
	"injection", "xss", "rce", "exploit", "disclosure", "auth bypass",
	"privilege", "unauthenticated",
}

// ConservativeClassifier is the default: any security vocabulary in the
// predicate, labels, or title marks the finding sensitive. A finding whose
// predicate names security explicitly is sensitive even with a bland title.
func ConservativeClassifier(f Finding) Sensitivity {
	fields := append([]string{f.Predicate, f.Title}, f.Labels...)
	for _, field := range fields {
		lower := strings.ToLower(field)
		for _, term := range securityLexicon {
			if strings.Contains(lower, term) {
				return SensitivitySensitive
			}
		}
	}
	return SensitivityPublic
}

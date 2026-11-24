package customers

import "strings"

// NormalizeCompany drops the "@" GitHub profiles put before an organization
// handle ("@ibm" is shown as "ibm") and collapses whitespace.
func NormalizeCompany(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	return strings.Join(strings.Fields(s), " ")
}

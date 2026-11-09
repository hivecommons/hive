package hub

import "strings"

// forgeUnreachableLabel is the row label for a hive whose latest beat said the
// forge host cannot be reached from the pod; "" when it did not.
func forgeUnreachableLabel(e RegistryEntry) string {
	if !e.ForgeUnreachable {
		return ""
	}
	if host := strings.TrimSpace(e.GitHubHost); host != "" {
		return "forge unreachable (" + host + ")"
	}
	return "forge unreachable"
}

// withoutForgeChecks drops the github_auth check, whose failure is a symptom
// of forge egress being down rather than of the hive being unhealthy.
func withoutForgeChecks(names []string) []string {
	kept := names[:0:0]
	for _, n := range names {
		if n == healthCheckGitHubAuth {
			continue
		}
		kept = append(kept, n)
	}
	return kept
}

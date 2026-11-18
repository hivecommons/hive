package kubejob

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// workspaceSubPath turns hive's absolute workspace path into the path relative
// to the claim mount, which is what the Job's volumeMount.subPath needs.
func workspaceSubPath(workspace, claimMount string) (string, error) {
	if claimMount == "" {
		claimMount = DefaultWorkspaceClaimMount
	}
	ws := filepath.Clean(workspace)
	cm := filepath.Clean(claimMount)
	rel, err := filepath.Rel(cm, ws)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("kubejob: workspace %q is not under the workspace claim mount %q — set agent_sandbox.workspace_dir under it, or job.workspace_claim_mount to where the claim is mounted", workspace, claimMount)
	}
	return filepath.ToSlash(rel), nil
}

var labelUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// jobName builds a unique, DNS-label-safe Job name from the launch name and a
// timestamp: hive-kick-<agent>-<unix>. Names must survive as pod-name prefixes.
func jobName(launchName string, now time.Time) string {
	base := sanitizeLabel(strings.TrimSuffix(launchName, "-sandbox"))
	if base == "" {
		base = "agent"
	}
	suffix := fmt.Sprintf("-%d", now.Unix())
	room := jobNameMax - len(namePrefix) - len(suffix)
	if len(base) > room {
		base = strings.TrimRight(base[:room], "-")
	}
	return namePrefix + base + suffix
}

func sanitizeLabel(s string) string {
	s = labelUnsafe.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort: env maps are tiny and this avoids importing sort for
	// one call site.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

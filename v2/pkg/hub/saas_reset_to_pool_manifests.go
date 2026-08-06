package hub

import (
	"fmt"
	"strings"
)

// This file builds the throwaway Kubernetes manifests the reset-to-pool wipe
// applies: the wipe Job, the verify Job, and the placeholder Secret. They are
// piped to `kubectl apply -f -` via stdin (never written to disk with secret
// values, mirroring provisionHive which deletes its manifest right after apply).
//
// SHARED SEED ALLOWLIST. Both the wipe and the verify Jobs derive their find(1)
// prune expression from the SAME resetToPoolSeedAllowlist (via
// resetToPoolFindPruneExpr), so the set of files the wipe KEEPS and the set the
// verify TOLERATES cannot drift. With an empty allowlist the wipe deletes
// everything under /data and the verify tolerates nothing.

const (
	// resetToPoolCleanSentinel is printed by the verify Job when /data contains
	// only the seed allowlist (i.e. nothing unexpected). The hub treats a log
	// consisting solely of this sentinel as "verified clean". Any other non-blank
	// line is a leftover entry -> FAIL CLOSED.
	resetToPoolCleanSentinel = "RESET_TO_POOL_CLEAN"

	// resetToPoolDataMount is where the PVC is mounted in the wipe/verify Jobs.
	resetToPoolDataMount = "/data"
)

// resetToPoolFindPruneExpr builds a find(1) expression fragment that EXCLUDES the
// seed allowlist from a `find /data -mindepth 1` scan. Each allowlisted entry is
// matched at the top level (-maxdepth 1 semantics enforced by the caller's find
// invocation) and pruned. With an empty allowlist this returns "" and nothing is
// excluded — the wipe is total and the verify tolerates nothing.
//
// The allowlist entries are top-level names under /data (e.g. "hive.yaml" would
// exclude /data/hive.yaml). They are shell-quoted defensively even though they
// are compile-time constants, so a future entry with an odd character cannot
// break the expression.
func resetToPoolFindPruneExpr(allowlist []string) string {
	if len(allowlist) == 0 {
		return ""
	}
	var parts []string
	for _, name := range allowlist {
		parts = append(parts, fmt.Sprintf(`-name %s`, shellSingleQuote(name)))
	}
	// ( -name a -o -name b ) -prune -o
	return "( " + strings.Join(parts, " -o ") + " ) -prune -o "
}

// shellSingleQuote wraps s in single quotes, escaping any embedded single quotes,
// for safe inclusion in a /bin/sh command.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resetToPoolWipeScript is the shell the wipe Job runs. It deletes every top-level
// entry under /data except the seed allowlist, then re-asserts by listing what
// remains (should be only the allowlist). Runs under `set -e` so any rm failure
// fails the Job (and thus the reset, closed).
//
// -mindepth 1 -maxdepth 1: operate on top-level entries only; -exec rm -rf {} +
// recurses INTO each, so nested owner-gated trees (per-agent CODEX_HOME chowned
// to an agent uid, .copilot group-rw, sqlite locks) are removed because the Job
// runs as root (uid 0 — see the manifest's securityContext).
func resetToPoolWipeScript(allowlist []string) string {
	prune := resetToPoolFindPruneExpr(allowlist)
	// The wipe: find <mount> -mindepth 1 -maxdepth 1 [prune] -exec rm -rf {} +
	return "set -e; " +
		fmt.Sprintf("echo '[reset-wipe] wiping %s (keeping seed allowlist: %s)'; ", resetToPoolDataMount, allowlistForLog(allowlist)) +
		fmt.Sprintf("find %s -mindepth 1 -maxdepth 1 %s -exec rm -rf {} + ; ", resetToPoolDataMount, prune) +
		fmt.Sprintf("echo '[reset-wipe] done; remaining top-level entries:'; ls -A %s || true", resetToPoolDataMount)
}

// resetToPoolVerifyScript is the shell the verify Job runs. It lists every
// top-level entry under /data that is NOT in the seed allowlist. If there are
// none it prints the clean sentinel; otherwise it prints each leftover path (one
// per line) which the hub reads back as the fail-closed signal.
func resetToPoolVerifyScript(allowlist []string) string {
	prune := resetToPoolFindPruneExpr(allowlist)
	// Collect leftover entries; print sentinel if empty, else the entries.
	return "set -e; " +
		fmt.Sprintf("LEFT=$(find %s -mindepth 1 -maxdepth 1 %s -print); ", resetToPoolDataMount, prune) +
		`if [ -z "$LEFT" ]; then echo ` + resetToPoolCleanSentinel + `; else echo "$LEFT"; fi`
}

func allowlistForLog(allowlist []string) string {
	if len(allowlist) == 0 {
		return "(none — total wipe)"
	}
	return strings.Join(allowlist, ", ")
}

// resetToPoolJobManifest builds a Job manifest running `sh -c <script>` against
// the hive-data PVC, as root, bounded by activeDeadline/backoffLimit. On
// SCC-gated clusters (OpenShift) the Job uses the hive service account so the
// anyuid SCC lets it run as uid 0.
func resetToPoolJobManifest(ns, jobName, script string, cluster *ClusterConfig) string {
	var sb strings.Builder
	sb.WriteString("apiVersion: batch/v1\n")
	sb.WriteString("kind: Job\n")
	sb.WriteString("metadata:\n")
	sb.WriteString("  name: " + jobName + "\n")
	sb.WriteString("  namespace: " + ns + "\n")
	sb.WriteString("spec:\n")
	sb.WriteString(fmt.Sprintf("  activeDeadlineSeconds: %d\n", resetToPoolJobActiveDeadlineSecs))
	sb.WriteString(fmt.Sprintf("  backoffLimit: %d\n", resetToPoolJobBackoffLimit))
	// ttlSecondsAfterFinished lets the cluster GC the Job so it does not linger;
	// long enough that the hub can read its logs first.
	sb.WriteString("  ttlSecondsAfterFinished: 300\n")
	sb.WriteString("  template:\n")
	sb.WriteString("    spec:\n")
	sb.WriteString("      restartPolicy: Never\n")
	if cluster != nil && cluster.RequiresSCC {
		sb.WriteString("      serviceAccountName: " + sccServiceAccountName + "\n")
	}
	// Run as root so the wipe can remove files owned by the agent uids (2001+).
	sb.WriteString("      securityContext:\n")
	sb.WriteString("        runAsUser: 0\n")
	sb.WriteString("      containers:\n")
	sb.WriteString("      - name: reset\n")
	sb.WriteString("        image: " + resetToPoolWipeImage + "\n")
	sb.WriteString("        command: [\"sh\", \"-c\"]\n")
	// The script is a YAML flow scalar; single-quote it and escape embedded quotes.
	sb.WriteString("        args:\n")
	sb.WriteString("        - " + yamlSingleQuote(script) + "\n")
	sb.WriteString("        volumeMounts:\n")
	sb.WriteString("        - name: data\n")
	sb.WriteString("          mountPath: " + resetToPoolDataMount + "\n")
	sb.WriteString("      volumes:\n")
	sb.WriteString("      - name: data\n")
	sb.WriteString("        persistentVolumeClaim:\n")
	sb.WriteString("          claimName: " + hiveDataPVCName + "\n")
	return sb.String()
}

// resetToPoolWipeJobManifest / resetToPoolVerifyJobManifest are the two concrete
// Jobs, sharing resetToPoolSeedAllowlist so wipe and verify cannot drift.
func resetToPoolWipeJobManifest(ns string, cluster *ClusterConfig) string {
	return resetToPoolJobManifest(ns, resetToPoolWipeJobName, resetToPoolWipeScript(resetToPoolSeedAllowlist), cluster)
}

func resetToPoolVerifyJobManifest(ns string, cluster *ClusterConfig) string {
	return resetToPoolJobManifest(ns, resetToPoolVerifyJobName, resetToPoolVerifyScript(resetToPoolSeedAllowlist), cluster)
}

// resetToPoolSecretManifest builds the placeholder hive-secrets Secret: a fresh
// dashboard-token and a placeholder github-token, nothing else. This is the EXACT
// key set a non-App available slot boots with (see resetToPoolPlaceholderSecretKeys).
// dashboardToken is a freshly minted random value; the github-token is the
// non-secret sentinel. Both go in stringData so kubectl base64-encodes them.
func resetToPoolSecretManifest(ns, dashboardToken string) string {
	var sb strings.Builder
	sb.WriteString("apiVersion: v1\n")
	sb.WriteString("kind: Secret\n")
	sb.WriteString("metadata:\n")
	sb.WriteString("  name: " + hiveSecretsName + "\n")
	sb.WriteString("  namespace: " + ns + "\n")
	sb.WriteString("type: Opaque\n")
	sb.WriteString("stringData:\n")
	sb.WriteString("  dashboard-token: " + yamlSingleQuote(dashboardToken) + "\n")
	sb.WriteString("  github-token: " + yamlSingleQuote(resetToPoolPlaceholderToken) + "\n")
	return sb.String()
}

// yamlSingleQuote quotes s as a YAML single-quoted scalar (embedded single quotes
// doubled), safe for a value that may contain shell/YAML metacharacters.
func yamlSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

package hub

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// installRecordingKubectl writes a fake kubectl that appends every invocation's
// argument list to a log file and answers `get namespace ... --ignore-not-found`
// with getStdout / getExit. Returns the log path so a test can assert on what
// was — and crucially was NOT — executed.
//
// Asserting on the recorded commands rather than on a return value is what
// makes these tests real: the whole risk in this lane is a `kubectl delete
// namespace` that should never have run, and only the log can prove it did not.
func installRecordingKubectl(t *testing.T, getStdout string, getExit int) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + logPath + "\n" +
		"case \"$*\" in\n" +
		"  *\"get namespace\"*)\n" +
		"    printf '%s' '" + getStdout + "'\n" +
		"    exit " + strconv.Itoa(getExit) + "\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// kubectlCalls returns the recorded invocations, one per line.
func kubectlCalls(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func containsDeleteNamespace(calls []string, ns string) bool {
	for _, c := range calls {
		if strings.Contains(c, "delete namespace "+ns) {
			return true
		}
	}
	return false
}

// TestHostedNamespaceExistedBeforeApplyDistinguishesAbsentFromUnknown is the
// load-bearing test for the rollback's safety.
//
// The whole decision rests on telling "kubectl says the namespace is not there"
// apart from "kubectl could not tell me". A plain `get namespace` collapses
// both into a non-zero exit, which is why the pre-check passes
// --ignore-not-found. If that flag were ever dropped, the absent case would
// start returning Unknown, the rollback would stop running, and the leak would
// silently come back — this test is what catches that.
func TestHostedNamespaceExistedBeforeApplyDistinguishesAbsentFromUnknown(t *testing.T) {
	cases := []struct {
		name     string
		stdout   string
		exit     int
		want     namespacePresence
		why      string
		pullOnly bool
	}{
		{
			name:   "absent",
			stdout: "",
			exit:   0,
			want:   namespacePresenceAbsent,
			why:    "--ignore-not-found exits 0 with empty output for a namespace that does not exist",
		},
		{
			name:   "present",
			stdout: "namespace/hive-hosted-abc",
			exit:   0,
			want:   namespacePresentBeforeApply,
			why:    "a name on stdout means the namespace was already there and is not ours to delete",
		},
		{
			name:   "check failed",
			stdout: "",
			exit:   1,
			want:   namespacePresenceUnknown,
			why:    "a failed check must never be read as 'absent' — that reading ends in a namespace delete",
		},
		{
			name:     "pull-only cluster",
			pullOnly: true,
			want:     namespacePresenceUnknown,
			why:      "the hub has no kubectl path into a pull-only pool, so it cannot know",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installRecordingKubectl(t, tc.stdout, tc.exit)
			cluster := &ClusterConfig{ID: "hive-oke", InCluster: !tc.pullOnly, PullOnly: tc.pullOnly}
			got := hostedNamespaceExistedBeforeApply(cluster, "hive-hosted-abc")
			if got != tc.want {
				t.Errorf("presence = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestRollbackProvisionNamespaceDeletesOnlyWhatItCreated asserts the delete is
// issued for exactly one of the three pre-apply states, and asserts it against
// the RECORDED kubectl invocations rather than the return value, because the
// harm this guards against is an executed delete, not a returned bool.
func TestRollbackProvisionNamespaceDeletesOnlyWhatItCreated(t *testing.T) {
	const ns = "hive-hosted-hosted-available-oke-03-placeholder-y99x"

	cases := []struct {
		name       string
		before     namespacePresence
		wantDelete bool
		why        string
	}{
		{
			name:       "absent before the apply",
			before:     namespacePresenceAbsent,
			wantDelete: true,
			why:        "the failed apply created it seconds ago; nothing else can be using it, and leaving it is the leak #5768 is about",
		},
		{
			name:       "already existed",
			before:     namespacePresentBeforeApply,
			wantDelete: false,
			why:        "deleting a pre-existing namespace cascades to the PVCs and pods of a live spoke",
		},
		{
			name:       "could not tell",
			before:     namespacePresenceUnknown,
			wantDelete: false,
			why:        "'could not tell' must never be resolved into a destructive verb",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logPath := installRecordingKubectl(t, "", 0)
			cluster := &ClusterConfig{ID: "hive-oke", InCluster: true}

			issued := rollbackProvisionNamespace(cluster, ns, tc.before, slog.Default())
			if issued != tc.wantDelete {
				t.Errorf("rollbackProvisionNamespace reported issued=%v, want %v — %s", issued, tc.wantDelete, tc.why)
			}

			calls := kubectlCalls(t, logPath)
			deleted := containsDeleteNamespace(calls, ns)
			if deleted != tc.wantDelete {
				t.Errorf("kubectl delete namespace %s executed=%v, want %v — %s (calls: %v)", ns, deleted, tc.wantDelete, tc.why, calls)
			}
		})
	}
}

// TestRollbackProvisionNamespaceDoesNotWaitForTermination asserts the delete
// carries --wait=false and --ignore-not-found.
//
// Neither flag is cosmetic. Without --wait=false a namespace held by a PVC
// finalizer pins the provisioning queue slot (provision_queue.go bounds
// concurrency hub-wide) for as long as the terminate takes, so one bad
// provision would throttle every other one. Without --ignore-not-found the
// overwhelmingly common case — the apply failed before creating anything —
// logs a spurious failure that reads as a leaked namespace when there is none.
func TestRollbackProvisionNamespaceDoesNotWaitForTermination(t *testing.T) {
	const ns = "hive-hosted-abc"
	logPath := installRecordingKubectl(t, "", 0)
	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true}

	rollbackProvisionNamespace(cluster, ns, namespacePresenceAbsent, slog.Default())

	calls := kubectlCalls(t, logPath)
	var del string
	for _, c := range calls {
		if strings.Contains(c, "delete namespace") {
			del = c
			break
		}
	}
	if del == "" {
		t.Fatalf("no delete was issued at all; calls: %v", calls)
	}
	if !strings.Contains(del, "--wait=false") {
		t.Errorf("delete %q lacks --wait=false, so a namespace with a stuck finalizer would pin a provisioning queue slot", del)
	}
	if !strings.Contains(del, "--ignore-not-found") {
		t.Errorf("delete %q lacks --ignore-not-found, so the common 'apply failed before creating anything' case logs a false leak", del)
	}
}

// TestRollbackProvisionNamespaceIgnoresBlankInputs asserts the rollback is a
// no-op rather than a wildcard when it is handed nothing to act on. A blank
// namespace reaching `kubectl delete namespace` would be a syntax error today,
// but the guard is what keeps it from ever becoming something worse.
func TestRollbackProvisionNamespaceIgnoresBlankInputs(t *testing.T) {
	logPath := installRecordingKubectl(t, "", 0)
	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true}

	if rollbackProvisionNamespace(cluster, "   ", namespacePresenceAbsent, slog.Default()) {
		t.Error("a blank namespace produced a delete")
	}
	if rollbackProvisionNamespace(nil, "hive-hosted-abc", namespacePresenceAbsent, slog.Default()) {
		t.Error("a nil cluster produced a delete")
	}
	if calls := kubectlCalls(t, logPath); len(calls) != 0 {
		t.Errorf("kubectl ran %d times for blank inputs: %v", len(calls), calls)
	}
}

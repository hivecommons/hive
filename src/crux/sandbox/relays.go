package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Sandbox relays (#18512).
//
// A sandboxed kick runs in the project's own image, which knows nothing about
// hive. The relays an agent needs to hand work back to hive (hive-review,
// hive-open-issue and hive-topic, each of which writes a signed request into
// the kick's request spool, #14986, #18572, #18573) are therefore supplied by hive itself on every launch rather than
// baked into the project image: the launcher puts hive's own copy in a
// read-only directory at RelayMountPath and prepends that directory to PATH.
// Hive's copy always wins over one the image carries, because the relay and
// the spool protocol it speaks ship together in the hive image.
//
// Only relays that work from a sandbox belong in RelayNames. A relay that
// needs the pod-local drop boxes (/var/run/hive-metrics/...) cannot reach them
// from a sandbox and stays out until it has a spool of its own.

const (
	// RelayMountPath is where the relays appear inside the sandbox.
	RelayMountPath = "/opt/hive/relays"
	// RelaySourceDirEnv, when set, names the directory hive reads its relays
	// from on the host (Podman). Unset, each relay is looked up on hive's own
	// PATH, which in the hive image resolves to /usr/local/bin.
	RelaySourceDirEnv = "HIVE_SANDBOX_RELAY_DIR"
	// RelayPreflightExitCode is the sandbox exit code when the image lacks a
	// tool the relays need (EX_CONFIG): the kick fails with the preflight's
	// message in its transcript instead of an agent that cannot post.
	RelayPreflightExitCode = 78
	// relayPreflightName is $0 of the preflight shell, so a failure in the
	// transcript says where it came from.
	relayPreflightName = "hive-relay-preflight"
	relayShell         = "/bin/sh"
	relayFileMode      = 0o555
)

// RelayNames are the sandbox-safe relays hive supplies to every sandbox.
var RelayNames = []string{"hive-review", "hive-open-issue", "hive-topic"}

// RelayRequirements are the interpreters the relays need inside the image.
var RelayRequirements = []string{"bash", "python3"}

// relayPreflightScript checks the image for every RelayRequirements tool,
// prepends the relay directory ($1) to PATH and execs the kick command.
var relayPreflightScript = buildRelayPreflightScript()

func buildRelayPreflightScript() string {
	return fmt.Sprintf(`relay_dir="$1"; shift
for tool in %s; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf 'hive: this sandbox image has no %%s on PATH. Hive supplies its sandbox relays (%s) to every sandboxed kick, and they need %s; add %%s to the image.\n' "$tool" "$tool" >&2
    exit %d
  fi
done
gaps=0
if [ -r /proc/self/status ]; then
  nnp=; cap=; sec=
  while read -r key val; do
    case "$key" in NoNewPrivs:) nnp=$val;; CapEff:) cap=$val;; Seccomp:) sec=$val;; esac
  done < /proc/self/status
  [ "$nnp" = 1 ] || { echo 'hive: sandbox containment gap: no_new_privs is not set' >&2; gaps=1; }
  case "$cap" in *[!0]*|"") echo "hive: sandbox containment gap: capabilities not dropped (CapEff=$cap)" >&2; gaps=1;; esac
  [ -n "$sec" ] && [ "$sec" != 0 ] || { echo 'hive: sandbox containment gap: no seccomp filter' >&2; gaps=1; }
else
  echo 'hive: sandbox containment gap: /proc/self/status is unreadable, so containment cannot be verified' >&2; gaps=1
fi
case "$HIVE_REQUIRE_HARDENED" in 1|true|yes|on) [ "$gaps" = 0 ] || exit %d;; esac
PATH="$relay_dir:$PATH"; export PATH
exec "$@"`, strings.Join(RelayRequirements, " "), strings.Join(RelayNames, ", "), strings.Join(RelayRequirements, " and "), RelayPreflightExitCode, RelayPreflightExitCode)
}

// RelayCommand wraps a kick command so it runs with the relays first on PATH
// after a preflight that fails the kick, with a clear message, when the image
// lacks bash or python3. An empty command (the image's own entrypoint or CMD)
// cannot be wrapped and is returned unchanged; the relays are still mounted.
func RelayCommand(command []string) []string {
	if len(command) == 0 {
		return command
	}
	out := make([]string, 0, len(command)+4)
	out = append(out, relayShell, "-c", relayPreflightScript, relayPreflightName, RelayMountPath)
	return append(out, command...)
}

// relayLookPath resolves a relay on hive's own PATH; a variable for tests.
var relayLookPath = exec.LookPath

// relaySource returns the host path of one relay.
func relaySource(name string) (string, error) {
	if dir := strings.TrimSpace(os.Getenv(RelaySourceDirEnv)); dir != "" {
		return filepath.Join(dir, name), nil
	}
	return relayLookPath(name)
}

// StageRelays copies hive's relays into a fresh private directory for a
// Podman bind mount and returns it with a cleanup function. Copying, rather
// than mounting hive's own /usr/local/bin entries, lets Podman relabel the
// mount (:Z) without touching the files hive itself runs.
func StageRelays() (string, func(), error) {
	dir, err := os.MkdirTemp("", "hive-relays-")
	if err != nil {
		return "", func() {}, fmt.Errorf("sandbox: staging relays: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	for _, name := range RelayNames {
		src, err := relaySource(name)
		if err == nil {
			err = copyRelay(src, filepath.Join(dir, name))
		}
		if err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("sandbox: hive's %s relay is not available to mount into the sandbox (set %s to the directory holding it): %w", name, RelaySourceDirEnv, err)
		}
	}
	return dir, cleanup, nil
}

func copyRelay(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, relayFileMode)
}

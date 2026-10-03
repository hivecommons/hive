// Command hive-quota-publisher publishes contributor subscription quota
// readings for a STANDALONE contributor (hivecommons/hive#10299).
//
// The contributor relay's quota guard evaluates a reading; something has to
// produce it. In server mode the `hive` process does (src/cmd/hive/main.go
// starts rotation.NewContributorReadingPublisher), but the standalone
// contributor launch paths — the contributor image entrypoint, the isolated
// Compose example and `just contribute-hive <cli> local` — run no `hive`
// process, so the guard found no source, reported that it was not guarding
// anything and admitted work. This binary is the publisher those paths start:
// it probes the contributor's own backend with the contributor's own
// credentials and publishes the normalized reading into the pool directory the
// relay already reads from.
//
// It is deliberately a thin wrapper. Every provider schema conversion, the
// atomic pool-keyed write, the presence marker, the "a failed probe never
// publishes a healthy reading" rule and the five-minute bounded poll with
// provider-error backoff all live in pkg/rotation and are shared, byte for
// byte, with the server-mode publisher. Nothing here makes an admission
// decision: the JS relay remains the single guard authority (#6955).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hivecommons/hive/pkg/rotation"
)

const (
	// envPublish is the explicit opt-out. Publishing is ON by default in a
	// standalone contributor, which is the whole point of #10299.
	envPublish = "HIVE_CONTRIBUTOR_QUOTA_PUBLISH"
	// envGuard is the relay's guard mode. With the guard off there is nothing
	// to feed, so probing the provider CLI every five minutes would be pure
	// cost.
	envGuard = "HIVE_CONTRIBUTOR_QUOTA_GUARD"
	// envReadingFile / envReadingJSON are the operator's explicitly configured
	// external reading sources. When one is set the operator has declared
	// where readings come from; a second publisher would compete with it.
	envReadingFile = "HIVE_CONTRIBUTOR_QUOTA_READING_FILE"
	envReadingJSON = "HIVE_CONTRIBUTOR_QUOTA_READING_JSON"
	envPoolDir     = "HIVE_CONTRIBUTOR_QUOTA_POOL_DIR"
	envPoolAccount = "HIVE_CONTRIBUTOR_QUOTA_POOL_ACCOUNT"
	// envBackend matches BACKEND in bin/contributor-relay.js, including its
	// `claude` default, so the publisher and the relay always resolve the same
	// pool.
	envBackend = "AGENT_BACKEND"
)

// plan is the startup decision: publish, or stand down with a stated reason.
// Keeping it a value makes every branch testable without a process.
type plan struct {
	publish bool
	reason  string
	backend string
	poolDir string
	account string
}

func main() {
	os.Exit(run(os.Getenv, os.Stdout, os.Stderr, signalContext, startPublisher))
}

// signalContext ends on SIGINT or SIGTERM, so the publisher stops with the
// contributor that started it.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// startPublisher starts the real publish-only rotation manager for p and
// returns a channel closed once its loop has stopped. It is a seam so run() can
// be tested without probing provider CLIs. ok is false for a backend with no
// reader, and then there is nothing to wait for.
func startPublisher(ctx context.Context, p plan) (<-chan struct{}, bool) {
	mgr, ok := rotation.NewContributorBackendReadingPublisher(p.poolDir, p.account, p.backend)
	if !ok {
		return nil, false
	}
	return mgr.StartPublishing(ctx), true
}

// run resolves the plan, reports it, and (when publishing) blocks until the
// context ends. It always exits 0: a contributor that cannot publish must keep
// contributing — the relay's own no-source handling, not a dead entrypoint, is
// what decides whether work is admitted.
func run(
	getenv func(string) string,
	stdout, stderr io.Writer,
	ctxFn func() (context.Context, context.CancelFunc),
	start func(context.Context, plan) (<-chan struct{}, bool),
) int {
	p := resolvePlan(getenv, rotation.DefaultContributorPoolDir)
	if !p.publish {
		fmt.Fprintf(stderr, "hive-quota-publisher: not publishing — %s\n", p.reason)
		fmt.Fprintln(stderr, "hive-quota-publisher: quota protection is only active if some other source feeds the guard.")
		return 0
	}
	ctx, stop := ctxFn()
	defer stop()
	done, ok := start(ctx, p)
	if !ok {
		fmt.Fprintf(stderr, "hive-quota-publisher: not publishing — backend %q has no quota reader\n", p.backend)
		return 0
	}
	fmt.Fprintf(stdout, "hive-quota-publisher: publishing %s quota readings every 5m0s into %s\n", p.backend, p.poolDir)
	fmt.Fprintln(stdout, "hive-quota-publisher: quota protection active — the relay holds new work below its configured reserve.")
	<-ctx.Done()
	// Wait for the publish loop's last write before exiting: the process
	// holding the pool's presence marker must not disappear mid-rename.
	<-done
	fmt.Fprintln(stdout, "hive-quota-publisher: stopped.")
	return 0
}

// resolvePlan decides whether this standalone contributor should publish.
// Order matters: an explicit opt-out outranks everything, an operator-declared
// external source is never competed with, and an unsupported backend is named
// as unsupported rather than silently left to wait.
func resolvePlan(getenv func(string) string, defaultPoolDir func() string) plan {
	backend := strings.ToLower(strings.TrimSpace(getenv(envBackend)))
	if backend == "" {
		backend = "claude"
	}
	p := plan{backend: backend, account: strings.TrimSpace(getenv(envPoolAccount))}

	if isOptOut(getenv(envPublish)) {
		p.reason = envPublish + " is set to an opt-out value"
		return p
	}
	if strings.ToLower(strings.TrimSpace(getenv(envGuard))) == "off" {
		p.reason = "the contributor quota guard is off (" + envGuard + "=off)"
		return p
	}
	if source := configuredReadingSource(getenv); source != "" {
		p.reason = source + " already supplies readings; not competing with it"
		return p
	}
	if _, ok := rotation.ContributorGuardBackendProvider(backend); !ok {
		p.reason = "backend " + backend + " is not a supported subscription backend for the quota guard"
		return p
	}
	p.poolDir = strings.TrimSpace(getenv(envPoolDir))
	if p.poolDir == "" {
		p.poolDir = defaultPoolDir()
	}
	if p.poolDir == "" {
		p.reason = "no quota pool directory could be resolved (set " + envPoolDir + ")"
		return p
	}
	p.publish = true
	return p
}

// configuredReadingSource names the external reading source the operator
// configured, or "" when none is.
func configuredReadingSource(getenv func(string) string) string {
	if strings.TrimSpace(getenv(envReadingJSON)) != "" {
		return envReadingJSON
	}
	if strings.TrimSpace(getenv(envReadingFile)) != "" {
		return envReadingFile
	}
	return ""
}

// isOptOut reads the opt-out variable. Only an explicit false-ish value opts
// out; anything else (including unset) keeps the default-on behaviour.
func isOptOut(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return true
	default:
		return false
	}
}

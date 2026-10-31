package watchdog

// Functional options for New (reconciler.go). Kept apart from the control
// loop so the reconciler file stays under the Crux file-size ratchet.

import (
	"time"

	"github.com/hivecommons/hive/crux"
)

// Option customizes a Reconciler.
type Option func(*Reconciler)

// WithClock injects a fake clock for tests.
func WithClock(now func() time.Time) Option {
	return func(r *Reconciler) { r.now = now }
}

// WithAuthProbes registers per-provider credential probes.
func WithAuthProbes(probes map[string]AuthProbe) Option {
	return func(r *Reconciler) { r.authProbes = probes }
}

// WithBackendProvider injects the backend→provider mapping.
func WithBackendProvider(f func(string) string) Option {
	return func(r *Reconciler) { r.backendProvider = f }
}

// WithConditionReporter injects the host sink for the watchdog's
// agent_crash_loop self-report (hive wires pkg/fleetreport). Without one the
// crash loop still pauses and alerts; it just is not reported upstream.
func WithConditionReporter(rep crux.ConditionReporter) Option {
	return func(r *Reconciler) { r.conditions = rep }
}

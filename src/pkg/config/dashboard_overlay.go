package config

import (
	"log"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func (c *Config) saveDashboardOverlay() error {
	if !IsKubernetesPod() {
		// Docker/LXC mode: RuntimeConfigFile is already the boot-time
		// source of truth there, so dashboard saves persist without an
		// overlay.
		return nil
	}
	data, err := c.dashboardOverlayBytes()
	if err != nil {
		log.Printf("[config] warning: failed to marshal dashboard overlay: %v", err)
		return err
	}
	tmpPath := DashboardOverlayFile + ".tmp"
	// 0600, not 0644: dashboardOverlayBytes only folds the dashboard auth
	// token back to its env form when it matches a bootstrap env var — a
	// dashboard-minted token is persisted verbatim, so the overlay is not
	// reliably secret-free (#5331).
	const overlayFileMode = 0o600
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, overlayFileMode)
	if err != nil {
		log.Printf("[config] warning: failed to open dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	// OpenFile's mode only applies on create; a leftover 0644 tmp file from a
	// crash before this fix would otherwise carry its old bits through the
	// rename. Best-effort: the rename below installs whatever mode f has.
	_ = f.Chmod(overlayFileMode)
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // best-effort cleanup; the write error is what's returned
		log.Printf("[config] warning: failed to write dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // best-effort cleanup; the sync error is what's returned
		log.Printf("[config] warning: failed to fsync dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := f.Close(); err != nil {
		log.Printf("[config] warning: failed to close dashboard overlay temp file %s (dashboard saves will not survive pod restarts): %v", tmpPath, err)
		return err
	}
	if err := os.Rename(tmpPath, DashboardOverlayFile); err != nil {
		log.Printf("[config] warning: failed to rename dashboard overlay into place %s (dashboard saves will not survive pod restarts): %v", DashboardOverlayFile, err)
		return err
	}
	log.Printf("[config] dashboard overlay written to %s (merged over the ConfigMap seed at next boot)", DashboardOverlayFile)
	return nil
}

// dashboardOverlayBytes marshals the config with env-derived secret VALUES
// collapsed back to their env-var forms, so the PVC overlay stays
// secret-free. Load() re-expands ${VAR} references and applyBootstrapEnv
// re-fills the dashboard auth token from the pod env, so nothing is lost.
func (c *Config) dashboardOverlayBytes() ([]byte, error) {
	// Shallow copy: top-level fields are struct values, so mutating the
	// copy's GitHub/Dashboard sections leaves the live config untouched
	// (the shared Agents map is not modified).
	cp := *c
	if tok := os.Getenv("HIVE_GITHUB_TOKEN"); tok != "" && cp.GitHub.Token == tok {
		cp.GitHub.Token = "${HIVE_GITHUB_TOKEN}"
	}
	for _, env := range []string{"DASHBOARD_AUTH_TOKEN", "HIVE_DASHBOARD_TOKEN"} {
		if v := os.Getenv(env); v != "" && cp.Dashboard.AuthToken == v {
			cp.Dashboard.AuthToken = ""
			break
		}
	}
	cp = *cp.redactedForPersist()
	return yaml.Marshal(&cp)
}

func (c *Config) redactedForPersist() *Config {
	if c == nil {
		return nil
	}
	cp := *c
	cp.OTel.Headers = envRedactedHeaders(cp.OTel.Headers)
	cp.Tracing.Headers = envRedactedHeaders(cp.Tracing.Headers)
	// Work-source credentials are persisted verbatim. The dashboard PUT stores
	// the operator's literal `${LINEAR_API_KEY}` reference (API saves are not
	// env-expanded) and worksource.FromConfig resolves it at the point of use,
	// so the reference round-trips through the overlay unchanged. Do NOT try
	// to "fold" a value back into ${VAR} by scanning the environment: any env
	// value that is a substring of the key (CI's ACCEPT_EULA=Y rewrote the
	// trailing Y of the literal reference) corrupts it.
	// #4041: never write the built-in login-pattern defaults as explicit
	// values. applyDefaults fills LoginPatterns on load, so by save time the
	// in-memory list always LOOKS explicit; marshaling it pins today's
	// defaults into the persisted config, where "defaults only apply to an
	// empty list" freezes them forever — exactly how every pre-#3959 hive
	// ended up stuck with the false-positive-prone generic list. A list equal
	// to the current defaults expresses no operator intent: persist it as
	// absent so future default fixes reach existing hives. An operator-
	// customized list differs from the defaults and is persisted verbatim.
	if stringSlicesEqual(cp.Governor.Sensing.LoginPatterns, defaultLoginPatterns) {
		cp.Governor.Sensing.LoginPatterns = nil
	}
	return &cp
}

func mergeOTelOverride(base, override OTelConfig) OTelConfig {
	merged := base
	if override.Enabled {
		merged.Enabled = true
	}
	if override.Endpoint != "" {
		merged.Endpoint = override.Endpoint
	}
	if len(override.Headers) > 0 {
		merged.Headers = override.Headers
	}
	if override.ServiceName != "" {
		merged.ServiceName = override.ServiceName
	}
	if override.Insecure {
		merged.Insecure = true
	}
	if override.SampleRatio != 0 {
		merged.SampleRatio = override.SampleRatio
	}
	return merged
}

func envRedactedHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return headers
	}
	out := make(map[string]string, len(headers))
	for k, value := range headers {
		out[k] = redactEnvExpandedValue(value)
	}
	return out
}

func redactEnvExpandedValue(value string) string {
	type envValue struct {
		name  string
		value string
	}
	values := make([]envValue, 0)
	for _, pair := range os.Environ() {
		name, val, ok := strings.Cut(pair, "=")
		if !ok || val == "" {
			continue
		}
		values = append(values, envValue{name: name, value: val})
	}
	sort.SliceStable(values, func(i, j int) bool {
		return len(values[i].value) > len(values[j].value)
	})

	redacted := value
	for _, item := range values {
		redacted = strings.ReplaceAll(redacted, item.value, "${"+item.name+"}")
	}
	return redacted
}

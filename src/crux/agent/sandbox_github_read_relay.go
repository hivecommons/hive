package agent

import (
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

// SandboxGitHubReadRelay is the relay a sandbox Job sends its GitHub reads
// through (#18865); see crux.SandboxGitHubReadRelay.
type SandboxGitHubReadRelay = crux.SandboxGitHubReadRelay

// sandboxReadRelayState is the Manager's relay wiring; the zero value is off.
// The relay and its extra NO_PROXY hosts are stored as one value so a reader
// never sees one from a previous install paired with the other.
type sandboxReadRelayState struct {
	cur atomic.Pointer[sandboxReadRelayInstall]
}

type sandboxReadRelayInstall struct {
	relay   SandboxGitHubReadRelay
	noProxy []string
}

// SetSandboxGitHubReadRelay installs (or, with nil, removes) the relay that
// carries sandbox Job GitHub reads (#18865). noProxy adds hosts Jobs reach
// directly.
func (m *Manager) SetSandboxGitHubReadRelay(relay SandboxGitHubReadRelay, noProxy []string) {
	if relay == nil {
		m.sandboxReadRelay.cur.Store(nil)
		return
	}
	m.sandboxReadRelay.cur.Store(&sandboxReadRelayInstall{relay: relay, noProxy: append([]string(nil), noProxy...)})
}

const (
	// sandboxReadCARelPath is where the trust bundle is written in the kick's
	// workspace; the Job sees it under sandbox.DefaultWorkspaceMount.
	sandboxReadCARelPath  = ".hive/github-read-ca.pem"
	sandboxReadCAFileMode = 0o644
	sandboxReadCADirMode  = 0o755
	// sandboxReadBaseNoProxy keeps cluster-local traffic off the relay.
	sandboxReadBaseNoProxy = "localhost,127.0.0.1,::1,.svc,.cluster.local"
)

// sandboxReadEnv is the environment that points a Job's HTTPS clients at the
// relay and at the CA bundle. It carries only the proxy URL (relay key) and a
// file path: no GitHub credential exists on this path, and
// TestSandboxReadEnvCarriesNoGitHubToken pins that.
func sandboxReadEnv(proxyURL, caPath string, extraNoProxy []string) map[string]string {
	noProxy := sandboxReadBaseNoProxy
	for _, h := range extraNoProxy {
		if h = strings.TrimSpace(h); h != "" {
			noProxy += "," + h
		}
	}
	return map[string]string{
		"HTTPS_PROXY":         proxyURL,
		"https_proxy":         proxyURL,
		"NO_PROXY":            noProxy,
		"no_proxy":            noProxy,
		"SSL_CERT_FILE":       caPath,
		"GIT_SSL_CAINFO":      caPath,
		"REQUESTS_CA_BUNDLE":  caPath,
		"CURL_CA_BUNDLE":      caPath,
		"NODE_EXTRA_CA_CERTS": caPath,
		// libcurl (so git) verifies an https PROXY against its own CA setting,
		// separate from the one for the origin server: GIT_SSL_CAINFO alone
		// does not cover it. Git reads this entry from the environment.
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "http.proxySSLCAInfo",
		"GIT_CONFIG_VALUE_0": caPath,
	}
}

// mergeSandboxEnv returns base overlaid with extra; neither input is mutated.
// GIT_CONFIG_* entries in extra are dropped when base already defines
// GIT_CONFIG_COUNT, so the relay never rewrites a git config the Job has set.
func mergeSandboxEnv(base, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	_, baseGitConfig := base["GIT_CONFIG_COUNT"]
	for k, v := range extra {
		if baseGitConfig && strings.HasPrefix(k, "GIT_CONFIG_") {
			continue
		}
		out[k] = v
	}
	return out
}

// enrollGitHubReadRelay routes this kick's Job through the read relay. It is
// best effort by design: the relay only improves rate limits, so any failure
// is logged and the kick runs as it did before, never failing the work. The
// returned release must be called when the kick ends; it is never nil.
func (e *SandboxExecutor) enrollGitHubReadRelay(spec SandboxKickSpec, workspace string) (map[string]string, func()) {
	noop := func() {}
	if e.GitHubReadRelay == nil || spec.Runtime != config.SandboxRuntimeJob {
		return nil, noop
	}
	proxyURL, bundle, release, err := e.GitHubReadRelay.Enroll(spec.Agent)
	if err != nil {
		e.warnReadRelay(spec.Agent, "enroll", err)
		return nil, noop
	}
	caHost := filepath.Join(workspace, filepath.FromSlash(sandboxReadCARelPath))
	writeErr := os.MkdirAll(filepath.Dir(caHost), sandboxReadCADirMode)
	if writeErr == nil {
		writeErr = os.WriteFile(caHost, bundle, sandboxReadCAFileMode)
	}
	if err = writeErr; err != nil {
		release()
		e.warnReadRelay(spec.Agent, "write CA bundle", err)
		return nil, noop
	}
	caJob := path.Join(sandbox.DefaultWorkspaceMount, sandboxReadCARelPath)
	return sandboxReadEnv(proxyURL, caJob, e.GitHubReadRelayNoProxy), release
}

func (e *SandboxExecutor) warnReadRelay(agentName, step string, err error) {
	if e.Logger != nil {
		e.Logger.Warn("sandbox GitHub read relay unavailable; this kick's Job reads GitHub anonymously", slog.String("agent", agentName), slog.String("step", step), slog.String("error", err.Error()))
	}
}

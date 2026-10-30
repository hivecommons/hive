package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/inferencehealth"
)

const (
	proxyListenPort        = 18443
	InferenceTranslatePort = 18444
	modeFilePrefix         = "/tmp/.hive-mode-"
	// capsFilePrefix holds the per-agent ORTHOGONAL capability set (#4492) — the
	// tokens agent.AgentCapabilities.String() writes. Deliberately a SECOND file
	// rather than a richer format in the mode file: bin/gh-wrapper.sh reads
	// /tmp/.hive-mode-<agent> under `set -e` and expects a bare mode string, so
	// changing that content would break every gh call on every hive.
	// A missing or empty file means no capabilities, which is the default.
	capsFilePrefix         = "/tmp/.hive-caps-"
	maxViolationLog        = 1000
	maxGitHubWriteBodyScan = 2 << 20

	// defaultProxyEgressMark is the fwmark (SO_MARK) the proxy stamps onto its
	// OWN upstream :443 dials so the forced-egress iptables redirect can exempt
	// them (via `-m mark --mark <mark> -j RETURN`) and avoid looping the proxy's
	// traffic back into itself.
	//
	// This replaces the `-m owner --uid-owner` self-exemption, which needs the
	// xt_owner kernel module — absent on OpenShift/OVN nodes. A packet mark works
	// on both OKE and OpenShift. The value is arbitrary but MUST match the mark
	// the entrypoint exempts; entrypoint.sh is the single source of truth and
	// exports HIVE_PROXY_EGRESS_MARK (defaulting to this same value) into the
	// proxy process, which reads it via proxyEgressMark().
	defaultProxyEgressMark = 0x1112

	// proxyEgressMarkEnv is the env var the entrypoint exports to keep the proxy
	// and the iptables rule in sync from one source of truth.
	proxyEgressMarkEnv = "HIVE_PROXY_EGRESS_MARK"
)

// proxyEgressMark returns the fwmark to stamp on the proxy's upstream dials,
// read from HIVE_PROXY_EGRESS_MARK (set by entrypoint.sh) so the Go proxy and
// the iptables exemption stay in lockstep. Accepts hex (0x1112) or decimal.
// Falls back to defaultProxyEgressMark when unset or unparseable — that default
// matches the entrypoint default, so an accidental drift fails safe by staying
// on the agreed value rather than silently disabling the exemption.
func proxyEgressMark() int {
	raw := strings.TrimSpace(os.Getenv(proxyEgressMarkEnv))
	if raw == "" {
		return defaultProxyEgressMark
	}
	// ParseInt with base 0 honours a 0x prefix (hex) and plain decimal.
	if v, err := strconv.ParseInt(raw, 0, 64); err == nil && v > 0 {
		return int(v)
	}
	return defaultProxyEgressMark
}

// markDialer returns a *net.Dialer whose Control hook stamps the proxy egress
// fwmark (SO_MARK) on the outbound socket before connect. On non-Linux builds
// setSockMark is a no-op, so the dialer is portable. The optional timeout is
// applied when non-zero.
//
// SO_MARK is a BEST-EFFORT egress hint, never a hard requirement: it exists so
// the forced-egress iptables redirect can exempt the proxy's own upstream dials
// via `-m mark`. But setsockopt(SO_MARK) needs CAP_NET_ADMIN in the process's
// EFFECTIVE set, and the hive Go process runs as a non-root user (entrypoint
// drops from root to `dev` via gosu, which strips all capabilities). So the mark
// call fails with EPERM ("operation not permitted") on exactly the deployments
// that run non-root — which is all of them. Returning that error from the
// Control hook ABORTS the dial, so every proxy upstream dial fails and agents
// can't reach GitHub at all. That is a regression: on OKE the `-m owner
// --uid-owner` exemption already covers the proxy's uid dials, so an unmarked
// dial succeeds; on OpenShift (no xt_owner) the mark is the only exemption, but
// there too, failing the dial helps nothing. So we log the mark failure ONCE and
// proceed unmarked rather than killing the connection.
func markDialer(timeout time.Duration) *net.Dialer {
	mark := proxyEgressMark()
	return &net.Dialer{
		Timeout: timeout,
		Control: func(_, _ string, c syscall.RawConn) error {
			// A failure to enter the raw-conn control context is a real dial
			// problem (the fd is unusable) — propagate it.
			if err := c.Control(func(fd uintptr) {
				if markErr := setSockMark(fd, mark); markErr != nil {
					warnSockMarkOnce(mark, markErr)
				}
			}); err != nil {
				return err
			}
			// Deliberately do NOT propagate a setSockMark failure: the mark is a
			// best-effort exemption hint, and the owner-UID iptables RETURN
			// already exempts the proxy's own uid on OKE, so an unmarked dial
			// still reaches GitHub.
			return nil
		},
	}
}

// warnSockMarkOnce logs the first SO_MARK failure at WARN and every subsequent
// one at DEBUG, so a non-CAP_NET_ADMIN deployment (the common non-root case)
// does not flood the log with one line per dial while still surfacing the
// condition once.
var sockMarkWarnOnce sync.Once

func warnSockMarkOnce(mark int, err error) {
	sockMarkWarnOnce.Do(func() {
		slog.Warn("proxy egress SO_MARK unavailable — dialing unmarked (owner-UID exemption still applies on OKE); this is expected when the hive runs non-root without CAP_NET_ADMIN in its effective set",
			"mark", mark, "error", err)
	})
}

// CACertPath and caKeyPath are the PVC locations of the persisted MITM CA.
// The certificate is intentionally in /data because agents need to trust it.
// The private key is kept in a dedicated owner-only directory: /data contains
// agent-readable state and must not be treated as a suitable secret directory.
// They are vars rather than consts solely so tests can redirect the CA
// read/write to a temporary directory; production never reassigns them.
var (
	CACertPath = "/data/proxy-ca.pem"
	caKeyPath  = "/data/.hive/proxy-ca-key.pem"
)

// GitHubProxy is an HTTP CONNECT proxy that performs MITM TLS
// inspection on GitHub API traffic and enforces ACMM mode rules.
type GitHubProxy struct {
	// localInferencePaths records CLI telemetry paths already logged once by
	// localInferenceResponse, so per-path DEBUG lines do not repeat.
	localInferencePaths sync.Map
	// timeoutWarnAt holds the last WARN time per timeout message, so bursts of
	// identical upstream timeouts emit one WARN per window (see logTimeout).
	timeoutWarnMu sync.Mutex
	timeoutWarnAt map[string]time.Time
	listenAddr    string
	caCert        tls.Certificate
	caX509        *x509.Certificate
	logger        *slog.Logger
	uidMap        *agent.UIDMap
	allowedRepos  map[string]bool

	// repoPaused reports whether a repo ("owner/repo", as ExtractRepo yields it)
	// is under an operator pause (#6203). Guarded because the dashboard can
	// pause a repo while request goroutines are reading it.
	//
	// Enforcement lives HERE rather than in an agent prompt on purpose: a
	// prompt-only pause has already been observed to fail when an agent's model
	// changes, and a run-state the operator relies on must not be advisory.
	pausedMu        sync.RWMutex
	repoPaused      func(repo string) bool
	autoMergeMu     sync.RWMutex
	repoAutoMergeOn func(repo string) bool
	// agentServesRepo reports whether an agent is scoped to a repo (#6204).
	// Guarded because the dashboard can re-scope an agent while request
	// goroutines are reading it.
	//
	// Enforcement lives HERE rather than in an agent prompt on purpose: an
	// out-of-scope agent must be REFUSED, not merely asked not to go. A
	// prompt-level scope has already been observed to fail when an agent's
	// model changes.
	scopeMu         sync.RWMutex
	agentServesRepo func(agent, repo string) bool

	// writeSurfaceEnforced reports whether an agent's lane is under
	// write_surface.enforce (#9772): its direct GitHub writes are refused so
	// the audited relays are its only write path. writeRefusedAudit records
	// each such refusal as agent_write_refused. Both are live and guarded
	// because config can change while request goroutines read them. Nil
	// enforced (the default) refuses nothing.
	writeSurfaceMu       sync.RWMutex
	writeSurfaceEnforced func(agent string) bool
	writeRefusedAudit    func(agent, kind, method, path, repo string)

	// proxyAdvisoryOK mirrors entrypoint.sh's HIVE_PROXY_ADVISORY_OK — the SAME
	// explicit, operator-set escape hatch that already governs whether a failed
	// forced-egress iptables redirect is fatal. Read once at construction (it
	// is a boot-time deployment choice, not something that changes mid-process).
	// See identifyAgentFromReq/identifyAgentFromConn (N7, #3841): it gates
	// whether the proxy may EVER trust the self-asserted Proxy-Authorization
	// header as an agent's identity.
	proxyAdvisoryOK bool

	// allowPrivateForward snapshots HIVE_PROXY_ALLOW_PRIVATE_FORWARD at
	// construction (a boot-time deployment choice like proxyAdvisoryOK). The
	// zero value keeps the plain-HTTP forward guard ON: non-routable
	// destinations (loopback, link-local/metadata, RFC 1918, CGNAT, multicast)
	// are refused on the plain path. See plain_forward_guard.go.
	allowPrivateForward bool

	// injectGHAuth snapshots config.ProxyInjectGHAuth() at construction (#1861,
	// opt-in, default off — a boot-time deployment choice like proxyAdvisoryOK).
	// When true, the proxy is the ONLY holder of usable GitHub credentials on
	// the request path: every MITM'd request has any agent-supplied
	// Authorization header STRIPPED and the identified agent's hub-held scoped
	// token injected in its place (see rewriteGitHubAuth). Agents hold only an
	// inert placeholder, so a prompt-injected agent has nothing worth
	// exfiltrating.
	injectGHAuth bool

	// agentTokenSource resolves an identified agent name to that agent's
	// hub-held tier-scoped token (github.AgentProxyToken, wired in main). It is
	// a func, not an import, to keep pkg/proxy decoupled from pkg/github's
	// minting machinery — the proxy consumes tokens the existing #3967 lane
	// already maintains; it never mints. May be nil (injection then never adds
	// a header, and stripped requests proceed unauthenticated — fail loud at
	// GitHub, never fall back to a shared token).
	agentTokenSource func(agentName string) (string, bool)

	mu         sync.RWMutex
	violations map[string]int // agent name -> blocked request count
	// blockedSeen holds the agent|method|path keys already logged at WARN so
	// a repeated refusal logs once and then at Debug. Bounded by maxViolationLog.
	blockedSeen map[string]struct{}

	writes writeCounters

	certMu    sync.RWMutex
	certCache map[string]cachedCert

	inference *inferenceRouter

	// entitlements caches the per-key entitled model set for LiteLLM
	// gateways (keyed by endpoint), learned from a key-info probe or a
	// "team not allowed" 403 body, so the dashboard can offer only usable
	// models.
	entitlements *entitlementStore

	// inferenceAuth tracks CONSECUTIVE inference-backend auth failures (a stale
	// gateway key returning 401 on every call) so the spoke can surface a hive
	// that is silently 401'ing as a health signal instead of merely looking
	// quiet. It latches only after inferenceAuthFailureThreshold failures and
	// clears on the next success (self-heal). Never nil after NewGitHubProxy.
	inferenceAuth *inferenceAuthState
	// inferenceLastErrors holds the most recent inference error text returned
	// to each agent so the dashboard can show it in full while Claude Code is
	// still retrying (#11761). Never nil after NewGitHubProxy.
	inferenceLastErrors *inferenceLastErrorState
	// inferenceBudget tracks PROVIDER spending-limit refusals (a LiteLLM key
	// past its daily dollar cap, a project out of quota). Distinct from
	// inferenceAuth (a rejected key) and from the hive's own token budget in
	// pkg/governor (denominated in tokens, blind to what the gateway charges).
	// See inference_budget.go (kubestellar/hive#4294).
	inferenceBudget *inferenceBudgetState
	// gatewayHealth tracks the current failing precondition per inference gateway.
	gatewayHealth *inferencehealth.Store

	// tokenSink records per-agent token usage for bare-mode inference
	// agents, whose usage the file-scanning token collector cannot see
	// otherwise. May be nil; the host installs it with SetTokenSink.
	tokenSink UsageSink

	// restAccounting receives one record per relayed GitHub REST response
	// and supplies the shared core-bucket snapshot; nil until the host
	// installs it with SetGitHubRESTAccounting.
	restAccounting GitHubRESTAccounting

	githubBudget          *agentGitHubBudget
	githubBudgetCap       func(agentName string) int
	githubReserveFloor    func() int
	githubReserveSnapshot func() (remaining int, reset time.Time, ok bool)
	githubBudgetNudge     func(agentName, message string)

	// copilotDial, when set, overrides how the Copilot-sniff MITM path dials the
	// upstream Copilot host (default: tls.Dial to host:443). It exists solely as
	// a test seam so the CONNECT handler can be driven against a local fake
	// upstream; production leaves it nil.
	copilotDial func(host string) (net.Conn, error)

	// tunnelDial, when set, overrides how a non-inspected transparent tunnel
	// dials its upstream (the raw TCP leg that carries the agent's ClientHello
	// verbatim). Tests substitute a pipe so the tunnel path is exercised
	// without network access.
	tunnelDial func(host string) (net.Conn, error)

	canariesEnabled  bool
	canaryFailClosed bool
	canaryRegistry   CanaryScanner
	canaryLeakFunc   func(crux.CanaryLeak)

	// linearCredential resolves the hive's CURRENT Linear write credential
	// for injection into agent requests to api.linear.app — see
	// injectLinearCredential. nil (the default) leaves agent requests
	// carrying whatever credential the agent supplied.
	linearCredential func() agent.LinearCredential

	// bodyStallTimeout, when > 0, overrides the package-level
	// responseBodyStallTimeout for this proxy instance. This test seam
	// allows tests to adjust the stall bound per-proxy without mutating shared state.
	bodyStallTimeout time.Duration
}

// NewGitHubProxy creates a proxy with a self-signed CA for MITM.
// The org and repos parameters define which repositories are allowed for
// write operations. Repos should be bare names (e.g. "console"); the org
// is prepended to form "org/repo" keys.
func NewGitHubProxy(logger *slog.Logger, org string, repos []string) (*GitHubProxy, error) {
	caCert, caX509, err := loadOrGenerateCA(logger)
	if err != nil {
		return nil, fmt.Errorf("CA setup: %w", err)
	}
	return newGitHubProxyWithCA(caCert, caX509, logger, org, repos), nil
}

// NewGitHubProxyEphemeral is NewGitHubProxy with a freshly generated CA that
// is never written to disk. It exists so cmd/hive boot tests can construct a
// real proxy (routes, providers, sinks) on a host without /data; nothing in
// production calls it.
func NewGitHubProxyEphemeral(logger *slog.Logger, org string, repos []string) (*GitHubProxy, error) {
	caCert, caX509, err := generateCA()
	if err != nil {
		return nil, fmt.Errorf("CA setup: %w", err)
	}
	return newGitHubProxyWithCA(caCert, caX509, logger, org, repos), nil
}

func newGitHubProxyWithCA(caCert tls.Certificate, caX509 *x509.Certificate, logger *slog.Logger, org string, repos []string) *GitHubProxy {
	var uidMap *agent.UIDMap
	if loaded, loadErr := agent.LoadUIDMap(agent.UIDMapPath); loadErr == nil {
		uidMap = loaded
		logger.Info("proxy loaded UID map", "agents", len(uidMap.Agents), "iptables", uidMap.IptablesActive)
	}

	// N7 (#3841): the same explicit opt-in entrypoint.sh already requires for a
	// degraded forced-egress gate. See proxyAdvisoryOK's doc comment.
	advisoryOK := strings.TrimSpace(os.Getenv("HIVE_PROXY_ADVISORY_OK")) == "true"
	if uidMap == nil {
		if advisoryOK {
			logger.Warn("proxy: no UID map — agent identity falls back to the self-asserted Proxy-Authorization header (HIVE_PROXY_ADVISORY_OK=true)")
		} else {
			logger.Warn("proxy: no UID map — agents will be treated as unidentified (ADVISORY, writes blocked) rather than trusting a self-asserted identity; set HIVE_PROXY_ADVISORY_OK=true to accept the header instead (N7, #3841)")
		}
	}

	allowed := make(map[string]bool, len(repos))
	for _, repo := range repos {
		key := org + "/" + repo
		allowed[key] = true
	}

	// #1861: snapshot the injection flag once, mirroring advisoryOK above.
	injectGHAuth := config.ProxyInjectGHAuth()
	if injectGHAuth {
		logger.Info("proxy GitHub credential injection enabled — agent-supplied Authorization headers are stripped and hub-held scoped tokens injected per identified agent (#1861)",
			"env", config.ProxyInjectGHAuthEnv)
	}

	// Plain-HTTP forward guard: on by default, lifted only by an explicit
	// operator opt-in, and announced when lifted so the log says the proxy may
	// dial the private estate on an agent's behalf.
	allowPrivateForward := plainForwardAllowPrivate()
	if allowPrivateForward {
		logger.Warn("proxy: plain-HTTP forwards to non-routable destinations are ALLOWED — agents can reach loopback, link-local (cloud metadata) and private-network services through this proxy",
			"env", plainForwardAllowPrivateEnv)
	}

	p := &GitHubProxy{
		listenAddr:          fmt.Sprintf("127.0.0.1:%d", proxyListenPort),
		caCert:              caCert,
		caX509:              caX509,
		logger:              logger,
		uidMap:              uidMap,
		proxyAdvisoryOK:     advisoryOK,
		allowPrivateForward: allowPrivateForward,
		injectGHAuth:        injectGHAuth,
		allowedRepos:        allowed,
		violations:          make(map[string]int),
		certCache:           make(map[string]cachedCert),
		inference:           newInferenceRouter(),
		entitlements:        newEntitlementStore(),
		inferenceAuth:       &inferenceAuthState{},
		inferenceLastErrors: newInferenceLastErrorState(),
		inferenceBudget:     &inferenceBudgetState{},
		gatewayHealth:       inferencehealth.NewStore(),
		githubBudget:        newAgentGitHubBudget(time.Now),
		githubBudgetCap:     func(string) int { return config.DefaultAgentsGitHubAPIHourlyCap },
		githubReserveFloor:  func() int { return config.DefaultGitHubAgentReserveFloor },
	}

	// Pre-warm cert cache for known GitHub hosts to avoid startup burst
	for host := range githubHosts {
		if cert, err := p.forgeCert(host); err == nil {
			_ = cert
			logger.Info("pre-warmed cert cache", "host", host)
		}
	}

	return p
}

// ListenAddr returns the proxy listen address.
func (p *GitHubProxy) ListenAddr() string { return p.listenAddr }

func newBufferedConn(c net.Conn) *bufio.Reader {
	return bufio.NewReader(c)
}

func newBufferedReader(c net.Conn) *bufio.Reader {
	return bufio.NewReader(c)
}

func (p *GitHubProxy) writeHTTPError(conn net.Conn, status int, msg string) {
	resp := &http.Response{
		StatusCode: status,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"%s"}}`, msg))),
	}
	_ = resp.Write(conn)
}

func truncateBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// jsonEscape escapes a string for safe interpolation into a JSON string
// literal (upstream error bodies can contain quotes/backslashes/newlines that
// would otherwise corrupt the hand-built error envelope).
func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	// Marshal wraps the value in quotes; strip them since the caller supplies
	// its own surrounding quotes in the format string.
	return string(b[1 : len(b)-1])
}

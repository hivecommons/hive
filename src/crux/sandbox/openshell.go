package sandbox

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	openshell "github.com/ai-openshell/sdk-go/openshell/v1"
)

// OpenShell runtime (ADR-0026 phase 1, hivecommons/hive#14092).
//
// OpenShellLauncher implements Launcher on top of an OpenShell gateway: it
// creates a kernel-confined sandbox from the kick spec, runs the kick command
// in it, collects stdout/stderr/exit, and deletes the sandbox. The gateway,
// not hive, owns the pod, the Landlock/seccomp confinement and the egress
// proxy; hive's part is to hand it a spec and a network policy and to keep
// the same cleanup and timeout semantics the Job launcher has.
//
// Workspace transport is a shared PVC mounted through the gateway's
// Kubernetes driver config, exactly like kubejob: the SDK's file transport
// is single-file and optional on the gateway, so copy in/out was not viable.

const (
	// OpenShellDefaultWorkspace is the OpenShell workspace (their term; not
	// a Kubernetes namespace) sandboxes land in when none is configured.
	OpenShellDefaultWorkspace = "default"
	// OpenShellDefaultWorkspaceClaimMount is where hive sees the workspace
	// claim; kubejob uses the same default.
	OpenShellDefaultWorkspaceClaimMount = "/data"
	// OpenShellWorkspaceMount is where the sandbox sees the per-kick
	// workspace, matching the Podman and Job launchers.
	OpenShellWorkspaceMount = "/workspace"
	// OpenShellPolicyVersion is the policy schema version the SDK expects.
	OpenShellPolicyVersion = 1
	// OpenShellExitCodeUnknown is reported when the sandbox ended without a
	// readable exit code (gateway lost, kick timeout). Same value as
	// kubejob.ExitCodeUnknown so callers treat both the same way.
	OpenShellExitCodeUnknown = -1

	// OpenShellPolicyProfileAnnotation carries sandbox.openshell.policy_profile
	// on the created sandbox.
	OpenShellPolicyProfileAnnotation = "hive.dev/policy-profile"

	// openshellNamePrefix and openshellNameMax follow the gateway's
	// MAX_ROUTABLE_NAME_LEN (19): a sandbox name is one of three segments
	// in a 63-char DNS label (<workspace>--<sandbox>--<service>), so the
	// prefix is short and the timestamp is base36 (measured on 0.1.8,
	// hivecommons/hive#17643).
	openshellNamePrefix   = "hk-"
	openshellNameMax      = 19
	openshellNameUnixBase = 36
	openshellNameTagLen   = 3
	openshellNameTagMod   = 36 * 36 * 36
	openshellRequestWait  = 30 * time.Second
	openshellReadyPoll    = 2 * time.Second
	openshellMaxOutput    = 16 << 20
	openshellManagedLabel = "app.kubernetes.io/managed-by"
	openshellManagedValue = "hive-openshell"
	// openshellWorkspaceVolume names hive's claim in driver_config; the
	// gateway reserves the bare name "workspace" for its own volumes.
	openshellWorkspaceVolume = "hive-workspace"
	openshellHTTPSPort       = 443
)

// OpenShellAlwaysAllowedHosts are the egress hosts every hive sandbox gets:
// the same GitHub hosts hive's MITM proxy (ADR-0002) brokers for a Podman
// sandbox. github.com is what `git fetch` talks to; api.github.com is
// restricted to read-only methods because the sandbox holds no token and
// hive does every write itself after the kick.
var OpenShellAlwaysAllowedHosts = []string{"github.com", "api.github.com"}

// OpenShellDefaultBinaries are the in-image executables the egress policy
// is attached to when the operator names none. OpenShell matches network
// rules per real binary path, and a rule with no binaries allows nothing;
// these cover git, curl, node-based CLIs and the Python-based ones in the
// sandbox images hive ships.
var OpenShellDefaultBinaries = []string{
	"/usr/bin/git",
	"/usr/bin/git-remote-https",
	"/usr/lib/git-core/git-remote-https",
	"/usr/bin/curl",
	"/usr/bin/node",
	"/usr/local/bin/node",
	"/usr/bin/python3",
	"/usr/local/bin/python3",
	"/usr/local/bin/crux",
}

// OpenShellOptions is the launcher's configuration; crux/agent builds it
// from config.SandboxOpenShellConfig.
type OpenShellOptions struct {
	GatewayURL          string
	Workspace           string
	PolicyProfile       string
	Token               string
	InsecureTLS         bool
	Timeout             time.Duration
	WorkspaceClaim      string
	WorkspaceClaimMount string
	AllowHosts          []string
	Binaries            []string
	RuntimeClassName    string
	NodeSelector        map[string]string
	Tolerations         []OpenShellToleration
}

// OpenShellToleration mirrors the subset of a Kubernetes toleration an
// operator writes by hand.
type OpenShellToleration struct {
	Key, Operator, Value, Effect string
}

// Validate reports the first configuration error a kick would hit.
func (o OpenShellOptions) Validate() error {
	if strings.TrimSpace(o.GatewayURL) == "" {
		return errors.New("openshell: gateway_url is required")
	}
	if _, _, err := openshellAddress(o.GatewayURL); err != nil {
		return err
	}
	if strings.TrimSpace(o.WorkspaceClaim) == "" {
		return errors.New("openshell: workspace_claim is required — the PVC hive's sandbox workspace root lives on, labelled openshell.ai/sandbox-attachable so the gateway may mount it")
	}
	for _, h := range o.AllowHosts {
		if _, _, err := openshellHostPort(h); err != nil {
			return err
		}
	}
	return nil
}

func (o OpenShellOptions) workspace() string {
	if ws := strings.TrimSpace(o.Workspace); ws != "" {
		return ws
	}
	return OpenShellDefaultWorkspace
}

// openshellSandboxes and openshellExec are the slices of the SDK the
// launcher uses. They exist so tests can drive the launcher with a fake and
// so an SDK method rename shows up here, not in every call site.
type openshellSandboxes interface {
	Create(ctx context.Context, workspace, name string, spec *openshell.SandboxSpec, opts ...openshell.CreateOption) (*openshell.Sandbox, error)
	WaitReady(ctx context.Context, workspace, name string, opts ...openshell.WaitOptions) (*openshell.Sandbox, error)
	Delete(ctx context.Context, workspace, name string, opts ...openshell.DeleteOptions) (*openshell.DeletionResult, error)
}

type openshellExec interface {
	Run(ctx context.Context, workspace, sandboxName string, command []string, opts ...openshell.ExecOptions) (*openshell.ExecResult, error)
}

type openshellHealth interface {
	Check(ctx context.Context) (*openshell.HealthResult, error)
}

type openshellConfig interface {
	GetGateway(ctx context.Context) (*openshell.GatewayConfig, error)
}

type openshellClient interface {
	Sandboxes() openshellSandboxes
	Exec() openshellExec
	Health() openshellHealth
	Config() openshellConfig
	Close() error
}

// sdkClient adapts *openshell.Client to the narrow interfaces above.
type sdkClient struct{ c *openshell.Client }

func (s sdkClient) Sandboxes() openshellSandboxes { return s.c.Sandboxes() }
func (s sdkClient) Exec() openshellExec           { return s.c.Exec() }
func (s sdkClient) Health() openshellHealth       { return s.c.Health() }
func (s sdkClient) Config() openshellConfig       { return s.c.Config() }
func (s sdkClient) Close() error                  { return s.c.Close() }

// OpenShellLauncher runs sandboxed kicks through an OpenShell gateway.
type OpenShellLauncher struct {
	Options OpenShellOptions

	// newClient overrides the SDK client constructor in tests.
	newClient func(OpenShellOptions) (openshellClient, error)
	// now overrides the clock in tests.
	now func() time.Time
}

func (l *OpenShellLauncher) client() (openshellClient, error) {
	if l.newClient != nil {
		return l.newClient(l.Options)
	}
	return newOpenShellSDKClient(l.Options)
}

func newOpenShellSDKClient(o OpenShellOptions) (openshellClient, error) {
	addr, tls, err := openshellAddress(o.GatewayURL)
	if err != nil {
		return nil, err
	}
	cfg := openshell.Config{Address: addr, Timeout: openshellRequestWait}
	if tls || o.InsecureTLS {
		cfg.TLS = &openshell.TLSConfig{Insecure: o.InsecureTLS}
	}
	if o.Token != "" {
		cfg.Auth = openshell.StaticToken(o.Token)
	}
	c, err := openshell.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("openshell: connecting to gateway %s: %w", addr, err)
	}
	return sdkClient{c: c}, nil
}

// Run implements Launcher.
func (l *OpenShellLauncher) Run(ctx context.Context, spec LaunchSpec) (Result, error) {
	res := Result{ExitCode: OpenShellExitCodeUnknown}
	if spec.ClineProvider != nil {
		_ = spec.ClineProvider.Close()
		return res, errors.New("openshell: scoped Cline provider files are Podman-only; the OpenShell runtime takes no credential files")
	}
	if err := l.Options.Validate(); err != nil {
		return res, err
	}
	if strings.TrimSpace(spec.Image) == "" {
		return res, errors.New("openshell: image is required")
	}
	if strings.TrimSpace(spec.Workspace) == "" {
		return res, errors.New("openshell: workspace is required")
	}
	if len(spec.Command) == 0 {
		return res, errors.New("openshell: command is required")
	}
	subPath, err := openshellWorkspaceSubPath(spec.Workspace, l.Options.WorkspaceClaimMount)
	if err != nil {
		return res, err
	}
	if l.Options.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.Options.Timeout)
		defer cancel()
	}

	client, err := l.client()
	if err != nil {
		return res, err
	}
	defer func() { _ = client.Close() }()

	ws := l.Options.workspace()
	now := time.Now
	if l.now != nil {
		now = l.now
	}
	name := openshellSandboxName(spec.Name, now())
	sbSpec, err := l.sandboxSpec(spec, subPath)
	if err != nil {
		return res, err
	}
	createOpts := []openshell.CreateOption{openshell.WithLabels(map[string]string{openshellManagedLabel: openshellManagedValue})}
	if p := strings.TrimSpace(l.Options.PolicyProfile); p != "" {
		// The SDK (v0.1.8) has no API to select a gateway-side policy profile
		// at create time; the name is recorded so gateway admission or an
		// operator can act on it, and so the intent is visible on the sandbox.
		createOpts = append(createOpts, openshell.WithAnnotations(map[string]string{OpenShellPolicyProfileAnnotation: p}))
	}
	if _, err := client.Sandboxes().Create(ctx, ws, name, sbSpec, createOpts...); err != nil {
		return res, fmt.Errorf("openshell: creating sandbox %s: %w", name, err)
	}

	// The caller's ctx may be dead already (kick timeout) or die between any
	// two of these requests; cleanup falls back to a fresh, request-bounded
	// context so a finished sandbox is still removed.
	var cancels []context.CancelFunc
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	live := func() context.Context {
		if ctx.Err() == nil {
			return ctx
		}
		fresh, cancel := context.WithTimeout(context.Background(), openshellRequestWait)
		cancels = append(cancels, cancel)
		return fresh
	}
	cleanup := func() {
		_, _ = client.Sandboxes().Delete(live(), ws, name, openshell.DeleteOptions{AllowMissing: true})
	}

	if _, err := client.Sandboxes().WaitReady(ctx, ws, name, openshell.WaitOptions{PollInterval: openshellReadyPoll}); err != nil {
		cleanup()
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, fmt.Errorf("openshell: sandbox %s never became ready: %w", name, err)
	}

	execOpts := openshell.ExecOptions{Env: l.execEnv(spec), WorkDir: spec.WorkDir, NoLoginShell: true}
	if execOpts.WorkDir == "" {
		execOpts.WorkDir = OpenShellWorkspaceMount
	}
	out, runErr := client.Exec().Run(ctx, ws, name, spec.Command, execOpts)
	// The SDK's Run is synchronous; a result is the first evidence that the
	// gateway accepted the command rather than merely creating the sandbox.
	if out != nil && spec.OnStarted != nil {
		spec.OnStarted()
	}
	if out != nil {
		res.Stdout = openshellCap(out.Stdout)
		res.Stderr = openshellCap(out.Stderr)
		res.ExitCode = out.ExitCode
	}
	cleanup()
	if runErr != nil {
		if ctx.Err() != nil {
			// Kick timeout or cancel: the sandbox has been deleted above so
			// the workload does not keep running on the cluster.
			return res, ctx.Err()
		}
		return res, fmt.Errorf("openshell: running kick in sandbox %s: %w", name, runErr)
	}
	return res, nil
}

// sandboxSpec maps the hive LaunchSpec onto the SDK's SandboxSpec.
func (l *OpenShellLauncher) sandboxSpec(spec LaunchSpec, subPath string) (*openshell.SandboxSpec, error) {
	policy, err := OpenShellNetworkPolicy(l.Options.AllowHosts, l.Options.Binaries)
	if err != nil {
		return nil, err
	}
	mount := spec.WorkspaceMount
	if mount == "" {
		mount = OpenShellWorkspaceMount
	}
	if spec.NetworkNone {
		policy.NetworkPolicies = map[string]openshell.NetworkPolicyRule{}
	}
	tpl := &openshell.SandboxTemplate{
		Image:        spec.Image,
		Labels:       map[string]string{openshellManagedLabel: openshellManagedValue},
		Environment:  l.execEnv(spec),
		DriverConfig: l.driverConfig(subPath, mount, spec.ReadOnly),
	}
	if l.Options.RuntimeClassName != "" {
		tpl.RuntimeClassName = l.Options.RuntimeClassName
	}
	return &openshell.SandboxSpec{Template: tpl, Policy: policy}, nil
}

// execEnv is the environment the kick runs with: the allowlisted,
// credential-filtered subset of hive's own, plus the spec's explicit values —
// the same rule the Podman and Job launchers apply.
func (l *OpenShellLauncher) execEnv(spec LaunchSpec) map[string]string {
	env := map[string]string{}
	for _, entry := range SanitizedEnv(os.Environ(), spec.EnvAllowlist, spec.Env) {
		if k, v, ok := strings.Cut(entry, "="); ok {
			env[k] = v
		}
	}
	return env
}

// driverConfig is the gateway's Kubernetes driver passthrough: the shared
// workspace claim at a subPath, plus scheduling. The gateway only honours
// it when its `allow_driver_config` setting is on and the claim carries the
// `openshell.ai/sandbox-attachable` labels; that is a documented
// prerequisite rather than something hive can verify from here.
func (l *OpenShellLauncher) driverConfig(subPath, mount string, readOnly bool) map[string]any {
	k8s := map[string]any{
		"volumes": []any{map[string]any{
			"name":                    openshellWorkspaceVolume,
			"persistent_volume_claim": map[string]any{"claim_name": l.Options.WorkspaceClaim, "read_only": false},
		}},
		"containers": map[string]any{"agent": map[string]any{
			"volume_mounts": []any{map[string]any{
				"name":       openshellWorkspaceVolume,
				"mount_path": mount,
				"sub_path":   subPath,
				"read_only":  readOnly,
			}},
		}},
	}
	pod := map[string]any{}
	if len(l.Options.NodeSelector) > 0 {
		pod["node_selector"] = l.Options.NodeSelector
	}
	if len(l.Options.Tolerations) > 0 {
		tols := make([]any, 0, len(l.Options.Tolerations))
		for _, t := range l.Options.Tolerations {
			tol := map[string]any{}
			for k, v := range map[string]string{"key": t.Key, "operator": t.Operator, "value": t.Value, "effect": t.Effect} {
				if v != "" {
					tol[k] = v
				}
			}
			tols = append(tols, tol)
		}
		pod["tolerations"] = tols
	}
	if l.Options.RuntimeClassName != "" {
		pod["runtime_class_name"] = l.Options.RuntimeClassName
	}
	if len(pod) > 0 {
		k8s["pod"] = pod
	}
	return map[string]any{"kubernetes": k8s}
}

// OpenShellNetworkPolicy derives the sandbox's egress policy from hive's
// always-allowed GitHub hosts plus the operator's extra hosts. github.com
// gets read-write TLS passthrough (git over HTTPS needs POST for
// fetch-pack even when it only reads); api.github.com is read-only so a
// sandbox that somehow obtained a token still could not mutate; extra hosts
// (model gateways, the hub) are read-write. Every rule is attached to the
// same binaries, because OpenShell allows nothing for an unlisted binary.
func OpenShellNetworkPolicy(allowHosts, binaries []string) (*openshell.SandboxPolicy, error) {
	if len(binaries) == 0 {
		binaries = OpenShellDefaultBinaries
	}
	bins := make([]openshell.PolicyNetworkBinary, 0, len(binaries))
	for _, b := range binaries {
		if b = strings.TrimSpace(b); b != "" {
			bins = append(bins, openshell.PolicyNetworkBinary{Path: b})
		}
	}
	rules := map[string]openshell.NetworkPolicyRule{
		"hive-git": {
			Name: "hive-git",
			Endpoints: []openshell.PolicyNetworkEndpoint{{
				Host: "github.com", Port: openshellHTTPSPort,
				Access:      openshell.NetworkAccessPresetReadWrite,
				Enforcement: openshell.NetworkEnforcementModeEnforce,
			}},
			Binaries: bins,
		},
		"hive-github-api": {
			Name: "hive-github-api",
			Endpoints: []openshell.PolicyNetworkEndpoint{{
				Host: "api.github.com", Port: openshellHTTPSPort,
				Access:      openshell.NetworkAccessPresetReadOnly,
				Enforcement: openshell.NetworkEnforcementModeEnforce,
			}},
			Binaries: bins,
		},
	}
	seen := map[string]bool{"github.com": true, "api.github.com": true}
	var extra []openshell.PolicyNetworkEndpoint
	for _, h := range allowHosts {
		host, port, err := openshellHostPort(h)
		if err != nil {
			return nil, err
		}
		key := host + ":" + strconv.Itoa(int(port))
		if seen[host] || seen[key] {
			continue
		}
		seen[key] = true
		extra = append(extra, openshell.PolicyNetworkEndpoint{
			Host: host, Port: port,
			Access:      openshell.NetworkAccessPresetReadWrite,
			Enforcement: openshell.NetworkEnforcementModeEnforce,
		})
	}
	if len(extra) > 0 {
		sort.Slice(extra, func(i, j int) bool {
			if extra[i].Host != extra[j].Host {
				return extra[i].Host < extra[j].Host
			}
			return extra[i].Port < extra[j].Port
		})
		rules["hive-allow-hosts"] = openshell.NetworkPolicyRule{Name: "hive-allow-hosts", Endpoints: extra, Binaries: bins}
	}
	return &openshell.SandboxPolicy{
		Version: OpenShellPolicyVersion,
		Filesystem: &openshell.FilesystemPolicy{
			IncludeWorkdir: true,
			ReadWrite:      []string{OpenShellWorkspaceMount, "/tmp"},
		},
		NetworkPolicies: rules,
	}, nil
}

// openshellAddress turns gateway_url into the SDK's host:port address and
// whether TLS is implied. Accepts `host:port`, `http://host:port` and
// `https://host[:port]`.
func openshellAddress(raw string) (addr string, tls bool, err error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		if _, _, err := openshellHostPort(raw); err != nil || !strings.Contains(raw, ":") {
			return "", false, fmt.Errorf("openshell: gateway_url %q must be host:port or an http(s) URL", raw)
		}
		return raw, false, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("openshell: gateway_url %q is not a valid URL", raw)
	}
	switch u.Scheme {
	case "https", "grpcs":
		tls = true
	case "http", "grpc":
	default:
		return "", false, fmt.Errorf("openshell: gateway_url %q has unsupported scheme %q", raw, u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		if tls {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	return host, tls, nil
}

// openshellHostName is a literal DNS name or IPv4 address: no wildcard,
// userinfo, query, fragment or second colon can ride along in a host entry.
var openshellHostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// openshellHostPort parses an allow_hosts entry: `host` means 443.
func openshellHostPort(raw string) (string, uint32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, errors.New("openshell: allow_hosts entry is empty")
	}
	if strings.Contains(raw, "://") || strings.ContainsAny(raw, "/ ") {
		return "", 0, fmt.Errorf("openshell: allow_hosts entry %q must be host or host:port, not a URL", raw)
	}
	host, portStr, found := strings.Cut(raw, ":")
	if !openshellHostName.MatchString(host) {
		return "", 0, fmt.Errorf("openshell: allow_hosts entry %q must be a literal host name — no wildcards, userinfo, query or fragment", raw)
	}
	if !found {
		return host, openshellHTTPSPort, nil
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 || host == "" {
		return "", 0, fmt.Errorf("openshell: allow_hosts entry %q has an invalid port", raw)
	}
	return host, uint32(port), nil
}

// openshellWorkspaceSubPath turns hive's absolute workspace path into the
// path relative to the claim mount, which is what the volume mount's
// sub_path needs. Same rule as kubejob.
func openshellWorkspaceSubPath(workspace, claimMount string) (string, error) {
	if claimMount == "" {
		claimMount = OpenShellDefaultWorkspaceClaimMount
	}
	rel, err := filepath.Rel(filepath.Clean(claimMount), filepath.Clean(workspace))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("openshell: workspace %q is not under the workspace claim mount %q — set agent_sandbox.workspace_dir under it, or openshell.workspace_claim_mount to where the claim is mounted", workspace, claimMount)
	}
	return filepath.ToSlash(rel), nil
}

var openshellNameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// openshellSandboxName builds a unique, DNS-label-safe sandbox name from the
// launch name and a timestamp: hk-<agent>-<unix in base36>, at most
// openshellNameMax characters. The launch name's hive-/-sandbox wrapping is
// dropped so the agent slug gets the room; a slug that still does not fit is
// cut and tagged with a short hash of the whole slug, so two agents sharing
// a long prefix never map to the same sandbox name.
func openshellSandboxName(launchName string, now time.Time) string {
	base := strings.ToLower(strings.TrimSuffix(launchName, launchNameSuffix))
	base = strings.TrimPrefix(base, launchNamePrefix)
	base = strings.Trim(openshellNameUnsafe.ReplaceAllString(base, "-"), "-")
	if base == "" {
		base = "agent"
	}
	suffix := "-" + strconv.FormatInt(now.Unix(), openshellNameUnixBase)
	room := openshellNameMax - len(openshellNamePrefix) - len(suffix)
	if len(base) > room {
		h := fnv.New32a()
		_, _ = h.Write([]byte(base))
		tag := strconv.FormatUint(uint64(h.Sum32())%openshellNameTagMod, openshellNameUnixBase)
		for len(tag) < openshellNameTagLen {
			tag = "0" + tag
		}
		base = strings.TrimRight(base[:room-openshellNameTagLen], "-") + tag
	}
	return openshellNamePrefix + base + suffix
}

func openshellCap(b []byte) string {
	if len(b) > openshellMaxOutput {
		b = b[:openshellMaxOutput]
	}
	return string(b)
}

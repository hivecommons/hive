package sandbox

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
)

// SubstrateDefaultBridgeCommand is the actor entrypoint when the operator
// names none: the bridge subcommand of the crux binary every hive sandbox
// image ships (ADR-0026 phase 2).
var SubstrateDefaultBridgeCommand = []string{"/usr/local/bin/crux", "substrate-bridge"}

// SubstrateAlwaysAllowedHosts are the egress hosts every hive actor gets:
// the same GitHub hosts hive's MITM proxy (ADR-0002) brokers for a Podman
// sandbox and the OpenShell policy allows. Substrate's atenet-egress has no
// per-method read-only preset, so api.github.com is allowed as TLS
// passthrough like github.com; the actor still holds no token, and hive does
// every write itself after the kick.
var SubstrateAlwaysAllowedHosts = []string{"github.com", "api.github.com"}

// substrateImageDigest accepts only digest-pinned references.
var substrateImageDigest = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// actorTemplate maps the hive LaunchSpec onto a per-kick ActorTemplate: one
// container running the bridge, the workspace as a durableDir, golden
// snapshots off (a template used once has nothing to warm), and the
// operator's resource limits and pool selector.
func (l *SubstrateLauncher) actorTemplate(ref *substrateapi.ObjectRef, image string) *substrateapi.ActorTemplate {
	command := l.Options.Command
	if len(command) == 0 {
		command = SubstrateDefaultBridgeCommand
	}
	cpu, mem := l.Options.CPUMillicores, l.Options.MemoryBytes
	if cpu == 0 {
		cpu = SubstrateDefaultCPUMillicores
	}
	if mem == 0 {
		mem = SubstrateDefaultMemoryBytes
	}
	env := []*substrateapi.EnvVar{{Name: "HIVE_SUBSTRATE_BRIDGE_PORT", Value: strconv.Itoa(l.Options.bridgePort())}}
	if p := strings.TrimSpace(l.Options.PolicyProfile); p != "" {
		// Substrate has no annotations; the profile travels as an env var the
		// bridge and in-actor tooling can read, and as a selector label an
		// operator can pin a pool on.
		env = append(env, &substrateapi.EnvVar{Name: "HIVE_POLICY_PROFILE", Value: p})
	}
	return &substrateapi.ActorTemplate{
		Metadata:       &substrateapi.ResourceMetadata{Atespace: ref.Atespace, Name: ref.Name},
		WorkerSelector: substrateSelector(l.Options.WorkerSelector),
		Containers: []*substrateapi.Container{{
			Name:         substrateContainer,
			Image:        image,
			Command:      command,
			Env:          env,
			VolumeMounts: []*substrateapi.VolumeMount{{Name: substrateVolume, MountPath: SubstrateWorkspaceMount}},
		}},
		Volumes:        []*substrateapi.Volume{{Name: substrateVolume, DurableDir: &substrateapi.DurableDirVolumeSource{}}},
		SnapshotConfig: &substrateapi.SnapshotConfig{GoldenSnapshotConfig: &substrateapi.GoldenSnapshotConfig{Mode: substrateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}},
		Resources:      &substrateapi.ResourceRequirements{Limits: &substrateapi.Resources{CpuMillicores: cpu, MemoryBytes: mem}},
	}
}

func substrateSelector(labels map[string]string) *substrateapi.Selector {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return &substrateapi.Selector{MatchLabels: out}
}

// substrateEnv is the environment the kick runs with: the allowlisted,
// credential-filtered subset of hive's own, plus the spec's explicit values —
// the same rule the Podman, Job and OpenShell launchers apply.
func substrateEnv(spec LaunchSpec) map[string]string {
	env := map[string]string{}
	for _, entry := range SanitizedEnv(os.Environ(), spec.EnvAllowlist, spec.Env) {
		if k, v, ok := strings.Cut(entry, "="); ok {
			env[k] = v
		}
	}
	return env
}

// SubstrateEgressPolicy derives the actor's egress policy from hive's
// always-allowed GitHub hosts plus the operator's extra hosts, all as TLS
// passthrough on their port: the actor talks TLS end to end to the same hosts
// hive's MITM proxy would broker for a Podman sandbox, and atenet-egress
// default-denies everything else (including all UDP). Hosts are sorted so the
// policy hive sends is stable across kicks.
func SubstrateEgressPolicy(allowHosts []string) (*substrateapi.EgressPolicy, error) {
	byPort := map[int32][]string{}
	seen := map[string]bool{}
	add := func(host string, port int32) {
		key := host + ":" + strconv.Itoa(int(port))
		if seen[key] {
			return
		}
		seen[key] = true
		byPort[port] = append(byPort[port], host)
	}
	for _, h := range SubstrateAlwaysAllowedHosts {
		add(h, substrateHTTPSPort)
	}
	for _, h := range allowHosts {
		host, port, err := substrateHostPort(h)
		if err != nil {
			return nil, err
		}
		add(host, int32(port))
	}
	ports := make([]int32, 0, len(byPort))
	for p := range byPort {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	rules := make([]*substrateapi.EgressRule, 0, len(ports))
	for _, p := range ports {
		hosts := byPort[p]
		sort.Strings(hosts)
		rules = append(rules, &substrateapi.EgressRule{TlsPassthrough: &substrateapi.TlsPassthroughRule{
			Hostnames: hosts,
			Ports:     &substrateapi.Ports{Numbers: []int32{p}},
		}})
	}
	return &substrateapi.EgressPolicy{
		Metadata: &substrateapi.ResourceMetadata{Name: substrateEgressPolicy},
		Rules:    rules,
	}, nil
}

// substrateAddress turns endpoint into a gRPC host:port target and whether
// TLS is implied. Accepts `host:port`, `http://host:port`, `https://host[:port]`
// and the grpc(s) spellings; a bare host:port is TLS, which is what the
// Control API serves.
func substrateAddress(raw string) (addr string, tls bool, err error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		if _, _, err := substrateHostPort(raw); err != nil || !strings.Contains(raw, ":") {
			return "", false, fmt.Errorf("substrate: endpoint %q must be host:port or an http(s) URL", raw)
		}
		return raw, true, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("substrate: endpoint %q is not a valid URL", raw)
	}
	switch u.Scheme {
	case "https", "grpcs":
		tls = true
	case "http", "grpc":
	default:
		return "", false, fmt.Errorf("substrate: endpoint %q has unsupported scheme %q", raw, u.Scheme)
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

// substrateRouterAddress validates router_url and returns its host:port; the
// router serves TLS only, so https is the one accepted scheme.
func substrateRouterAddress(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return "", fmt.Errorf("substrate: router_url %q must be an https URL (atenet-router serves TLS only)", raw)
	}
	host := u.Host
	if u.Port() == "" {
		host += ":443"
	}
	return host, nil
}

// substrateHostName is a literal DNS name or a leftmost-wildcard DNS name,
// the two shapes Substrate's egress hostnames accept.
var substrateHostName = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// substrateHostPort parses an allow_hosts entry: `host` means 443.
func substrateHostPort(raw string) (string, uint32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, errors.New("substrate: allow_hosts entry is empty")
	}
	if strings.Contains(raw, "://") || strings.ContainsAny(raw, "/ ") {
		return "", 0, fmt.Errorf("substrate: allow_hosts entry %q must be host or host:port, not a URL", raw)
	}
	host, portStr, found := strings.Cut(raw, ":")
	if !substrateHostName.MatchString(host) {
		return "", 0, fmt.Errorf("substrate: allow_hosts entry %q must be a DNS name with at most a leading *. wildcard — no userinfo, query or fragment", raw)
	}
	host = strings.ToLower(host)
	if !found {
		return host, substrateHTTPSPort, nil
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return "", 0, fmt.Errorf("substrate: allow_hosts entry %q has an invalid port", raw)
	}
	return host, uint32(port), nil
}

var substrateNameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// substrateActorName builds a unique, DNS-label-safe actor (and template)
// name from the launch name and a timestamp: hive-kick-<agent>-<unix>.
func substrateActorName(launchName string, now time.Time) string {
	base := strings.Trim(substrateNameUnsafe.ReplaceAllString(strings.ToLower(strings.TrimSuffix(launchName, "-sandbox")), "-"), "-")
	if base == "" {
		base = "agent"
	}
	suffix := fmt.Sprintf("-%d", now.Unix())
	room := substrateNameMax - len(substrateNamePrefix) - len(suffix)
	if len(base) > room {
		base = strings.TrimRight(base[:room], "-")
	}
	return substrateNamePrefix + base + suffix
}

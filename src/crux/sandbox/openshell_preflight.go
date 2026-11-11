package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	openshell "github.com/ai-openshell/sdk-go/openshell/v1"
)

// OpenShell cluster-prerequisite preflight (ADR-0026 phase 4,
// hivecommons/hive#17644). Hive cannot see the node kernel from its pod, but
// the gateway reports whether it is up and which global settings it runs
// with; the three that decide whether a kick can work or is safe are checked
// here, read-only, and surfaced as boot/reload warnings.

const (
	openshellPreflightTTL = 5 * time.Minute

	openshellSettingAllowDriverConfig = "allow_driver_config"
	openshellSettingTelemetry         = "telemetry"
)

type openshellPreflightEntry struct {
	at       time.Time
	warnings []string
}

// OpenShellPreflighter checks gateways and remembers the answer for a TTL, so
// the dashboard polling the same warnings does not hit the gateway per call.
type OpenShellPreflighter struct {
	mu    sync.Mutex
	cache map[string]openshellPreflightEntry

	newClient func(OpenShellOptions) (openshellClient, error)
	now       func() time.Time
}

// DefaultOpenShellPreflighter backs the warnings AgentSandboxGateWarnings emits.
var DefaultOpenShellPreflighter = &OpenShellPreflighter{}

// Check returns the warnings for one gateway. Never an error: an unreachable
// gateway is itself a warning.
func (p *OpenShellPreflighter) Check(ctx context.Context, opts OpenShellOptions) []string {
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	key := opts.GatewayURL + "\x00" + opts.Token
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.cache[key]; ok && now().Sub(e.at) < openshellPreflightTTL {
		return append([]string(nil), e.warnings...)
	}
	warnings := p.probe(ctx, opts)
	if p.cache == nil {
		p.cache = map[string]openshellPreflightEntry{}
	}
	p.cache[key] = openshellPreflightEntry{at: now(), warnings: warnings}
	return append([]string(nil), warnings...)
}

func (p *OpenShellPreflighter) probe(ctx context.Context, opts OpenShellOptions) []string {
	gw := opts.GatewayURL
	ctx, cancel := context.WithTimeout(ctx, openshellRequestWait)
	defer cancel()

	newClient := newOpenShellSDKClient
	if p.newClient != nil {
		newClient = p.newClient
	}
	client, err := newClient(opts)
	if err != nil {
		return []string{fmt.Sprintf("openshell gateway %s is unreachable (%v); every kick on the openshell runtime will fail", gw, err)}
	}
	defer func() { _ = client.Close() }()

	health, err := client.Health().Check(ctx)
	if err != nil {
		return []string{fmt.Sprintf("openshell gateway %s is unreachable (%v); every kick on the openshell runtime will fail", gw, err)}
	}
	var out []string
	if health == nil || !health.Healthy {
		out = append(out, fmt.Sprintf("openshell gateway %s reports itself unhealthy; kicks on the openshell runtime may fail", gw))
	}
	gwCfg, err := client.Config().GetGateway(ctx)
	if err != nil || gwCfg == nil {
		return append(out, fmt.Sprintf("openshell gateway %s settings could not be read (%v); allow_driver_config and telemetry are unverified", gw, err))
	}
	if v, ok := gwCfg.Settings[openshellSettingAllowDriverConfig]; !ok || !v.BoolVal {
		out = append(out, fmt.Sprintf("openshell gateway %s does not have allow_driver_config enabled; it will ignore the workspace volume mount and every kick will run without the workspace — set allow_driver_config = true on the gateway", gw))
	}
	for k, v := range gwCfg.Settings {
		if strings.Contains(strings.ToLower(k), openshellSettingTelemetry) && v.Type == openshell.SettingValueBool && v.BoolVal {
			out = append(out, fmt.Sprintf("openshell gateway %s has telemetry enabled (%s); sandbox activity is reported off-cluster — disable it on the gateway if that is not intended", gw, k))
		}
	}
	return out
}

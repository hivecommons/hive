package rotation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ccleft "github.com/tuna-os/ccleft"

	"github.com/hivecommons/hive/crux/agent/copilottoken"
	"github.com/hivecommons/hive/pkg/config"
)

// copilotDisplayProvider is the provider key a Copilot reading is stored
// under, matching CopilotProber and rotation.providers.github.
const copilotDisplayProvider = "github"

// copilotUserTokenPath is where the dashboard's Copilot device-flow login
// persists its token (crux.CopilotUserTokenPath). A var as a test seam.
var copilotUserTokenPath = "/data/copilot-user-token"

// copilotDisplaySource reads the Copilot plan's premium-request quota for the
// owner's usage gauge, for hives whose agents run on the copilot backend
// without provider rotation. It reads the quota Copilot itself shows (percent
// left, monthly reset) through ccleft's Copilot reader, so it needs no
// configured allowance — unlike CopilotProber, which reads the documented
// billing endpoint and needs rotation.providers.github.monthly_allowance.
//
// It signs in only with the Copilot login the hive's agents use (see
// copilotAgentToken). With none it reports no_credentials without a network
// call: ccleft's own discovery would fall back to GH_TOKEN, GITHUB_TOKEN or a
// gh login, which on a hive can be a different account from the agents'.
//
// The reading is display-only: "github" is not a contributor-guard provider
// (contributorGuardProviders), so publishContributorReading never writes it
// and no relay holds work on it.
type copilotDisplaySource struct {
	ccleftHeadroomSource
	token func() string
}

func newCopilotDisplaySource(thresholdPct int) copilotDisplaySource {
	return copilotDisplaySource{
		ccleftHeadroomSource: ccleftHeadroomSource{
			provider:     copilotDisplayProvider,
			thresholdPct: thresholdPct,
			source:       ccleft.Source{Provider: ccleft.Copilot, Home: sharedCLIHomePath},
			client:       ccleft.NewClient(nil),
		},
		token: copilotAgentToken,
	}
}

func (s copilotDisplaySource) Probe(ctx context.Context) Headroom {
	tok := ""
	if s.token != nil {
		tok = strings.TrimSpace(s.token())
	}
	if tok == "" {
		return failOpen(s.provider, withCause(ProbeCauseNoCredentials, errors.New("copilot usage: this hive has no Copilot login")))
	}
	inner := s.ccleftHeadroomSource
	inner.source.Credentials = tok
	// An empty Env keeps ccleft from reading a token of its own choosing.
	inner.source.Env = map[string]string{}
	return inner.Probe(ctx)
}

// copilotAgentToken is the GitHub token the hive's Copilot agents sign in
// with, in the agent manager's order: COPILOT_GITHUB_TOKEN from the hive
// process, else the dashboard login's durable token, else the token in the
// shared Copilot CLI config. "" when none is set.
func copilotAgentToken() string {
	if tok := strings.TrimSpace(os.Getenv("COPILOT_GITHUB_TOKEN")); tok != "" {
		return tok
	}
	// The dashboard login is sealed when a keystore provider is active, so it
	// must be opened, not read raw. A read error means no token here.
	if data, err := copilottoken.ReadDurableToken(copilotUserTokenPath); err == nil {
		if tok := strings.TrimSpace(data); tok != "" {
			return tok
		}
	}
	return strings.TrimSpace(copilottoken.ExtractToken(filepath.Join(sharedCLIHomePath, ".copilot", "config.json")))
}

// usageProviderBackends names, per provider, the agent backends that spend
// its plan: what the owner's usage gauge groups agents by. A Claude model run
// through Copilot spends Copilot premium requests, so it is the backend, not
// the model, that picks the provider.
var usageProviderBackends = map[string][]string{
	"anthropic": {"claude"},
	"github":    {"copilot"},
	"openai":    {"codex"},
	"google":    {"agy", "gemini"},
	"aws-kiro":  {"kiro"},
	"deepseek":  {"deepseek"},
}

// UsageProviderForBackend is the provider whose plan an agent on backend
// spends. Backends listed under rotation.providers win over the built-in
// pairs, so an operator's litellm-fronts-deepseek mapping is honoured. ""
// when the backend is unknown.
func UsageProviderForBackend(backend string, configured map[string]config.ProviderRotationConfig) string {
	backend = strings.ToLower(strings.TrimSpace(backend))
	if backend == "" {
		return ""
	}
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, b := range configured[name].Backends {
			if strings.EqualFold(strings.TrimSpace(b), backend) {
				return name
			}
		}
	}
	for provider, backends := range usageProviderBackends {
		for _, b := range backends {
			if b == backend {
				return provider
			}
		}
	}
	return ""
}

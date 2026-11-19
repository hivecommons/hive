package rotation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ccleft "github.com/tuna-os/ccleft"

	"github.com/hivecommons/hive/crux/agent/copilottoken"
	"github.com/hivecommons/hive/crux/keystore"
)

type recordingCCLeftClient struct {
	reading ccleft.Reading
	got     *ccleft.Source
}

func (f recordingCCLeftClient) Get(_ context.Context, src ccleft.Source) ccleft.Reading {
	*f.got = src
	return f.reading
}

// The Copilot gauge reading signs in with the agents' Copilot token and
// nothing else, and maps the premium-request quota to a monthly window.
func TestCopilotDisplaySourceUsesTheAgentsToken(t *testing.T) {
	reset := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	used, remaining := 88.0, 12.0
	var got ccleft.Source
	s := newCopilotDisplaySource(85)
	s.token = func() string { return " agents-token\n" }
	s.client = recordingCCLeftClient{got: &got, reading: ccleft.Reading{
		Provider: ccleft.Copilot,
		State:    ccleft.StateOK,
		Plan:     "business",
		Windows: []ccleft.Window{{
			ID: "premium_interactions", Kind: ccleft.KindMonthly, Binding: true,
			UsedPct: &used, RemainingPct: &remaining, ResetsAt: &reset, Unit: "requests",
		}},
	}}
	h := s.Probe(context.Background())
	if got.Provider != ccleft.Copilot || got.Credentials != "agents-token" {
		t.Fatalf("source = %+v, want Copilot with the agents' token", got)
	}
	if got.Env == nil || len(got.Env) != 0 {
		t.Fatalf("source Env = %v, want empty so ccleft cannot pick GH_TOKEN or GITHUB_TOKEN", got.Env)
	}
	if h.Provider != "github" || h.ProbeErr != nil || h.PctRemaining != 12 || h.PlanType != "business" {
		t.Fatalf("headroom = %+v, want github 12%% left on the business plan", h)
	}
	if len(h.Limits) != 1 || h.Limits[0].ID != "premium_interactions" || h.Limits[0].Kind != "monthly" || !h.Limits[0].ResetAt.Equal(reset) {
		t.Fatalf("limits = %+v, want one monthly premium_interactions window", h.Limits)
	}
}

// Without a Copilot login the source reports no_credentials and never asks
// ccleft, which would otherwise discover a token of its own.
func TestCopilotDisplaySourceWithoutLoginMakesNoCall(t *testing.T) {
	var got ccleft.Source
	s := newCopilotDisplaySource(85)
	s.token = func() string { return "  " }
	s.client = recordingCCLeftClient{got: &got}
	h := s.Probe(context.Background())
	if got.Provider != "" {
		t.Fatalf("ccleft was called with %+v; want no call without a Copilot login", got)
	}
	if h.Provider != "github" || h.ProbeErrCause != ProbeCauseNoCredentials {
		t.Fatalf("headroom = %+v, want github no_credentials", h)
	}
}

// copilotAgentToken follows the agent manager's order: the process env, then
// the dashboard login's durable token, then the shared Copilot CLI config.
func TestCopilotAgentTokenOrder(t *testing.T) {
	dir := t.TempDir()
	oldPath, oldHome := copilotUserTokenPath, sharedCLIHomePath
	t.Cleanup(func() { copilotUserTokenPath, sharedCLIHomePath = oldPath, oldHome })
	copilotUserTokenPath = filepath.Join(dir, "copilot-user-token")
	sharedCLIHomePath = filepath.Join(dir, "home")

	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	if got := copilotAgentToken(); got != "" {
		t.Fatalf("no login anywhere: token = %q, want empty", got)
	}
	if err := os.MkdirAll(filepath.Join(sharedCLIHomePath, ".copilot"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedCLIHomePath, ".copilot", "config.json"), []byte(`{"copilotTokens":{"https://github.com:someone":"config-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := copilotAgentToken(); got != "config-token" {
		t.Fatalf("config only: token = %q, want config-token", got)
	}
	if err := os.WriteFile(copilotUserTokenPath, []byte("durable-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := copilotAgentToken(); got != "durable-token" {
		t.Fatalf("durable login: token = %q, want durable-token", got)
	}
	t.Setenv("COPILOT_GITHUB_TOKEN", "env-token")
	if got := copilotAgentToken(); got != "env-token" {
		t.Fatalf("env set: token = %q, want env-token", got)
	}
}

// The default publisher reads Copilot for the gauge but never publishes it
// to a contributor pool: no relay may hold work on a display-only reading.
func TestContributorReadingPublisherCopilotIsDisplayOnly(t *testing.T) {
	dir := t.TempDir()
	m := NewContributorReadingPublisher(dir, "")
	found := false
	for _, p := range m.probers {
		if p.Provider() == "github" {
			found = true
		}
	}
	if !found {
		t.Fatal("the default publisher has no Copilot reading")
	}
	m.SetHeadroom(failOpen("github", withCause(ProbeCauseRateLimited, os.ErrDeadlineExceeded)))
	for _, backend := range []string{"copilot", "github"} {
		if _, err := os.Stat(ContributorReadingPath(dir, backend, "")); !os.IsNotExist(err) {
			t.Errorf("a Copilot reading was published for backend %q; it must stay display-only", backend)
		}
	}
	if resp := m.HeadroomResponse(); len(resp.Providers) != 1 || resp.Providers[0].Provider != "github" {
		t.Errorf("served providers = %+v, want the github reading", resp.Providers)
	}
}

// hivecommons/hive#18444: with a keystore provider active the dashboard's
// Copilot login is sealed on disk; the gauge must open it, not send the
// envelope to GitHub as the token.
func TestCopilotAgentTokenOpensSealedLogin(t *testing.T) {
	dir := t.TempDir()
	oldPath, oldHome := copilotUserTokenPath, sharedCLIHomePath
	t.Cleanup(func() { copilotUserTokenPath, sharedCLIHomePath = oldPath, oldHome })
	copilotUserTokenPath = filepath.Join(dir, "copilot-user-token")
	sharedCLIHomePath = filepath.Join(dir, "home")
	t.Setenv("COPILOT_GITHUB_TOKEN", "")

	keystore.ResetForTest()
	t.Cleanup(keystore.ResetForTest)
	prov, err := keystore.NewEnvKeyProvider([]byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	keystore.SetActive(prov)

	if err := copilottoken.WriteDurableToken(copilotUserTokenPath, "durable-token"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(copilotUserTokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "durable-token") {
		t.Fatal("login file holds the plaintext token, want a sealed envelope")
	}
	if got := copilotAgentToken(); got != "durable-token" {
		t.Fatalf("sealed login: token = %q, want durable-token", got)
	}
}

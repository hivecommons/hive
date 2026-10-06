package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// envMap builds a getenv func over a fixed map, so no test mutates the process
// environment (and the table can run in parallel).
func envMap(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func fixedPoolDir(dir string) func() string {
	return func() string { return dir }
}

// Publishing must be ON by default: a plain `just contribute-hive codex`
// launch, with no quota variables set at all, is exactly the unattended
// contributor hivecommons/hive#10299 is about.
func TestResolvePlan_DefaultOnForSupportedBackend(t *testing.T) {
	p := resolvePlan(envMap(map[string]string{"AGENT_BACKEND": "codex"}), fixedPoolDir("/pool"))
	if !p.publish {
		t.Fatalf("a default codex contributor must publish, got reason %q", p.reason)
	}
	if p.backend != "codex" || p.poolDir != "/pool" {
		t.Fatalf("plan = %+v, want backend codex in /pool", p)
	}
}

// The relay defaults BACKEND to claude (bin/contributor-relay.js). The
// publisher must default identically or the two resolve different pool keys
// and the reading lands where nothing reads it.
func TestResolvePlan_BackendDefaultsToClaudeLikeTheRelay(t *testing.T) {
	p := resolvePlan(envMap(nil), fixedPoolDir("/pool"))
	if !p.publish || p.backend != "claude" {
		t.Fatalf("plan = %+v, want the relay's claude default", p)
	}
}

func TestResolvePlan_BackendIsCaseAndSpaceNormalized(t *testing.T) {
	p := resolvePlan(envMap(map[string]string{"AGENT_BACKEND": "  Codex "}), fixedPoolDir("/pool"))
	if !p.publish || p.backend != "codex" {
		t.Fatalf("plan = %+v, want normalized codex", p)
	}
}

// The opt-out is the documented escape hatch and must outrank everything else.
func TestResolvePlan_ExplicitOptOut(t *testing.T) {
	for _, raw := range []string{"0", "false", "off", "no", "OFF", " false "} {
		p := resolvePlan(envMap(map[string]string{
			"AGENT_BACKEND":                   "codex",
			"HIVE_CONTRIBUTOR_QUOTA_PUBLISH":  raw,
			"HIVE_CONTRIBUTOR_QUOTA_POOL_DIR": "/pool",
		}), fixedPoolDir("/pool"))
		if p.publish {
			t.Fatalf("HIVE_CONTRIBUTOR_QUOTA_PUBLISH=%q must opt out", raw)
		}
		if !strings.Contains(p.reason, "HIVE_CONTRIBUTOR_QUOTA_PUBLISH") {
			t.Fatalf("reason %q must name the variable that stopped publishing", p.reason)
		}
	}
}

// Anything that is not an explicit false-ish value keeps the default on — an
// unrecognized value must never silently disable quota protection.
func TestResolvePlan_NonFalseValuesKeepPublishing(t *testing.T) {
	for _, raw := range []string{"", "1", "true", "on", "yes", "nonsense"} {
		p := resolvePlan(envMap(map[string]string{
			"AGENT_BACKEND":                  "codex",
			"HIVE_CONTRIBUTOR_QUOTA_PUBLISH": raw,
		}), fixedPoolDir("/pool"))
		if !p.publish {
			t.Fatalf("HIVE_CONTRIBUTOR_QUOTA_PUBLISH=%q must not opt out (reason %q)", raw, p.reason)
		}
	}
}

// With the guard explicitly off there is no consumer for the reading, so
// probing the provider CLI every five minutes would be pure cost.
func TestResolvePlan_GuardOffStandsDown(t *testing.T) {
	p := resolvePlan(envMap(map[string]string{
		"AGENT_BACKEND":                "codex",
		"HIVE_CONTRIBUTOR_QUOTA_GUARD": "OFF",
	}), fixedPoolDir("/pool"))
	if p.publish {
		t.Fatal("guard off must not start a publisher — nothing would read the reading")
	}
	if !strings.Contains(p.reason, "guard is off") {
		t.Fatalf("reason %q must say the guard is off", p.reason)
	}
}

// An explicitly configured external reading source is the operator declaring
// where readings come from. #10299 requires that such a source keeps working
// WITHOUT a competing publisher writing over the same pool.
func TestResolvePlan_ExternalReadingSourceIsNotCompetedWith(t *testing.T) {
	for _, env := range []string{"HIVE_CONTRIBUTOR_QUOTA_READING_FILE", "HIVE_CONTRIBUTOR_QUOTA_READING_JSON"} {
		p := resolvePlan(envMap(map[string]string{
			"AGENT_BACKEND": "codex",
			env:             "/some/reading.json",
		}), fixedPoolDir("/pool"))
		if p.publish {
			t.Fatalf("%s is an operator-declared source; the publisher must stand down", env)
		}
		if !strings.Contains(p.reason, env) {
			t.Fatalf("reason %q must name %s", p.reason, env)
		}
	}
}

// An unsupported backend is reported as unsupported. The failure mode #10299
// calls out is a contributor stranded waiting for a reading that can never be
// produced, so this must be an explicit, named state.
func TestResolvePlan_UnsupportedBackendIsNamed(t *testing.T) {
	p := resolvePlan(envMap(map[string]string{"AGENT_BACKEND": "copilot"}), fixedPoolDir("/pool"))
	if p.publish {
		t.Fatal("copilot has no guard-supported quota reading; publishing must not start")
	}
	if !strings.Contains(p.reason, "copilot") || !strings.Contains(p.reason, "not a supported") {
		t.Fatalf("reason %q must name the backend and say it is unsupported", p.reason)
	}
}

// Every guard-supported backend the relay knows must resolve to a publisher,
// or that backend is stranded on the unprovisioned admit.
func TestResolvePlan_EveryGuardSupportedBackendPublishes(t *testing.T) {
	for _, backend := range []string{"claude", "codex", "agy", "gemini", "kiro"} {
		p := resolvePlan(envMap(map[string]string{"AGENT_BACKEND": backend}), fixedPoolDir("/pool"))
		if !p.publish {
			t.Fatalf("backend %s must publish, got reason %q", backend, p.reason)
		}
	}
}

// An explicit pool dir wins over the derived default, and the account is
// carried through so two relays on one provider account share a pool.
func TestResolvePlan_ExplicitPoolDirAndAccount(t *testing.T) {
	p := resolvePlan(envMap(map[string]string{
		"AGENT_BACKEND":                       "codex",
		"HIVE_CONTRIBUTOR_QUOTA_POOL_DIR":     " /explicit ",
		"HIVE_CONTRIBUTOR_QUOTA_POOL_ACCOUNT": " acct ",
	}), fixedPoolDir("/derived"))
	if p.poolDir != "/explicit" || p.account != "acct" {
		t.Fatalf("plan = %+v, want the explicit pool dir and trimmed account", p)
	}
}

// With no explicit dir and no derivable default there is nowhere to write;
// say so rather than starting a publisher that silently writes nothing.
func TestResolvePlan_NoResolvablePoolDir(t *testing.T) {
	p := resolvePlan(envMap(map[string]string{"AGENT_BACKEND": "codex"}), fixedPoolDir(""))
	if p.publish {
		t.Fatal("no pool directory means nowhere to publish")
	}
	if !strings.Contains(p.reason, "HIVE_CONTRIBUTOR_QUOTA_POOL_DIR") {
		t.Fatalf("reason %q must name the variable that fixes it", p.reason)
	}
}

func cancelledCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx, cancel
}

// A publishing run must say, on startup, that quota protection is active and
// where it publishes — #10299 asks for startup output that makes the state
// obvious to an unattended operator reading logs.
func TestRun_PublishingAnnouncesActiveProtection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	started := 0
	code := run(
		envMap(map[string]string{"AGENT_BACKEND": "codex", "HIVE_CONTRIBUTOR_QUOTA_POOL_DIR": "/pool"}),
		&stdout, &stderr, cancelledCtx,
		func(context.Context, plan) (<-chan struct{}, bool) { started++; return closedDone(), true },
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if started != 1 {
		t.Fatalf("publisher started %d times, want 1", started)
	}
	out := stdout.String()
	for _, want := range []string{"quota protection active", "codex", "/pool"} {
		if !strings.Contains(out, want) {
			t.Fatalf("startup output %q must contain %q", out, want)
		}
	}
}

// Standing down must never take the contributor down with it: the relay's own
// no-source handling decides whether work is admitted, not this process's exit
// code.
func TestRun_StandDownExitsZeroAndExplains(t *testing.T) {
	var stdout, stderr bytes.Buffer
	started := 0
	code := run(
		envMap(map[string]string{"AGENT_BACKEND": "copilot"}),
		&stdout, &stderr, cancelledCtx,
		func(context.Context, plan) (<-chan struct{}, bool) { started++; return closedDone(), true },
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 — a contributor that cannot publish must keep contributing", code)
	}
	if started != 0 {
		t.Fatal("no publisher may start when the plan stood down")
	}
	if !strings.Contains(stderr.String(), "not publishing") {
		t.Fatalf("stderr %q must explain why nothing is published", stderr.String())
	}
}

// Defence in depth: if the publisher constructor itself rejects the backend
// (the two supported-backend sets having drifted apart), say so and exit 0
// rather than reporting protection that is not running.
func TestRun_StartRejectionIsReported(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(
		envMap(map[string]string{"AGENT_BACKEND": "codex", "HIVE_CONTRIBUTOR_QUOTA_POOL_DIR": "/pool"}),
		&stdout, &stderr, cancelledCtx,
		func(context.Context, plan) (<-chan struct{}, bool) { return nil, false },
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.Contains(stdout.String(), "quota protection active") {
		t.Fatal("must not claim active protection when the publisher never started")
	}
	if !strings.Contains(stderr.String(), "no quota reader") {
		t.Fatalf("stderr %q must report the rejected backend", stderr.String())
	}
}

// closedDone is an already-finished publisher, so run() never blocks on a
// stub's exit.
func closedDone() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

// The real seam must build a publisher for a supported backend and refuse an
// unsupported one, and it must hand back a channel the caller can wait on:
// the publish loop writes into the pool directory, so a caller that returned
// while it was still running would tear that directory down underneath it.
//
// PATH is emptied so the provider probe resolves deterministically to
// "executable file not found" on any machine, whether or not a real codex is
// installed — the probe outcome is not what is under test here.
func TestStartPublisher_SupportedBackendRunsAndStops(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done, ok := startPublisher(ctx, plan{backend: "codex", poolDir: t.TempDir()})
	if !ok {
		t.Fatal("codex must get a publisher")
	}
	cancel()
	<-done // the loop has stopped: nothing is writing into the pool dir any more
}

func TestStartPublisher_UnsupportedBackendStartsNothing(t *testing.T) {
	ctx, cancel := cancelledCtx()
	defer cancel()
	done, ok := startPublisher(ctx, plan{backend: "copilot", poolDir: t.TempDir()})
	if ok {
		t.Fatal("copilot must not get a publisher")
	}
	if done != nil {
		t.Fatal("a refused backend must not hand back a channel to wait on")
	}
}

func TestSignalContext_IsCancellable(t *testing.T) {
	ctx, stop := signalContext()
	stop()
	<-ctx.Done()
}

// Pi credentials cannot be handed to the Claude Code/Codex CLI readers.
func TestResolvePlan_PiNamesSelectedProviderWithoutProbingAnotherCLI(t *testing.T) {
	for _, provider := range []string{"openai-codex", "openrouter", "anthropic", "openai"} {
		p := resolvePlan(envMap(map[string]string{
			"AGENT_BACKEND": "pi", "AGENT_MODEL": provider + "/model",
		}), fixedPoolDir("/pool"))
		if p.publish || !strings.Contains(p.reason, "pi provider "+provider) || !strings.Contains(p.reason, "no Pi-compatible quota reader") || !strings.Contains(p.reason, envReadingFile) {
			t.Fatalf("plan = %+v, want explicit unsupported Pi provider with external-source guidance", p)
		}
	}
}

// #10598: the banked-reset redemption controller is default off, only the
// local opt-in enables it, only for codex, and an external reading source
// (which stands the publisher down) never gets a competing controller.
func TestResolvePlan_BankedResetRedeemOptIn(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		publish  bool
		redeem   bool
		wantNote bool
	}{
		{"default off", map[string]string{"AGENT_BACKEND": "codex"}, true, false, false},
		{"explicit off", map[string]string{"AGENT_BACKEND": "codex", envAutoUseBankedReset: "false"}, true, false, false},
		{"opt in", map[string]string{"AGENT_BACKEND": "codex", envAutoUseBankedReset: "1"}, true, true, false},
		{"opt in word", map[string]string{"AGENT_BACKEND": "codex", envAutoUseBankedReset: " Yes "}, true, true, false},
		{"invalid value", map[string]string{"AGENT_BACKEND": "codex", envAutoUseBankedReset: "maybe"}, true, false, true},
		{"non-codex backend", map[string]string{"AGENT_BACKEND": "claude", envAutoUseBankedReset: "on"}, true, false, true},
		{"external reading source", map[string]string{"AGENT_BACKEND": "codex", envAutoUseBankedReset: "1", envReadingFile: "/r.json"}, false, false, false},
		{"publisher opted out", map[string]string{"AGENT_BACKEND": "codex", envAutoUseBankedReset: "1", envPublish: "off"}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := resolvePlan(envMap(tc.env), fixedPoolDir("/pool"))
			if p.publish != tc.publish || p.redeemBankedReset != tc.redeem || (p.redeemNote != "") != tc.wantNote {
				t.Fatalf("plan = %+v, want publish=%v redeem=%v note=%v", p, tc.publish, tc.redeem, tc.wantNote)
			}
		})
	}
}

func TestRun_AnnouncesBankedResetRedeem(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var got plan
	code := run(
		envMap(map[string]string{"AGENT_BACKEND": "codex", "HIVE_CONTRIBUTOR_QUOTA_POOL_DIR": "/pool", envAutoUseBankedReset: "1"}),
		&stdout, &stderr, cancelledCtx,
		func(_ context.Context, p plan) (<-chan struct{}, bool) { got = p; return closedDone(), true },
	)
	if code != 0 || !got.redeemBankedReset {
		t.Fatalf("code=%d plan=%+v, want redemption enabled", code, got)
	}
	if !strings.Contains(stdout.String(), envAutoUseBankedReset+" is on") {
		t.Fatalf("stdout %q must announce the opt-in", stdout.String())
	}
}

func TestRun_ReportsIgnoredBankedResetOptIn(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(
		envMap(map[string]string{"AGENT_BACKEND": "codex", "HIVE_CONTRIBUTOR_QUOTA_POOL_DIR": "/pool", envAutoUseBankedReset: "nope"}),
		&stdout, &stderr, cancelledCtx,
		func(context.Context, plan) (<-chan struct{}, bool) { return closedDone(), true },
	)
	if code != 0 || !strings.Contains(stderr.String(), "must be true or false") {
		t.Fatalf("code=%d stderr=%q, want the invalid opt-in reported", code, stderr.String())
	}
}

func TestStartPublisher_BankedResetRedeemRunsAndStops(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done, ok := startPublisher(ctx, plan{backend: "codex", poolDir: t.TempDir(), redeemBankedReset: true})
	if !ok {
		t.Fatal("codex must get a publisher")
	}
	cancel()
	<-done
}

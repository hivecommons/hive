package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// quietLogger discards output: these tests assert on returned state, not on log
// lines, and a hive booting degraded is expected to log loudly.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// realAppID stands in for a genuine GitHub App ID. Any non-zero value that is
// not config.PlaceholderAppID qualifies; this one is the App the production
// fleet actually uses, so a reader can tell it apart from the sentinel at a
// glance.
const realAppID int64 = 3568013

// realInstallationID is the installation_id observed on the hive that hit this
// bug in production. Its only property that matters is being non-zero — that is
// what flipped the old `AppID != 0 && InstallationID != 0` guard to true.
const realInstallationID int64 = 149757667

// isolateAppKeyLookup points both on-disk key fallbacks at an empty temp dir and
// clears $GH_APP_KEY_FILE, so a test that means "no key anywhere" cannot
// accidentally pick up a real key from /data or /secrets on a dev machine.
func isolateAppKeyLookup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origData, origSecrets := appKeys.DataKeyPath, appKeys.ProvisionedKeyPath
	appKeys.DataKeyPath = filepath.Join(dir, "absent-data-key.pem")
	appKeys.ProvisionedKeyPath = filepath.Join(dir, "absent-secrets-key.pem")
	t.Setenv("GH_APP_KEY_FILE", "")
	t.Cleanup(func() {
		appKeys.DataKeyPath, appKeys.ProvisionedKeyPath = origData, origSecrets
	})
	return dir
}

// TestInitGitHubAuthPlaceholderAppIDBoots is the regression test that matters
// most: it is the exact production failure, reproduced from config alone.
//
// A hosted hive was provisioned with the placeholder app_id 999999999 and no
// GitHub App key. When its owner installed the App, the hub delivered a real
// installation_id — and the hive's config became {placeholder app_id, real
// installation_id, key_file pointing at a Secret mount with no PEM in it}.
//
// The old guard was `AppID != 0 && InstallationID != 0`. 999999999 is non-zero,
// so that read as "App auth is configured", NewAppAuth failed to read the
// absent PEM, and main() called os.Exit(1) — before the HTTP listener bound.
// The pod crashlooped permanently (89 restarts on one, 24 on its replacement),
// never heartbeated, showed offline on the hub, and wedged the rollout because
// the new pod could never go Ready.
//
// Booting is the assertion. Nine more hives were carrying this exact config.
//
// It also locks the CLASSIFICATION: a sentinel hive is operator-actionable
// (AppStateNoAppAssigned), never AppStateNotInstalled.
func TestInitGitHubAuthPlaceholderAppIDBoots(t *testing.T) {
	dir := isolateAppKeyLookup(t)

	cfg := &config.Config{}
	cfg.Project.Org = "jeejz"
	cfg.GitHub.AppID = config.PlaceholderAppID
	cfg.GitHub.InstallationID = realInstallationID
	// A Secret mount that exists but holds no PEM — the state that made
	// NewAppAuth fail. Pointing at a path inside the temp dir keeps the test
	// hermetic.
	cfg.GitHub.KeyFile = filepath.Join(dir, "gh-app-key.pem")

	got := initGitHubAuth(context.Background(), cfg, quietLogger())

	// The whole point: the placeholder must never be mistaken for a real App.
	if got.AppAuth != nil {
		t.Fatal("initialized GitHub App auth from the placeholder app_id; " +
			"this is the crashloop bug — HasUsableApp() must reject the sentinel")
	}
	if got.Client != nil {
		t.Error("built a GitHub client with no credentials; agents would act unauthenticated")
	}
	if got.Failure == "" {
		t.Fatal("no failure reason recorded; the dashboard banner would show nothing " +
			"and the owner could not tell why agents are idle")
	}
	if !strings.Contains(got.Failure, "placeholder") {
		t.Errorf("failure reason does not identify the placeholder as the cause: %q", got.Failure)
	}
	// A placeholder hive is OPERATOR-actionable, not user-actionable.
	//
	// This assertion was inverted until the placeholder-assignment fix. The
	// premise "the owner can fix this by installing the App" is false: the
	// sentinel app_id names no GitHub App, so the JWT fails before any
	// installation is consulted. The App is typically already installed and the
	// installation_id already correct (the reporting owner's was the very same
	// installation another working hive uses) — only the hub can supply the real
	// app_id. Classifying it as "not installed" pointed the owner at the
	// install link and the Set-ID box, i.e. at the one field already right.
	if got.State != github.AppStateNoAppAssigned {
		t.Errorf("State = %v, want AppStateNoAppAssigned", got.State)
	}
	if got.State.UserActionable() {
		t.Error("placeholder app_id is not user-actionable: no installation_id the owner " +
			"enters can make a nonexistent app_id authenticate")
	}
	if !got.State.OperatorActionable() {
		t.Error("only the hub operator can assign a real app_id; the banner must say so")
	}
}

// TestInitGitHubAuthPlaceholderWithoutInstallation covers the nine hives that
// were armed but had not detonated yet: placeholder app_id, installation_id
// still zero. This booted before the fix too, so the test guards against a
// regression in the other direction — the degraded path must stay non-fatal and
// must still explain itself.
func TestInitGitHubAuthPlaceholderWithoutInstallation(t *testing.T) {
	isolateAppKeyLookup(t)

	cfg := &config.Config{}
	cfg.Project.Org = "available-pool"
	cfg.GitHub.AppID = config.PlaceholderAppID

	got := initGitHubAuth(context.Background(), cfg, quietLogger())

	if got.AppAuth != nil || got.Client != nil {
		t.Fatal("a placeholder with no installation must yield no GitHub credentials")
	}
	if !strings.Contains(got.Failure, "placeholder") {
		t.Errorf("failure reason = %q, want it to name the placeholder", got.Failure)
	}
}

// TestInitGitHubAuthRealAppMissingKeyDegrades covers requirement 3: a hive whose
// App is genuinely configured but whose private key is absent must NOT exit. It
// must boot, and the message must be actionable enough to fix without reading
// the source — the old "reading app key /secrets/gh-app-key.pem: no such file"
// never revealed that four paths are consulted in a fixed order, so operators
// routinely put the key in the wrong one.
func TestInitGitHubAuthRealAppMissingKeyDegrades(t *testing.T) {
	dir := isolateAppKeyLookup(t)
	missing := filepath.Join(dir, "never-provisioned.pem")

	cfg := &config.Config{}
	cfg.Project.Org = "kubestellar"
	cfg.GitHub.AppID = realAppID
	cfg.GitHub.InstallationID = realInstallationID
	cfg.GitHub.KeyFile = missing

	got := initGitHubAuth(context.Background(), cfg, quietLogger())

	if got.AppAuth != nil {
		t.Fatal("App auth initialized without a readable private key")
	}
	if got.Client != nil {
		t.Error("built a GitHub client despite unusable App credentials")
	}
	if got.Failure == "" {
		t.Fatal("a real App with an unreadable key must record a diagnosable failure, not exit")
	}
	// The exact path tried, so the operator does not have to guess which of the
	// four candidates won.
	if !strings.Contains(got.Failure, missing) {
		t.Errorf("failure reason omits the path actually tried (%q): %q", missing, got.Failure)
	}
	// The resolution order, named explicitly.
	for _, want := range []string{"GH_APP_KEY_FILE", "github.key_file", appKeys.DataKeyPath, appKeys.ProvisionedKeyPath} {
		if !strings.Contains(got.Failure, want) {
			t.Errorf("failure reason omits %q from the resolution order: %q", want, got.Failure)
		}
	}
	// An absent key is the hub's failure to deliver, never the owner's. Telling
	// them to "install the GitHub App" sends them to redo work they already did
	// — and the journey nudges can escalate to threatening de-provisioning over
	// it. That is exactly what OperatorActionable() guards against.
	if got.State != github.AppStateKeyMissing {
		t.Errorf("State = %v, want AppStateKeyMissing", got.State)
	}
	if !got.State.OperatorActionable() {
		t.Error("a missing key is operator-actionable; classifying it otherwise blames the user " +
			"for a credential the hub never delivered")
	}
}

// TestInitGitHubAuthRealAppMalformedKeyIsKeyInvalid separates "no key" from "a
// key that is not parseable". Both are operator-actionable, but the hub needs
// the distinction to tell "never pushed" from "pushed something broken" — the
// classic symptom being a public github.com key on a GitHub Enterprise hive.
func TestInitGitHubAuthRealAppMalformedKeyIsKeyInvalid(t *testing.T) {
	dir := isolateAppKeyLookup(t)
	garbage := filepath.Join(dir, "not-a-pem.pem")
	if err := os.WriteFile(garbage, []byte("this is not a PEM block"), 0o600); err != nil {
		t.Fatalf("write garbage key: %v", err)
	}

	cfg := &config.Config{}
	cfg.Project.Org = "kubestellar"
	cfg.GitHub.AppID = realAppID
	cfg.GitHub.InstallationID = realInstallationID
	cfg.GitHub.KeyFile = garbage

	got := initGitHubAuth(context.Background(), cfg, quietLogger())

	if got.AppAuth != nil {
		t.Fatal("App auth initialized from a file that is not a PEM key")
	}
	if got.State != github.AppStateKeyInvalid {
		t.Errorf("State = %v, want AppStateKeyInvalid — a present-but-unparseable key is "+
			"a different operator fault than a missing one", got.State)
	}
	if !got.State.OperatorActionable() {
		t.Error("a broken key is operator-actionable, not the owner's to fix")
	}
}

// TestInitGitHubAuthRealAppValidKey is the positive control: with a real app_id,
// a real installation_id and a readable PEM, App auth must still initialize
// normally. Without this, "never initialize App auth" would pass every other
// test in this file.
func TestInitGitHubAuthRealAppValidKey(t *testing.T) {
	dir := isolateAppKeyLookup(t)
	keyPath := filepath.Join(dir, "gh-app-key.pem")
	writeTestKey(t, keyPath)

	cfg := &config.Config{}
	// Org is deliberately empty: healGitHubAppInstallation returns immediately
	// on an empty org, so this test makes no network call.
	cfg.GitHub.AppID = realAppID
	cfg.GitHub.InstallationID = realInstallationID
	cfg.GitHub.KeyFile = keyPath

	got := initGitHubAuth(context.Background(), cfg, quietLogger())

	if got.AppAuth == nil {
		t.Fatal("App auth did not initialize with a real app_id and a valid key; " +
			"the placeholder guard has over-rejected")
	}
	if got.Client == nil {
		t.Error("no GitHub client built despite working App auth")
	}
	if got.Failure != "" {
		t.Errorf("healthy App auth recorded a failure: %q", got.Failure)
	}
}

// TestInitGitHubAuthTokenStillWorks guards the token-authenticated hives, which
// share this code path and must be unaffected by the App changes.
func TestInitGitHubAuthTokenStillWorks(t *testing.T) {
	isolateAppKeyLookup(t)
	t.Setenv("HIVE_GITHUB_TOKEN", "")

	cfg := &config.Config{}
	cfg.Project.Org = "kubestellar"
	cfg.GitHub.Token = "ghp_exampletoken"

	got := initGitHubAuth(context.Background(), cfg, quietLogger())

	if got.Client == nil {
		t.Fatal("token-authenticated hive got no GitHub client")
	}
	if got.AppAuth != nil {
		t.Error("token auth must not produce App auth")
	}
	if got.Failure != "" {
		t.Errorf("token auth recorded a failure: %q", got.Failure)
	}
}

// TestInitGitHubAuthPlaceholderWithTokenPrefersToken covers a placeholder hive
// that also carries a token: the token is real credentials and must win, rather
// than the hive being parked in dashboard-only mode over a sentinel app_id.
func TestInitGitHubAuthPlaceholderWithTokenPrefersToken(t *testing.T) {
	isolateAppKeyLookup(t)
	t.Setenv("HIVE_GITHUB_TOKEN", "")

	cfg := &config.Config{}
	cfg.Project.Org = "kubestellar"
	cfg.GitHub.AppID = config.PlaceholderAppID
	cfg.GitHub.Token = "ghp_exampletoken"

	got := initGitHubAuth(context.Background(), cfg, quietLogger())

	if got.Client == nil {
		t.Fatal("a placeholder app_id suppressed a perfectly good token")
	}
	if got.Failure != "" {
		t.Errorf("working token credentials recorded a failure: %q", got.Failure)
	}
}

// TestInitGitHubAuthNoCredentialsDoesNotExit covers the last degraded shape.
// config.validate() rejects this at load, so it is only reachable if the config
// was mutated afterwards — but "unreachable" is exactly what the placeholder
// path was assumed to be, and it still has to not kill the process.
func TestInitGitHubAuthNoCredentialsDoesNotExit(t *testing.T) {
	isolateAppKeyLookup(t)
	t.Setenv("HIVE_GITHUB_TOKEN", "")

	cfg := &config.Config{}
	cfg.Project.Org = "kubestellar"

	got := initGitHubAuth(context.Background(), cfg, quietLogger())

	if got.Client != nil || got.AppAuth != nil {
		t.Fatal("credentials materialized from an empty config")
	}
	if got.Failure == "" {
		t.Error("no failure reason recorded for a hive with no credentials at all")
	}
}

// TestDescribeAppKeyFailureNamesEnvOverride checks that an operator who set
// $GH_APP_KEY_FILE sees that value echoed back, so a typo in the env var is
// visible in the message rather than having to be inferred from the resolved
// path.
func TestDescribeAppKeyFailureNamesEnvOverride(t *testing.T) {
	msg := describeAppKeyFailure("/secrets/gh-app-key.pem", "/tmp/typo.pem", "/tmp/typo.pem", os.ErrNotExist)

	if !strings.Contains(msg, "/tmp/typo.pem") {
		t.Errorf("message omits the env override value: %q", msg)
	}
	if !strings.Contains(msg, "/secrets/gh-app-key.pem") {
		t.Errorf("message omits the configured key_file, so its precedence is unclear: %q", msg)
	}
	// With both sources set, nothing is "(unset)".
	if strings.Contains(msg, "(unset)") {
		t.Errorf("both key sources were set, so none should render as unset: %q", msg)
	}

	// With neither set, the message must say so rather than printing a bare
	// "=" and leaving the operator to guess whether the value was empty or the
	// message was truncated.
	bare := describeAppKeyFailure("", "", appKeys.ProvisionedKeyPath, os.ErrNotExist)
	if strings.Count(bare, "(unset)") != 2 {
		t.Errorf("both unset sources should render as (unset): %q", bare)
	}
}

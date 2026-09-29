package main

// Tests for configureGitHubClient (hivecommons/hive#9614): the one function
// that installs every hook on a *github.Client, called at boot and on every
// credential rebuild.
//
// Three layers, because each catches a regression the others cannot:
//
//   - TestGitHubClientSetHooksAllConfigured inventories every exported Set*
//     method on *github.Client by reflection and requires each to be reachable
//     from configureGitHubClient or explicitly allowlisted. A new hook added
//     to the client and wired only at one boot site fails here.
//   - TestGitHubClientRebuildSitesUseSharedConstructor pins the call sites:
//     rebuilds construct through newConfiguredGitHubAppClient and never set
//     hooks inline, so a new rebuild path cannot start with a hand-kept list.
//   - TestRebuiltGitHubClientEnforcesPRRepoPolicyGate is the behavioural
//     regression for the issue: after a simulated App credential rebuild the
//     PR repo policy gate still refuses an agent whose repo level forbids PRs.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

// clientSetterAllowlist names the exported Set* methods on *github.Client that
// configureGitHubClient deliberately does NOT call, each with the reason. A
// method belongs here only when it is not a hook a rebuilt client could lose:
// it is a constructor argument, it is owned by a different flow, or it is not
// wired in production at all. Anything else goes in configureGitHubClient.
var clientSetterAllowlist = map[string]string{
	"SetOrg": "constructor argument; re-pointed at runtime only by the dashboard's " +
		"org-adoption flow (pkg/dashboard), which mutates the live client in place",
	"SetRepos": "constructor argument (every rebuild passes b.cfg.Project.Repos); " +
		"re-synced on config reload, bootState overrides and heartbeat project claims",
	"SetAppBotLogin": "constructor argument: NewClientFromAppWithBotLogin records the bot login itself",
	"SetApprovalDesk": "not wired in production: the self-authored auto-merge sweep receives " +
		"the approval desk through automerge.Options.ApprovalDesk",
	"SetMergeRequestPolicy": "combined form of SetMergeRequestAllowUnprotectedBaseRepos and " +
		"SetMergeRequestNoCIAllowedRepos, which syncAutoMergePolicyToGitHubClient calls individually",
	"SetAttributionAudit": "legacy untyped audit sink, superseded by SetAttributionAuditRecord " +
		"(#9587), which configureGitHubClient installs and which takes precedence",
}

// clientResyncSetters may be called directly on b.ghClient outside
// configureGitHubClient: they re-sync a config value on the LIVE client after
// a config reload or heartbeat claim. Each is also applied by
// configureGitHubClient (or allowlisted above), so a rebuild still gets it.
var clientResyncSetters = map[string]bool{
	"SetRepos":          true,
	"SetHoldLabels":     true,
	"SetExemptLabels":   true,
	"SetAutoMergeLabel": true,
	"SetIssueFilter":    true,
}

const (
	rebuildTestHiveID    = "rebuild-test"
	rebuildTestOpenRepo  = "acme/open"
	rebuildTestLockedRep = "acme/locked"
	// rebuildTestHiveLevel lets "quality" open PRs; rebuildTestLockedLevel
	// (advisory) does not. See agent.DefaultAgentMode.
	rebuildTestHiveLevel   = 5
	rebuildTestLockedLevel = 2
	rebuildTestAgent       = "quality"
	rebuildTestAppID       = 424242
	rebuildTestInstallID   = 777
)

func rebuildTestConfig(withApp bool) *config.Config {
	cfg := &config.Config{}
	cfg.HiveID = rebuildTestHiveID
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{rebuildTestOpenRepo, rebuildTestLockedRep}
	hiveLevel, lockedLevel := rebuildTestHiveLevel, rebuildTestLockedLevel
	cfg.ACMMLevel = &hiveLevel
	cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: rebuildTestLockedRep, ACMMLevel: &lockedLevel}}
	cfg.Governor.Labels.Exempt = []string{"wip"}
	cfg.Governor.Labels.AutoMerge = "ship-it"
	if withApp {
		cfg.GitHub.AppID = rebuildTestAppID
		cfg.GitHub.InstallationID = rebuildTestInstallID
	}
	return cfg
}

// testAppAuth builds an App auth from a throwaway in-memory key, the way a
// credential re-delivery hands the rebuild paths a fresh one.
func testAppAuth(t *testing.T, cfg *config.Config) *github.AppAuth {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, testRSAKeyBits)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	auth, err := github.NewAppAuthFromPEM(cfg.GitHub.AppID, cfg.GitHub.InstallationID, pemData,
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), cfg.GitHub.ResolvedAPIURL())
	if err != nil {
		t.Fatalf("NewAppAuthFromPEM: %v", err)
	}
	return auth
}

// --- behavioural regression (#9614) ------------------------------------------

func TestRebuiltGitHubClientEnforcesPRRepoPolicyGate(t *testing.T) {
	cfg := rebuildTestConfig(true)
	b, _ := newDepsTestBoot(t, cfg)

	// Boot: initGitHubAuth builds the App client, bootGitHub configures it.
	bootAuth := testAppAuth(t, cfg)
	b.bootGitHubWith(bootGitHubDeps{
		initGitHubAuth: func(_ context.Context, c *config.Config, logger *slog.Logger) githubAuth {
			client := github.NewClientFromAppWithBotLogin(bootAuth, c.Project.Org, c.Project.Repos, logger, c.GitHub.BotLogin())
			return githubAuth{Client: client, AppAuth: bootAuth}
		},
	})
	bootClient := b.ghClient
	if bootClient == nil {
		t.Fatal("boot left no client")
	}

	assertGate := func(label string, c *github.Client) {
		t.Helper()
		err := c.CheckPRRepoPolicy(rebuildTestAgent, rebuildTestLockedRep)
		if err == nil {
			t.Fatalf("%s client: PR repo policy gate let %s open a PR on %s (effective L%d) - policy bypass",
				label, rebuildTestAgent, rebuildTestLockedRep, rebuildTestLockedLevel)
		}
		if !strings.Contains(err.Error(), "does not allow "+rebuildTestAgent+" to open PRs") {
			t.Fatalf("%s client: unexpected refusal %q", label, err)
		}
		if err := c.CheckPRRepoPolicy(rebuildTestAgent, rebuildTestOpenRepo); err != nil {
			t.Fatalf("%s client: gate refused %s on %s (hive L%d): %v - over-gating",
				label, rebuildTestAgent, rebuildTestOpenRepo, rebuildTestHiveLevel, err)
		}
	}
	assertGate("boot", bootClient)

	// Positive control: a bare App client (what the rebuild paths produced
	// before #9614, which never re-installed this gate) has no gate at all,
	// so the assertions above and below are capable of failing.
	bare := github.NewClientFromAppWithBotLogin(testAppAuth(t, cfg), cfg.Project.Org, cfg.Project.Repos, b.logger, cfg.GitHub.BotLogin())
	if err := bare.CheckPRRepoPolicy(rebuildTestAgent, rebuildTestLockedRep); err != nil {
		t.Fatalf("positive control: an unconfigured client already refuses (%v); the regression check is vacuous", err)
	}

	// Rebuild: fresh credentials arrive, the rebuild path constructs through
	// the shared constructor (pinned below by
	// TestGitHubClientRebuildSitesUseSharedConstructor).
	rebuilt := b.newConfiguredGitHubAppClient(testAppAuth(t, cfg))
	if rebuilt == bootClient {
		t.Fatal("rebuild returned the boot client; nothing was rebuilt")
	}
	assertGate("rebuilt", rebuilt)

	// The gate reads the live config: a per-repo level lowered after the
	// rebuild takes effect without another rebuild.
	lowered := rebuildTestLockedLevel
	cfg.Project.RepoPolicies = append(cfg.Project.RepoPolicies, config.RepoPolicy{Repo: rebuildTestOpenRepo, ACMMLevel: &lowered})
	if err := rebuilt.CheckPRRepoPolicy(rebuildTestAgent, rebuildTestOpenRepo); err == nil {
		t.Fatal("rebuilt client's gate did not see a repo level lowered after the rebuild")
	}
}

// The App-only policy tier keeps its boot-time gate: a PAT-authenticated hive
// (no usable App) never had these hooks and runs no App relay they govern.
// The config tier still applies.
func TestConfigureGitHubClientWithoutAppSkipsAppPolicyTier(t *testing.T) {
	cfg := rebuildTestConfig(false)
	b, _ := newDepsTestBoot(t, cfg)
	client := fakeGitHubClient(t)

	b.configureGitHubClient(client)

	if err := client.CheckPRRepoPolicy(rebuildTestAgent, rebuildTestLockedRep); err != nil {
		t.Fatalf("PAT client got the App-only PR repo policy gate: %v", err)
	}
	if got := client.AutoMergeLabel(); got != "ship-it" {
		t.Fatalf("config tier not applied: auto-merge label = %q, want ship-it", got)
	}
	if !client.IsHeldLabels([]string{github.CanonicalHiveHoldLabel(rebuildTestHiveID)}) {
		t.Fatal("config tier not applied: the canonical hive hold label does not hold")
	}
}

// Boot applies tiers as their dependencies come up; a tier whose dependency
// is still nil must skip itself rather than install a hook that would panic
// on first use, and a later call (the rebuild) must apply it. Repeating the
// call must not change the outcome.
func TestConfigureGitHubClientTiersWaitForDependencies(t *testing.T) {
	cfg := rebuildTestConfig(true)
	b, _ := newDepsTestBoot(t, cfg)
	client := github.NewClientFromAppWithBotLogin(testAppAuth(t, cfg), cfg.Project.Org, cfg.Project.Repos, b.logger, cfg.GitHub.BotLogin())

	// No agent manager, dashboard server or mutation boundary yet.
	b.configureGitHubClient(client)
	if err := client.CheckPRRepoPolicy(rebuildTestAgent, rebuildTestLockedRep); err == nil {
		t.Fatal("App policy tier (no dependencies) was not applied")
	}

	b.agentMgr = agent.NewManager(map[string]config.AgentConfig{}, b.logger, agent.ProjectContext{})
	b.dashSrv = dashboard.NewServer(0, b.logger)
	b.configureGitHubClient(client)
	b.configureGitHubClient(client)

	if err := client.CheckPRRepoPolicy(rebuildTestAgent, rebuildTestLockedRep); err == nil {
		t.Fatal("re-applying configureGitHubClient dropped the PR repo policy gate")
	}
	if got := client.AutoMergeLabel(); got != "ship-it" {
		t.Fatalf("re-applying changed the auto-merge label to %q", got)
	}
	level := b.agentMgr.GetACMMLevel()
	if got, want := b.selfAuthorizationHoldEnabled(rebuildTestLockedRep), cfg.SelfAuthorizationHoldEnabledForRepoAtLevel(rebuildTestLockedRep, level); got != want {
		t.Fatalf("selfAuthorizationHoldEnabled = %v, want %v (the config's answer at the manager's level)", got, want)
	}

	// Nil-safety: nothing to configure, nothing to panic on.
	b.configureGitHubClient(nil)
	var nilBoot *boot
	nilBoot.configureGitHubClient(client)
}

// A rebuilt client must carry exactly the hold labels the boot client does:
// the canonical hive-pause/<id> hold (the only hive-configured hold label;
// the generic "hold" substrings are built into pkg/github). Before #9614 each
// rebuild site re-typed this list by hand.
func TestRebuiltGitHubClientGetsBootHoldLabels(t *testing.T) {
	cfg := rebuildTestConfig(true)
	b, _ := newDepsTestBoot(t, cfg)
	bootAuth := testAppAuth(t, cfg)
	b.bootGitHubWith(bootGitHubDeps{
		initGitHubAuth: func(_ context.Context, c *config.Config, logger *slog.Logger) githubAuth {
			client := github.NewClientFromAppWithBotLogin(bootAuth, c.Project.Org, c.Project.Repos, logger, c.GitHub.BotLogin())
			return githubAuth{Client: client, AppAuth: bootAuth}
		},
	})
	rebuilt := b.newConfiguredGitHubAppClient(testAppAuth(t, cfg))

	canonical := []string{github.CanonicalHiveHoldLabel(rebuildTestHiveID)}
	otherHive := []string{github.CanonicalHiveHoldLabel("some-other-hive")}
	bare := github.NewClientFromAppWithBotLogin(testAppAuth(t, cfg), cfg.Project.Org, cfg.Project.Repos, b.logger, cfg.GitHub.BotLogin())
	if bare.IsHeldLabels(canonical) {
		t.Fatal("positive control: an unconfigured client already holds the canonical label; the check is vacuous")
	}
	for label, c := range map[string]*github.Client{"boot": b.ghClient, "rebuilt": rebuilt} {
		if !c.IsHeldLabels(canonical) {
			t.Fatalf("%s client does not hold %v", label, canonical)
		}
		if c.IsHeldLabels(otherHive) {
			t.Fatalf("%s client holds another hive's pause label %v; the hold must be hive-scoped", label, otherHive)
		}
	}
	if got, want := b.githubHoldLabels(), canonical; !reflect.DeepEqual(got, want) {
		t.Fatalf("githubHoldLabels = %v, want %v", got, want)
	}
}

// Pins #9371: the legacy hive/<id> label is the provenance label on every
// item the hive claims and is never a hold, not even when it is passed to the
// client explicitly as a hold label. A failed hold-label migration therefore
// cannot "fail closed" by adding it (that parked every claimed item), which is
// why runLoopWith warns instead.
func TestProvenanceLabelIsNeverAHold(t *testing.T) {
	cfg := rebuildTestConfig(true)
	b, _ := newDepsTestBoot(t, cfg)
	client := b.newConfiguredGitHubAppClient(testAppAuth(t, cfg))
	provenance := github.HiveProvenanceLabel(rebuildTestHiveID)

	if client.IsHeldLabels([]string{provenance}) {
		t.Fatalf("configured client treats provenance label %q as a hold", provenance)
	}
	client.SetHoldLabels([]string{github.CanonicalHiveHoldLabel(rebuildTestHiveID), provenance})
	if client.IsHeldLabels([]string{provenance}) {
		t.Fatalf("provenance label %q became a hold when passed explicitly; this recreates #9371", provenance)
	}
	// A hive ID containing "hold" must not turn its provenance label into a
	// hold through the generic substring rule either.
	holdy := github.HiveProvenanceLabel("hosted-placeholder-r05x")
	if client.IsHeldLabels([]string{holdy}) {
		t.Fatalf("provenance label %q matched the generic hold substring", holdy)
	}
	// Positive control: the canonical hold still holds.
	if !client.IsHeldLabels([]string{github.CanonicalHiveHoldLabel(rebuildTestHiveID)}) {
		t.Fatal("positive control: the canonical hold label does not hold")
	}
}

func TestHoldMigrationFailedAlertMessage(t *testing.T) {
	msg := holdMigrationFailedAlertMessage(rebuildTestHiveID, "/data/hold-migration.json")
	for _, want := range []string{
		github.HiveProvenanceLabel(rebuildTestHiveID),
		github.CanonicalHiveHoldLabel(rebuildTestHiveID),
		"NOT treated as held",
		"#9371",
		"/data/hold-migration.json",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("alert message %q missing %q", msg, want)
		}
	}
	if strings.Contains(holdMigrationFailedAlertMessage(rebuildTestHiveID, ""), "Migration report") {
		t.Error("alert message names a report when there is none")
	}
}

// --- inventory parity ---------------------------------------------------------

// hivePackageFiles parses this package's non-test sources.
func hivePackageFiles(t *testing.T) []*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	return files
}

func isBootMethod(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return false
	}
	star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "boot"
}

// exprString renders an identifier or selector chain ("b.ghClient"); anything
// else renders as "".
func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if inner := exprString(x.X); inner != "" {
			return inner + "." + x.Sel.Name
		}
	}
	return ""
}

// clientSetterNames lists every exported Set* method on *github.Client.
func clientSetterNames() []string {
	typ := reflect.TypeOf((*github.Client)(nil))
	var names []string
	for i := 0; i < typ.NumMethod(); i++ {
		if name := typ.Method(i).Name; strings.HasPrefix(name, "Set") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// setCallsReachableFrom returns the names of every Set* method called
// anywhere in the in-package call graph rooted at boot method root: package
// functions it calls, boot methods it calls or passes as values, and the
// closures inside them. The receiver type is not resolved, so a same-named
// setter on another type also counts; that can only make this test more
// permissive, never produce a false failure.
func setCallsReachableFrom(t *testing.T, files []*ast.File, root string) map[string]bool {
	t.Helper()
	funcs := map[string][]*ast.FuncDecl{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			switch {
			case fd.Recv == nil:
				funcs[fd.Name.Name] = append(funcs[fd.Name.Name], fd)
			case isBootMethod(fd):
				funcs["b."+fd.Name.Name] = append(funcs["b."+fd.Name.Name], fd)
			}
		}
	}
	rootKey := "b." + root
	if len(funcs[rootKey]) == 0 {
		t.Fatalf("(*boot).%s not found in package sources", root)
	}
	sets := map[string]bool{}
	seen := map[string]bool{}
	queue := []string{rootKey}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if seen[key] {
			continue
		}
		seen[key] = true
		for _, fd := range funcs[key] {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					switch fun := x.Fun.(type) {
					case *ast.Ident:
						if _, ok := funcs[fun.Name]; ok {
							queue = append(queue, fun.Name)
						}
					case *ast.SelectorExpr:
						if strings.HasPrefix(fun.Sel.Name, "Set") {
							sets[fun.Sel.Name] = true
						}
					}
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "b" {
						if _, ok := funcs["b."+x.Sel.Name]; ok {
							queue = append(queue, "b."+x.Sel.Name)
						}
					}
				}
				return true
			})
		}
	}
	return sets
}

func TestGitHubClientSetHooksAllConfigured(t *testing.T) {
	setters := clientSetterNames()
	if len(setters) == 0 {
		t.Fatal("reflection found no Set* methods on *github.Client; the inventory is broken")
	}
	reached := setCallsReachableFrom(t, hivePackageFiles(t), "configureGitHubClient")

	// Positive controls: hooks known to be installed there must be found, or
	// the call-graph walk itself is broken and every assertion is vacuous.
	for _, known := range []string{"SetPRRepoPolicyGate", "SetHoldLabels", "SetReviewBots", "SetRequiredChecks", "SetCanaryScanner"} {
		if !reached[known] {
			t.Fatalf("positive control: %s not found reachable from configureGitHubClient; the walk is broken", known)
		}
	}

	isSetter := map[string]bool{}
	for _, name := range setters {
		isSetter[name] = true
		if _, allowed := clientSetterAllowlist[name]; !reached[name] && !allowed {
			t.Errorf("(*github.Client).%s is not applied by configureGitHubClient. A hook set only at "+
				"boot is silently lost when the client is rebuilt on an App credential change (#9614). "+
				"Install it in configureGitHubClient (github_client_config.go), or, if it is genuinely not "+
				"a hook (constructor argument, owned by another flow), add it to clientSetterAllowlist "+
				"with the reason", name)
		}
	}
	for name := range clientSetterAllowlist {
		if !isSetter[name] {
			t.Errorf("clientSetterAllowlist names %s, which is no longer a method on *github.Client; remove it", name)
		}
	}
	for name := range clientResyncSetters {
		if !isSetter[name] {
			t.Errorf("clientResyncSetters names %s, which is no longer a method on *github.Client; remove it", name)
		}
		_, allowed := clientSetterAllowlist[name]
		if !reached[name] && !allowed {
			t.Errorf("resync setter %s is not applied by configureGitHubClient; a rebuild would lose it", name)
		}
	}
}

func TestGitHubClientRebuildSitesUseSharedConstructor(t *testing.T) {
	isSetter := map[string]bool{}
	for _, name := range clientSetterNames() {
		isSetter[name] = true
	}
	wantConstructors := map[string]bool{"initGitHubAuth": true, "newConfiguredGitHubAppClient": true}
	gotConstructors := map[string]bool{}
	rebuildSites := 0

	for _, f := range hivePackageFiles(t) {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := fd.Name.Name
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					recv := exprString(sel.X)
					switch {
					case recv == "github" && sel.Sel.Name == "NewClientFromAppWithBotLogin":
						gotConstructors[fn] = true
					case recv == "b" && sel.Sel.Name == "newConfiguredGitHubAppClient":
						rebuildSites++
					case isSetter[sel.Sel.Name] && (recv == "newClient" || recv == "out.Client"):
						t.Errorf("%s: %s.%s called inline on a freshly built client; install it in "+
							"configureGitHubClient so boot and every rebuild apply it (#9614)", fn, recv, sel.Sel.Name)
					case isSetter[sel.Sel.Name] && recv == "b.ghClient" && !clientResyncSetters[sel.Sel.Name]:
						t.Errorf("%s: b.ghClient.%s installed directly on the live client; a rebuild would "+
							"drop it. Install it in configureGitHubClient (#9614)", fn, sel.Sel.Name)
					}
				case *ast.AssignStmt:
					for i, lhs := range x.Lhs {
						name := exprString(lhs)
						var rhs ast.Expr
						if len(x.Rhs) == len(x.Lhs) {
							rhs = x.Rhs[i]
						}
						if name == "b.ghClient" && fn != "bootGitHubWith" && exprString(rhs) != "newClient" {
							t.Errorf("%s: b.ghClient assigned from something other than a rebuilt newClient; "+
								"construct it with b.newConfiguredGitHubAppClient (#9614)", fn)
						}
						if name == "newClient" && x.Tok == token.DEFINE {
							call, ok := rhs.(*ast.CallExpr)
							if !ok || exprString(call.Fun) != "b.newConfiguredGitHubAppClient" {
								t.Errorf("%s: newClient built without b.newConfiguredGitHubAppClient; a rebuilt "+
									"client must get every boot-time hook (#9614)", fn)
							}
						}
					}
				}
				return true
			})
		}
	}

	for fn := range gotConstructors {
		if !wantConstructors[fn] {
			t.Errorf("%s calls github.NewClientFromAppWithBotLogin directly; rebuild through "+
				"b.newConfiguredGitHubAppClient so the client is configured (#9614)", fn)
		}
	}
	for fn := range wantConstructors {
		if !gotConstructors[fn] {
			t.Errorf("expected %s to construct the App client; the source walk may be broken", fn)
		}
	}
	// The dashboard's ReinitGitHubFunc, the config watcher and the heartbeat
	// App delivery. Fewer means a site stopped using the shared constructor
	// (or the walk is broken).
	const knownRebuildSites = 3
	if rebuildSites < knownRebuildSites {
		t.Errorf("found %d calls to b.newConfiguredGitHubAppClient, want at least %d rebuild sites", rebuildSites, knownRebuildSites)
	}
}

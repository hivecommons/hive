// Package apphealth diagnoses and classifies the health of the hive's GitHub
// App credentials: whether the installation authenticates, belongs to the
// expected account, holds the permissions the hive relies on, and actually
// covers the repositories the hive is configured to work on.
//
// It was extracted from package main (#7238), where it sat in a 4,789-line
// grab-bag alongside five unrelated concerns and could only be tested from
// inside cmd/hive.
//
// The extraction made one deliberate design change: the App key paths are now
// PARAMETERS (KeyPaths) rather than mutable package-level variables. In
// package main they were vars specifically so tests could repoint them at a
// temp dir -- exactly the "tests monkey-patch shared mutable globals" coupling
// #7238 objects to. Passing them in preserves that flexibility without the
// global, and makes the dependency visible in every signature that has it.
package apphealth

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// KeyPaths locates the two places a spoke's GitHub App private key can live.
// Both are consulted, in the order pkg/github defines.
type KeyPaths struct {
	// Spoke is the PVC path a hub-delivered key lands at, which takes effect
	// over the provisioned mount.
	Spoke string
	// Provisioned is the read-only Kubernetes Secret mount written at
	// provisioning time.
	Provisioned string
}

// Heal self-heals a hive whose github.installation_id
// points at the WRONG account — the failure mode Diagnose
// already detects and reports ("installation N belongs to 'X', not 'Y'"). It
// asks pkg/github to rediscover the installation covering cfg.Project.Org via
// the App JWT and, only on an unambiguous match, adopts it in place and
// persists it so the fix survives a pod restart.
//
// Every failure path is soft and silent-ish: a hive with no App key is not
// App-authenticated (skip), an API error or an ambiguous/absent discovery
// result leaves installation_id exactly as configured so the existing
// "check github.installation_id" banner still stands. It never returns an
// error to the caller and never blocks startup or a heartbeat.
//
// Rediscovery is rate-limited by pkg/github's discovery cache
// (github.InstallationDiscoveryTTL), so calling this from the self-heal tick
// is cheap even when the App is genuinely not installed on the org.
func Heal(ctx context.Context, appAuth *github.AppAuth, cfg *config.Config, logger *slog.Logger) {
	if appAuth == nil || !appAuth.HasKey() || cfg == nil {
		return
	}
	org := cfg.Project.Org
	if org == "" {
		return
	}
	newID, err := appAuth.RediscoverAndAdopt(ctx, org, logger)
	if err != nil {
		logger.Debug("github app installation rediscovery did not adopt a new id",
			"org", org, "error", err)
		return
	}
	if newID == 0 {
		return // already correct, or nothing safe to adopt
	}
	cfg.GitHub.InstallationID = newID
	if err := cfg.Save(); err != nil {
		logger.Error("adopted rediscovered installation_id but failed to persist it — "+
			"it will revert on the next pod restart",
			"installation_id", newID, "error", err)
		return
	}
	logger.Info("persisted rediscovered github app installation_id",
		"installation_id", newID, "org", org)
}

// Diagnose classifies this hive's GitHub App credential state and
// returns both the machine-readable state and banner-ready copy.
//
// It supersedes a substring match on the formatted error ("403"/"401"), which
// could not tell a user-side failure from an operator-side one and so showed
// every hive the same "GitHub App Not Installed" banner. The most damaging
// case that fixes: a spoke holding the WRONG private key (the hub's key push
// has not landed, or delivered a public github.com key to a GitHub Enterprise
// hive) gets `401 A JSON web token could not be decoded`. The user cannot see,
// supply, or correct that key — telling them to install the App or check
// github.installation_id sends them to redo work they already did correctly.
//
// The candidate key paths are passed so a MISSING key is detected without any
// API round-trip at all.
//
// Returns ("", AppStateOK) when App auth is healthy, and ("", state) for a nil
// appAuth (a token-authenticated hive has nothing to check).
func Diagnose(ctx context.Context, appAuth *github.AppAuth, expectedOwner string, keys KeyPaths) (string, github.AppAuthState) {
	d := DiagnoseFull(ctx, appAuth, expectedOwner, keys)
	return d.Message(), d.State
}

// DiagnoseFull is Diagnose without the lossy projection to
// (message, state). Callers that only need the banner should keep using the
// wrapper above; this exists for the one caller that also reports the granted
// Actions and Commit-statuses permissions (#4030), which the projection drops.
func DiagnoseFull(ctx context.Context, appAuth *github.AppAuth, expectedOwner string, keys KeyPaths) github.AppAuthDiagnosis {
	if appAuth == nil {
		return github.AppAuthDiagnosis{State: github.AppStateOK, ExpectedAccount: expectedOwner}
	}
	return appAuth.DiagnoseAppAuth(ctx, expectedOwner, keys.Spoke, keys.Provisioned)
}

// bannerAttempts is how many times ClassifyFailure probes
// before accepting an unclassifiable (AppStateUnknown) verdict. A cold start
// races the cluster's DNS/egress-proxy readiness, so the FIRST App call a pod
// makes is the one most likely to fail for reasons that have nothing to do
// with the App. One retry converts that transient into a correct verdict.
const bannerAttempts = 2

// bannerRetryDelay spaces those attempts. Short enough not to stall
// boot, long enough for an egress proxy or DNS cache to come up.
const bannerRetryDelay = 3 * time.Second

// ClassifyFailure is the SINGLE decision point for "should the GitHub
// App banner be raised?" — used by the boot path, the advisory-digest path and
// the manual Re-check button alike, so those three can never again disagree
// about the same hive.
//
// It exists because they DID disagree. The boot path used to raise the banner
// on a substring match for "403"/"401" in a formatted error string (the exact
// pattern #2224 replaced), set githubAppRequired=true UNCONDITIONALLY, and
// then classify — never lowering the flag again when classification came back
// healthy or inconclusive. Re-check ran the very same Diagnose probe
// but treated an empty diagnosis as success and cleared the banner. Same
// evidence, opposite conclusion: the banner appeared on every cold start whose
// first advisory-issue call blipped, and vanished the moment the user clicked
// Re-check without anything having been fixed.
//
// Two rules make the verdict trustworthy:
//
//  1. Only a state that is genuinely actionable raises the banner. AppStateOK
//     obviously does not, and neither does AppStateUnknown — #2224 defines it
//     as "we could not reach a conclusion", and escalating on it is precisely
//     the false accusation that design was meant to prevent.
//  2. An unknown verdict is retried before it is accepted, so a transient
//     startup network failure is not mistaken for a definitive one.
//
// Returns raise=false with an empty message when the App is fine or when we
// simply cannot tell.
func ClassifyFailure(ctx context.Context, appAuth *github.AppAuth, expectedOwner string, keys KeyPaths, logger *slog.Logger) (raise bool, msg string, state github.AppAuthState) {
	var d github.AppAuthDiagnosis
	for attempt := 1; attempt <= bannerAttempts; attempt++ {
		d = DiagnoseFull(ctx, appAuth, expectedOwner, keys)
		msg, state = d.Message(), d.State
		if state != github.AppStateUnknown {
			break
		}
		if attempt < bannerAttempts {
			logger.Debug("github app classification inconclusive — retrying before accepting a verdict",
				"attempt", attempt, "owner", expectedOwner)
			select {
			case <-ctx.Done():
				return false, "", github.AppStateUnknown
			case <-time.After(bannerRetryDelay):
			}
		}
	}

	// AppStateUnknown must never raise the banner: we did not get an answer
	// from GitHub, and a hive whose App is perfectly healthy would otherwise
	// be told to reinstall it because of a momentary network fault. The
	// self-heal loop and the next eval cycle both re-probe, so deferring the
	// verdict costs nothing but a delay on a hive that IS genuinely broken.
	if state == github.AppStateUnknown {
		logger.Warn("github app state could not be determined — leaving the banner down rather than guessing",
			"owner", expectedOwner)
		return false, "", github.AppStateUnknown
	}

	// #4030: record the Actions and Commit-statuses grants alongside the
	// verdict. These are NOT required of the Hive App and their absence is not
	// a fault — the optional Visual Hive App exists so they never have to be.
	// But an installation that has not approved them is otherwise
	// indistinguishable from one that has, both reporting "ok", which is what
	// would make a half-approved fleet invisible during any later
	// consolidation.
	//
	// It is deliberately emitted for EVERY verdict, including AppStateOK.
	// Gating it on a fault would defeat the purpose: the half-approved
	// installation is the one that looks healthy. It costs no extra API call —
	// the diagnosis above already fetched the installation.
	//
	// Note this runs where verdicts are computed, not on every eval cycle:
	// every caller reaches here from a failed GitHub call or from the
	// dashboard's Re-check. Re-check is therefore the operator-invokable way to
	// read a specific installation's grants.
	// #5774: record the write-path grants on the SAME line, for the same
	// reason and with the same posture. The App migration that blocked every
	// agent PR flow was invisible here because this verdict read Issues and
	// nothing else: a hive that could file issues and could not push a branch
	// reported "ok", and so did a healthy one. Contents/Pull-requests/Workflows
	// are recorded, never enforced — see GrantsAgentPushFlow for why requiring
	// them would misreport the read-only advisory tier — and, like the grants
	// above, they are emitted for EVERY verdict including AppStateOK, because
	// the installation that looks healthy is precisely the one worth counting.
	logger.Info("github app credential verdict",
		"owner", expectedOwner, "state", state.String(),
		"grants", d.ExecutionGrants(),
		"visual_hive_execution_grants", d.GrantsVisualHiveExecution(),
		"push_flow_grants", d.PushFlowGrants(),
		"agent_push_flow_grants", d.GrantsAgentPushFlow())
	if state == github.AppStateOK {
		return false, "", github.AppStateOK
	}
	return true, msg, state
}

// ClassifyWriteForbidden (#2353) is the verdict for a REAL write that
// returned 403 "Resource not accessible by integration". Authentication-only
// health checks (ClassifyFailure / Diagnose) inspect the
// installation's granted PERMISSIONS but never whether the target repo is in
// the installation's `selected` repositories — so they return AppStateOK for a
// repo the App cannot write, and the write failure stayed invisible to health
// (githubAppState=None) or, worse, got hard-overridden into a false "lacks
// Issues: Read & Write" banner.
//
// The rule here keeps attribution honest:
//
//   - If Diagnose finds a genuine, classifiable App-auth problem
//     (wrong installation, missing key, insufficient PERMISSION, etc.), report
//     THAT — it is the real cause and its copy is already accurate.
//   - If Diagnose reports the installation is healthy (AppStateOK:
//     right owner, issues:write granted) OR could not reach a verdict
//     (AppStateUnknown), the write 403 is still real and must NOT be silently
//     healthy. Report AppStateWriteForbidden with copy that names the repo and
//     the likeliest cause (repo not in the installation's selected repos),
//     WITHOUT inventing a permission gap that the diagnosis just proved absent.
//
// It always raises: a write that returned 403 is a genuine, standing failure to
// surface, distinct from the transient/unknown probe failures
// ClassifyFailure guards against.
func ClassifyWriteForbidden(ctx context.Context, appAuth *github.AppAuth, expectedOwner, repo string, keys KeyPaths) (msg string, state github.AppAuthState) {
	diagMsg, diagState := Diagnose(ctx, appAuth, expectedOwner, keys)
	if diagState != github.AppStateOK && diagState != github.AppStateUnknown {
		// A real, classifiable App-auth problem — its message is the accurate
		// one (e.g. wrong-installation, key-missing, or a genuine permission
		// gap). Use it as-is rather than masking it with the repo-scope story.
		return diagMsg, diagState
	}
	// Installation authenticates and (per the diagnosis) holds issues:write, yet
	// the write was forbidden. Attribute it to the one thing the permission
	// check cannot see — repo scope — and name the repo so the fix is concrete.
	d := github.AppAuthDiagnosis{
		State:           github.AppStateWriteForbidden,
		ExpectedAccount: expectedOwner,
		Repo:            repo,
	}
	return d.Message(), github.AppStateWriteForbidden
}

// ClassifyRepoCoverage (#4360) asks the deterministic question the
// other classifiers cannot: does this installation actually COVER the repos
// this hive is configured to work on?
//
// Everything else here reasons from a failed call. Diagnose inspects
// installation-level PERMISSIONS and never repo scope, and
// ClassifyWriteForbidden infers scope from a 403 after a write has
// already failed. Neither can see the case that prompted this: a hive pointed
// at a second repo in the right org, on the right installation, simply not
// ticked in the App's selected repos. GitHub answers 404 for that — the same
// answer it gives for a repo that does not exist — so the read path reported
// "app not installed / no read" and the dashboard blamed an undelivered
// private key that had in fact arrived. The operator was sent to re-upload a
// key, which could not possibly help.
//
// An error here is NOT a verdict. If the listing cannot be fetched the
// credentials themselves are the more likely story, and the existing checks
// tell it better; this returns raise=false and lets them run.
func ClassifyRepoCoverage(ctx context.Context, appAuth *github.AppAuth, org string, repos []string, logger *slog.Logger) (raise bool, msg string, state github.AppAuthState) {
	if appAuth == nil || len(repos) == 0 {
		return false, "", github.AppStateUnknown
	}

	cov, err := appAuth.InstallationCoverage(ctx)
	if err != nil {
		logger.Debug("github app repo coverage: could not list installation repositories — deferring to the credential checks",
			"org", org, "error", err)
		return false, "", github.AppStateUnknown
	}

	missing := cov.Missing(org, repos)
	if len(missing) == 0 {
		return false, "", github.AppStateOK
	}

	// #5774: a coverage miss whose shape is an org TRANSFER gets its own
	// verdict, checked first because the not-covered copy is actively wrong for
	// it. "Tick this repo in the installation's repository access" cannot be
	// followed when the repository has left that account — there is nothing
	// there to tick — and this classifier exists in the first place because
	// sending an operator to a fix that cannot work costs them real debugging
	// time. MovedTo returns nothing unless the shape is unambiguous (see its
	// three clauses), so the not-covered verdict below remains the default.
	if moves := cov.MovedTo(org, repos); len(moves) > 0 {
		d := github.AppAuthDiagnosis{
			State:           github.AppStateRepoMoved,
			ExpectedAccount: org,
			InstallationID:  appAuth.InstallationID(),
			APIURL:          appAuth.APIURL(),
			RepoMoves:       moves,
		}
		logger.Warn("github app repo coverage: configured repositories were transferred to another account",
			"configured_org", org, "now_under", github.MovedOwner(moves), "repos", len(moves))
		return true, d.Message(), github.AppStateRepoMoved
	}

	d := github.AppAuthDiagnosis{
		State:           github.AppStateRepoNotCovered,
		ExpectedAccount: org,
		InstallationID:  appAuth.InstallationID(),
		APIURL:          appAuth.APIURL(),
		Repos:           missing,
	}
	return true, d.Message(), github.AppStateRepoNotCovered
}

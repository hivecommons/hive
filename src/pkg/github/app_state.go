package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// AppAuthState classifies WHY GitHub App authentication is not working, so the
// UI can say the true thing instead of one catch-all "install the App" banner.
//
// The distinction that matters most is between the states a USER can fix
// (install the App, correct installation_id) and the state only an OPERATOR can
// fix (the private key this spoke signs with is missing or belongs to a
// different App). Telling a user to "install the GitHub App" when the operator
// has not delivered a usable key sends them to redo work they already did
// correctly, and — via the journey nudges — can escalate to threatening
// de-provisioning for our own failure.
type AppAuthState int

const (
	// AppStateUnknown means classification was not attempted or could not
	// reach a conclusion (network error, unparseable response). Callers must
	// treat it as "we cannot tell" and never escalate on it.
	AppStateUnknown AppAuthState = iota

	// AppStateOK means the App authenticated and the installation resolved.
	AppStateOK

	// AppStateNotInstalled means the App is genuinely not installed on the
	// target account. USER-ACTIONABLE: install the App.
	AppStateNotInstalled

	// AppStateWrongInstallation means the App authenticated fine, but the
	// installation it resolved belongs to a different account than this hive
	// targets. USER-ACTIONABLE: correct github.installation_id.
	AppStateWrongInstallation

	// AppStateInsufficientPerms means the App is installed on the right
	// account but the org owner has not approved the permissions it needs.
	// USER-ACTIONABLE (by an org owner): approve the permission update.
	AppStateInsufficientPerms

	// AppStateKeyMissing means no App private key is present on this spoke at
	// all. OPERATOR-ACTIONABLE: the hub has not delivered the key.
	AppStateKeyMissing

	// AppStateKeyInvalid means a key IS present but GitHub rejected the JWT it
	// signed — the key does not belong to the App this hive claims to be (the
	// classic symptom of a public github.com key on a GitHub Enterprise hive).
	// OPERATOR-ACTIONABLE: the hub must push the correct key.
	AppStateKeyInvalid

	// AppStateNoAppAssigned means the hive is still running
	// config.PlaceholderAppID — it was provisioned as a placeholder and never
	// assigned a real GitHub App ID. OPERATOR-ACTIONABLE: only the hub can
	// supply the App ID.
	//
	// This is deliberately NOT AppStateNotInstalled. Both leave the hive without
	// working App auth, but they point at opposite fixes, and reporting this one
	// as "not installed" actively misleads: it tells the owner to install the App
	// and set an installation_id, when their installation_id is already correct
	// and no value they can enter will help — the app_id names no App, so the
	// JWT fails before the installation is ever consulted. That misdiagnosis is
	// what cost the owner of the first hive to hit this real debugging time.
	AppStateNoAppAssigned

	// AppStateWriteForbidden (#2353) means the App authenticated, the
	// installation resolved on the RIGHT account, and DiagnoseAppAuth reports
	// the installation grants issues:write — yet a real WRITE attempt returned
	// 403 "Resource not accessible by integration". DiagnoseAppAuth only
	// inspects installation-level PERMISSIONS; it never checks whether the
	// target repo is in the installation's `selected` repositories. So an
	// authentication-and-permission check passes while the write is still
	// forbidden.
	//
	// The most likely cause is exactly that gap: the repo is not included in the
	// App installation's selected repositories, so no granted permission applies
	// to it. It is DISTINCT from AppStateInsufficientPerms — the permission IS
	// granted here, so labelling this "lacks Issues: Read & Write" is a false
	// accusation. USER-ACTIONABLE (by an org owner): add the repo to the App
	// installation (or, if using "all repositories", approve any pending
	// permission update).
	AppStateWriteForbidden

	// AppStateRepoNotCovered (#4360) means the App authenticated on the right
	// account and the installation is healthy, but one or more of the repos
	// this hive is configured to work on are not in the installation's
	// selected repositories.
	//
	// It is the DETERMINISTIC form of what AppStateWriteForbidden infers. That
	// state is reached after a real write returns 403, for whichever single
	// repo something happened to touch; this one is established up front, for
	// every configured repo, by comparing project.repos against
	// GET /installation/repositories. Where the two agree, prefer this one:
	// it names every affected repo rather than the first one to fail, and it
	// does not have to guess, because GitHub answers 404 — not 403 — for a
	// repo an installation cannot see, which is indistinguishable from the
	// repo not existing.
	//
	// USER-ACTIONABLE (by an org owner): tick the repo in the App
	// installation's repository access.
	AppStateRepoNotCovered

	// AppStateRepoMoved (#5774) means the installation is healthy and covers
	// the configured repository — but under a DIFFERENT account than the hive
	// is configured for, because the repository was transferred to another org.
	//
	// It is the transfer-shaped case of AppStateRepoNotCovered, split out
	// because the two need opposite instructions. That state tells an operator
	// to tick the repo in the configured org's installation; after a transfer
	// there is nothing to tick there, because the repository has left that
	// account. Issuing that instruction is not a vaguer version of the right
	// answer, it is a wrong one, and sending an operator to a settings page for
	// an org the repo no longer belongs to is exactly the confident
	// misdirection the deterministic coverage check (#4360) was built to stop.
	//
	// The live case is the kubestellar to hivecommons migration: once the App
	// is installed on the new org while a hive's config still names the old
	// one, every configured repo reads as "not covered".
	//
	// USER-ACTIONABLE: point the hive's configured org at the new account.
	AppStateRepoMoved
)

// String returns the stable wire token for a state. These tokens cross the
// heartbeat to the hub and into both dashboards, so they must not change
// without updating the consumers.
func (s AppAuthState) String() string {
	switch s {
	case AppStateOK:
		return "ok"
	case AppStateNotInstalled:
		return "not-installed"
	case AppStateWrongInstallation:
		return "wrong-installation"
	case AppStateInsufficientPerms:
		return "insufficient-permissions"
	case AppStateKeyMissing:
		return "key-missing"
	case AppStateKeyInvalid:
		return "key-invalid"
	case AppStateNoAppAssigned:
		return "no-app-assigned"
	case AppStateWriteForbidden:
		return "write-forbidden"
	case AppStateRepoNotCovered:
		return "repo-not-covered"
	case AppStateRepoMoved:
		return "repo-moved"
	default:
		return "unknown"
	}
}

// OperatorActionable reports whether this state can ONLY be fixed by whoever
// operates the hub — never by the hive's owner. Warnings for these states must
// not instruct the user to install anything or edit config, and the journey
// nudges must never escalate against them.
func (s AppAuthState) OperatorActionable() bool {
	return s == AppStateKeyMissing || s == AppStateKeyInvalid || s == AppStateNoAppAssigned
}

// UserActionable reports whether the hive's owner (or their org owner) can fix
// this state themselves. Only these states justify a "do something" banner.
func (s AppAuthState) UserActionable() bool {
	switch s {
	case AppStateNotInstalled, AppStateWrongInstallation, AppStateInsufficientPerms,
		AppStateWriteForbidden, AppStateRepoNotCovered, AppStateRepoMoved:
		return true
	default:
		return false
	}
}

// ParseAppAuthState turns a wire token back into a state. Unrecognised input
// yields AppStateUnknown so an older spoke reporting a token we do not know
// degrades to "cannot tell" rather than to a false accusation.
func ParseAppAuthState(s string) AppAuthState {
	switch strings.TrimSpace(s) {
	case "ok":
		return AppStateOK
	case "not-installed":
		return AppStateNotInstalled
	case "wrong-installation":
		return AppStateWrongInstallation
	case "insufficient-permissions":
		return AppStateInsufficientPerms
	case "key-missing":
		return AppStateKeyMissing
	case "key-invalid":
		return AppStateKeyInvalid
	case "no-app-assigned":
		return AppStateNoAppAssigned
	case "repo-not-covered":
		return AppStateRepoNotCovered
	case "write-forbidden":
		return AppStateWriteForbidden
	case "repo-moved":
		return AppStateRepoMoved
	default:
		return AppStateUnknown
	}
}

// jwtUndecodableMarker is the message GitHub returns on
// POST /app/installations/{id}/access_tokens (and GET /app/installations/{id})
// when the JWT presented was signed by a key that does not belong to the App
// named in its `iss` claim. It is the definitive signature of a key/App
// mismatch, and is what distinguishes a broken KEY from a wrong INSTALLATION:
// a wrong installation_id authenticates fine (the JWT decodes) and fails later
// with 403/404 on the installation itself.
const jwtUndecodableMarker = "a json web token could not be decoded"

// Required issues permission for a hive to do any GitHub work.
const requiredIssuesPerm = "write"

// classifyAPIError maps a go-github error to an AppAuthState using the ACTUAL
// HTTP status of the response, never a substring of a formatted error string.
//
// The three failure statuses mean genuinely different things on the App
// endpoints:
//
//   - 401 Unauthorized — the JWT itself was rejected. On these endpoints the
//     only credential is the App JWT, so a 401 means the signature did not
//     verify against the App's registered public key: the private key is
//     wrong. GitHub says "A JSON web token could not be decoded". This is
//     OPERATOR-side; the user has no way to supply or correct this key.
//   - 404 Not Found — the JWT verified (GitHub knows which App we are) but
//     there is no such installation visible to this App. The App is not
//     installed on the target.
//   - 403 Forbidden — the JWT verified and the installation exists, but this
//     App may not act on it ("Resource not accessible by integration"). That
//     is a permissions/installation-mismatch problem, not a key problem.
//
// A nil resp (transport failure, DNS, timeout) yields AppStateUnknown: we did
// not get an answer from GitHub and must not guess.
func classifyAPIError(err error, resp *gh.Response) AppAuthState {
	if err == nil {
		return AppStateOK
	}

	status := 0
	if resp != nil && resp.Response != nil {
		status = resp.StatusCode
	}
	// go-github surfaces the parsed body on *gh.ErrorResponse even when the
	// caller discarded the *gh.Response, so prefer it when we have one.
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		status = ghErr.Response.StatusCode
	}

	switch status {
	case http.StatusUnauthorized:
		// A 401 on an App endpoint is always a signing failure. The message
		// check only refines the log/telemetry story; the status alone is
		// sufficient to conclude the key is at fault.
		return AppStateKeyInvalid
	case http.StatusNotFound:
		return AppStateNotInstalled
	case http.StatusForbidden:
		// 403 can also be a rate limit, which is transient and must never be
		// reported as a credential problem.
		if isRateLimitErr(err) {
			return AppStateUnknown
		}
		return AppStateWrongInstallation
	}

	// No usable status. Fall back to the JWT marker, which some proxies
	// surface without preserving the status code.
	if strings.Contains(strings.ToLower(err.Error()), jwtUndecodableMarker) {
		return AppStateKeyInvalid
	}
	return AppStateUnknown
}

// isRateLimitErr reports whether an error is a GitHub rate/abuse limit. These
// are transient and must not latch a credential warning.
func isRateLimitErr(err error) bool {
	var rl *gh.RateLimitError
	if errors.As(err, &rl) {
		return true
	}
	var ab *gh.AbuseRateLimitError
	return errors.As(err, &ab)
}

// AppAuthDiagnosis is the full result of checking this hive's GitHub App
// credentials: what state they are in, and the supporting detail a caller
// needs to compose accurate copy.
type AppAuthDiagnosis struct {
	State AppAuthState
	// Account is the login the installation actually belongs to, when known.
	Account string
	// ExpectedAccount is the org/user this hive targets.
	ExpectedAccount string
	// InstallationID is the installation this hive is configured to use.
	InstallationID int64
	// IssuesPerm is the granted issues permission, when the installation
	// resolved.
	IssuesPerm string
	// ActionsPerm and StatusesPerm are the granted Actions and Commit-statuses
	// permissions, when the installation resolved. Empty means GitHub reported
	// no grant.
	//
	// They are RECORDED, never enforced (#4030). The Hive App holds neither at
	// write by design, so requiring them here would flip every healthy
	// installation to AppStateInsufficientPerms — see the note on
	// GrantsVisualHiveExecution for why observing them still matters.
	ActionsPerm  string
	StatusesPerm string
	// ContentsPerm, PullRequestsPerm and WorkflowsPerm are the granted
	// Contents, Pull-requests and Workflows permissions, when the installation
	// resolved. Empty means GitHub reported no grant.
	//
	// These are the grants the AGENT PR FLOW actually runs on, and until #5774
	// none of them was even read. A branch push needs Contents: write; opening
	// the PR needs Pull requests: write; a push whose diff touches
	// .github/workflows/** additionally needs Workflows: write or GitHub
	// rejects the ref update outright. DiagnoseAppAuth classified solely on
	// Issues, so an installation that could file issues and could not push a
	// single branch reported "ok" — which is how an org migration took out
	// every agent PR flow in the fleet while the credential surface stayed
	// green.
	//
	// Like ActionsPerm/StatusesPerm they are RECORDED, never enforced. See
	// GrantsAgentPushFlow for why observing without requiring is the right
	// posture here.
	ContentsPerm     string
	PullRequestsPerm string
	WorkflowsPerm    string
	// Repo, when set, is the repository whose write attempt was forbidden.
	// Only AppStateWriteForbidden (#2353) populates it, so the banner can name
	// the exact repo the operator must add to the App installation.
	Repo string
	// RepoMoves, when set, are the configured repositories the installation
	// covers under a DIFFERENT account. Only AppStateRepoMoved (#5774)
	// populates it, so the copy can name where each repository actually lives
	// now instead of pointing at the account it left.
	RepoMoves []RepoMove
	// Repos, when set, are the configured repositories the installation does
	// not cover, in "owner/name" form. Only AppStateRepoNotCovered (#4360)
	// populates it, so the banner can name every repo that needs ticking
	// rather than only the first one to fail a write.
	Repos []string
	// APIURL is the GitHub API base this hive talks to. Carried so copy can
	// build a web link that is correct on GitHub Enterprise too, instead of
	// hardcoding github.com.
	APIURL string
	// Err is the underlying error, for logs. Never rendered to a user
	// verbatim, because it can carry raw API text.
	Err error
}

// grantNone is what ExecutionGrants renders for a permission GitHub reported
// no grant for. "" would be ambiguous in a log line against a field that was
// simply never populated, and this string is only ever read by humans.
const grantNone = "none"

// grantWrite is the permission level GitHub reports for a write grant. It is
// spelled separately from requiredIssuesPerm, which happens to hold the same
// string: that one names a REQUIREMENT of the Hive App, and reusing it for the
// Visual Hive grants would read as though those were required too, which is
// exactly what they are not.
const grantWrite = "write"

// GrantsVisualHiveExecution reports whether this installation currently grants
// the two write permissions the optional Visual Hive App needs (#4030): Actions
// (to dispatch the installed workflow) and Commit statuses (to publish the
// provenance-bound setup authorization status).
//
// It answers a QUESTION; it does not impose a requirement. False is the normal,
// healthy answer for an ordinary Hive installation and must never be treated as
// a fault: the Hive App deliberately holds Actions at read and requests no
// Commit-statuses grant at all, and the whole reason #4030 registers a separate
// App is that turning Visual Hive on must not widen permissions for the
// installations that will never use it.
//
// What it is FOR is the consolidation decision #4030 defers. Folding these
// grants into the Hive App would make every installation re-approve, and
// GitHub keeps an App on its OLD permissions until an org owner accepts — so a
// fleet can sit half-approved indefinitely with nothing surfacing it. Before
// this, DiagnoseAppAuth read only the issues permission, so those two grants
// were not merely unenforced, they were unobservable: every installation
// reported the same "ok" whether or not the widened permissions had landed.
// Recording them is what makes a half-approved fleet countable.
//
// Metadata is not consulted. GitHub grants it implicitly to every installation
// that holds any repository permission, so it cannot discriminate between the
// approved and unapproved halves of a fleet.
func (d AppAuthDiagnosis) GrantsVisualHiveExecution() bool {
	return d.ActionsPerm == grantWrite && d.StatusesPerm == grantWrite
}

// ExecutionGrants renders the two Visual Hive execution grants as a stable,
// log-safe "actions=<grant> statuses=<grant>" pair.
//
// It carries only permission LEVELS ("read"/"write"/"none") that GitHub already
// reports, never account names, repository names, or error text, so it is safe
// to log unconditionally at info level — which is the point: a caller that
// emitted it only for a faulty verdict would never emit it for the case that
// motivated it, an installation which has not approved a permission update and
// therefore still classifies as healthy.
func (d AppAuthDiagnosis) ExecutionGrants() string {
	grant := func(p string) string {
		if strings.TrimSpace(p) == "" {
			return grantNone
		}
		return p
	}
	return fmt.Sprintf("actions=%s statuses=%s", grant(d.ActionsPerm), grant(d.StatusesPerm))
}

// GrantsAgentPushFlow reports whether this installation currently grants the
// two permissions an agent needs to open a pull request under its own identity:
// Contents (to push the branch) and Pull requests (to open the PR).
//
// It answers a QUESTION; it does not impose a requirement, and it is not part
// of the classification. Enforcing it would be wrong in both directions. A
// read-only advisory hive — the L2 guide tier, which by design receives a token
// scoped to Contents: read and never pushes anything — would flip to
// AppStateInsufficientPerms for operating exactly as intended. And a hive that
// legitimately holds both grants can still be unable to push for a reason no
// permission bit describes, which is what AppStateRepoNotCovered and
// AppStateRepoMoved exist to say.
//
// What it is FOR is making the write path COUNTABLE. #5774's failure mode was
// not that the hub misjudged a permission — it was that nothing on the
// credential surface described the write path at all. Issue creation kept
// working, every branch push died, and DiagnoseAppAuth reported "ok" for both,
// because it read Issues and nothing else. An installation that cannot push is
// now distinguishable from one that can, before a single agent tries and fails.
//
// This mirrors GrantsVisualHiveExecution's posture deliberately: observe the
// grant, log it on every verdict including the healthy one, and let the fault
// states stay the ones that are genuinely faults.
func (d AppAuthDiagnosis) GrantsAgentPushFlow() bool {
	return d.ContentsPerm == grantWrite && d.PullRequestsPerm == grantWrite
}

// PushFlowGrants renders the three write-path grants as a stable, log-safe
// "contents=<grant> pull_requests=<grant> workflows=<grant>" triple.
//
// Like ExecutionGrants it carries only permission LEVELS that GitHub already
// reports — never account names, repository names, or error text — so it is
// safe to log unconditionally at info level. That is the point: emitting it
// only on a faulty verdict would never emit it for the case that motivated it,
// an installation which reports "ok" and cannot push.
//
// Workflows is reported alongside the two GrantsAgentPushFlow consults, and
// deliberately not folded into that predicate. It is needed only by a push
// whose diff touches .github/workflows/**, so requiring it would misreport
// every installation that never edits a workflow; but when a push is rejected
// with "refusing to allow a GitHub App to create or update workflow", this
// field is the one that explains it, and it is free on the same API call.
func (d AppAuthDiagnosis) PushFlowGrants() string {
	grant := func(p string) string {
		if strings.TrimSpace(p) == "" {
			return grantNone
		}
		return p
	}
	return fmt.Sprintf("contents=%s pull_requests=%s workflows=%s",
		grant(d.ContentsPerm), grant(d.PullRequestsPerm), grant(d.WorkflowsPerm))
}

// keyFileReadable reports whether path exists and holds non-empty content.
// This is the cheap, pre-flight detection of AppStateKeyMissing: it needs no
// API round-trip and is the clearest possible signal that the operator's key
// push has not landed.
func keyFileReadable(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Size() > 0
}

// DiagnoseAppAuth classifies this hive's GitHub App credential state against
// expectedOwner (the org/user the hive targets).
//
// keyPaths are candidate private-key locations, checked in order. When NONE of
// them holds content, the result is AppStateKeyMissing WITHOUT making any API
// call — a missing key cannot possibly authenticate, and a failed round-trip
// would only produce a less specific answer. Pass no paths to skip the
// pre-flight and rely on the API classification alone.
//
// A nil receiver, or one with no loaded key, is AppStateKeyMissing: a hive
// that cannot sign a JWT has no credentials, which is an operator problem.
func (a *AppAuth) DiagnoseAppAuth(ctx context.Context, expectedOwner string, keyPaths ...string) AppAuthDiagnosis {
	d := AppAuthDiagnosis{ExpectedAccount: expectedOwner}

	if a == nil || !a.HasKey() {
		d.State = AppStateKeyMissing
		return d
	}
	d.InstallationID = a.InstallationID()
	d.APIURL = a.APIURL()

	// Cheap pre-flight: if we were given key locations and none of them holds
	// content, the key is missing. No API call needed.
	if len(keyPaths) > 0 {
		anyReadable := false
		for _, p := range keyPaths {
			if keyFileReadable(p) {
				anyReadable = true
				break
			}
		}
		if !anyReadable {
			d.State = AppStateKeyMissing
			return d
		}
	}

	jwtToken, err := a.generateJWT()
	if err != nil {
		// We hold a key but cannot sign with it — the key itself is unusable.
		d.State = AppStateKeyInvalid
		d.Err = fmt.Errorf("generating JWT: %w", err)
		return d
	}

	jwtClient := newJWTClient(jwtToken, a.apiURL)
	inst, resp, err := jwtClient.Apps.GetInstallation(ctx, a.installationID)
	if err != nil {
		d.State = classifyAPIError(err, resp)
		d.Err = err
		return d
	}
	if inst == nil {
		d.State = AppStateUnknown
		d.Err = errors.New("github returned no installation and no error")
		return d
	}

	d.Account = inst.GetAccount().GetLogin()
	perms := inst.GetPermissions()
	d.IssuesPerm = perms.GetIssues()
	// Recorded for observability only — the classification below is unchanged
	// and still turns solely on issues. See AppAuthDiagnosis.ActionsPerm.
	d.ActionsPerm = perms.GetActions()
	d.StatusesPerm = perms.GetStatuses()
	// #5774: the write-path grants. Recorded on the same call that already
	// fetched the installation, so this costs nothing and makes an
	// installation that cannot push distinguishable from one that can. The
	// classification below is deliberately unchanged — see GrantsAgentPushFlow.
	d.ContentsPerm = perms.GetContents()
	d.PullRequestsPerm = perms.GetPullRequests()
	d.WorkflowsPerm = perms.GetWorkflows()

	if expectedOwner != "" && d.Account != "" && !strings.EqualFold(d.Account, expectedOwner) {
		d.State = AppStateWrongInstallation
		return d
	}
	if d.IssuesPerm != requiredIssuesPerm {
		d.State = AppStateInsufficientPerms
		return d
	}
	d.State = AppStateOK
	return d
}

// Message returns accurate, non-blaming, user-facing copy for a diagnosis.
//
// The contract for operator-side states is strict: say what is wrong, say that
// an administrator fixes it, and do NOT ask the user to install anything or
// edit any config. A user who has done everything right must never be sent
// back to redo it.
func (d AppAuthDiagnosis) Message() string {
	owner := d.ExpectedAccount
	if strings.TrimSpace(owner) == "" {
		owner = "this hive's organization"
	}

	switch d.State {
	case AppStateOK:
		return ""

	case AppStateKeyMissing:
		return "This hive's GitHub App credentials have not been provisioned yet. " +
			"The App private key is supplied by the hub operator, not by you — there is nothing to install or configure on your side. " +
			"Agents will start working automatically once the key arrives. If this persists, contact your hub administrator."

	case AppStateKeyInvalid:
		return "This hive's GitHub App credentials are not valid for the GitHub instance it targets: " +
			"the private key it holds does not match the App it authenticates as, so GitHub rejects its signed token. " +
			"This key is distributed by the hub operator and cannot be supplied or corrected by you — " +
			"your App installation and installation ID are not at fault. Please contact your hub administrator to have the correct key pushed to this hive."

	case AppStateNoAppAssigned:
		return "This hive was never assigned a GitHub App ID. It was provisioned as a placeholder and still carries the " +
			"placeholder app_id, which does not name a real GitHub App — so it cannot authenticate no matter which " +
			"installation ID is set here. Your App installation and installation ID are NOT at fault, and setting an " +
			"installation ID will not resolve this. Only the hub operator can assign the real App ID. " +
			"Please contact your hub administrator."

	case AppStateNotInstalled:
		return fmt.Sprintf("The KubeStellar Hive GitHub App is not installed on %s. "+
			"Until it is, agents cannot read issues or open pull requests. Install it from the link on this banner.", owner)

	case AppStateWrongInstallation:
		acct := d.Account
		if strings.TrimSpace(acct) == "" {
			acct = "a different account"
		}
		return fmt.Sprintf("This hive is authenticating as GitHub App installation %d, which belongs to '%s' — not '%s'. "+
			"Check github.installation_id in the hive config.", d.InstallationID, acct, owner)

	case AppStateInsufficientPerms:
		granted := d.IssuesPerm
		if granted == "" {
			granted = "none"
		}
		acct := d.Account
		if strings.TrimSpace(acct) == "" {
			acct = owner
		}
		return fmt.Sprintf("The GitHub App is installed for '%s' but granted Issues: %s (Issues: Read & Write required). "+
			"The org owner must approve the updated permissions at the app installation settings page.", acct, granted)

	case AppStateWriteForbidden:
		// #2353: authentication and installation-level permissions both check
		// out, but a real write returned 403. Do NOT claim a missing
		// permission — the permission IS granted. The likeliest gap is repo
		// scope: this repo is not in the App installation's selected repos.
		repo := strings.TrimSpace(d.Repo)
		if repo == "" {
			repo = "this repository"
		}
		return fmt.Sprintf("The GitHub App is installed and authenticated for '%s' and holds Issues: Read & Write, "+
			"but a write to %s returned 403 (Resource not accessible by integration). The most likely cause is that "+
			"%s is not included in the App installation's selected repositories — add it to the installation "+
			"(or, if a permission update is pending, approve it) at the app installation settings page.", owner, repo, repo)

	case AppStateRepoNotCovered:
		// #4360. Everything about the credentials is fine: right App, right
		// org, valid key, healthy installation. The only thing wrong is which
		// repositories that installation was ticked for. Say exactly that, and
		// name them — the previous signal for this shape ("the key has not
		// reached this spoke") pointed at a re-upload that could not possibly
		// help.
		repos := strings.Join(d.Repos, ", ")
		if repos == "" {
			repos = "one or more configured repositories"
		}
		msg := fmt.Sprintf("The GitHub App is installed and authenticated for '%s', but its installation does not include: %s. "+
			"Nothing is wrong with the App, the organization, or the private key — these repositories are simply not "+
			"ticked in the installation's repository access, so every call against them returns 404. "+
			"Add them under the org's App configuration (Settings → Applications → Configure → Repository access).", owner, repos)
		if link := d.InstallationSettingsURL(); link != "" {
			msg += " " + link
		}
		return msg

	case AppStateRepoMoved:
		// #5774. Nothing is wrong with the App, the key, the installation, or
		// the repository access — the repository simply lives somewhere else
		// now. Say where, and do NOT reuse AppStateRepoNotCovered's "tick the
		// repo in this org's installation" instruction: there is nothing to
		// tick under an account the repository has left, and sending an
		// operator there costs them the same debugging time the coverage check
		// was written to give back.
		newOwner := MovedOwner(d.RepoMoves)
		var pairs []string
		for _, m := range d.RepoMoves {
			pairs = append(pairs, fmt.Sprintf("%s is now %s", m.Configured, m.CoveredAs))
		}
		detail := strings.Join(pairs, "; ")
		if detail == "" {
			detail = "the configured repositories now live under a different account"
		}
		msg := fmt.Sprintf("This hive is configured for '%s', but its GitHub App installation covers those repositories under a different account: %s. "+
			"The App, its private key, and the installation are all healthy — the repositories were transferred, so there is nothing to add under '%s'.",
			owner, detail, owner)
		if newOwner != "" {
			msg += fmt.Sprintf(" Point this hive's configured organization at '%s'.", newOwner)
		} else {
			msg += " Point this hive's configured organization at the account shown above."
		}
		return msg

	default:
		return "Could not verify this hive's GitHub App credentials. This is usually transient — " +
			"the check will run again automatically. No action is needed from you right now."
	}
}

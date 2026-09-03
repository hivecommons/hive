package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// ============================================================================
// #5774 — org transfer, and the write-path grants that were never observed
// ============================================================================

// TestMovedTo_LiveHivecommonsCase is the shape from #5774.
//
// The App is installed on the account the repository now lives under, while the
// hive's config still names the account it left. Every configured repo reads as
// "not covered", and the pre-existing verdict would send an operator to tick a
// repository in an org that no longer holds it.
//
// The test asserts BOTH halves deliberately: Missing still reports the repo, so
// the old verdict genuinely would have fired, and MovedTo reports where it went.
// Asserting only the second would leave it unclear whether this check changes
// any answer at all.
func TestMovedTo_LiveHivecommonsCase(t *testing.T) {
	const configuredOrg = "kubestellar"
	cov := InstallationCoverage{Repos: map[string]struct{}{
		"hivecommons/hive": {},
	}}
	configured := []string{"hive"}

	missing := cov.Missing(configuredOrg, configured)
	if !reflect.DeepEqual(missing, []string{"kubestellar/hive"}) {
		t.Fatalf("Missing = %v, want [kubestellar/hive] — without this the moved verdict would be changing nothing", missing)
	}

	moves := cov.MovedTo(configuredOrg, configured)
	want := []RepoMove{{Configured: "kubestellar/hive", CoveredAs: "hivecommons/hive"}}
	if !reflect.DeepEqual(moves, want) {
		t.Fatalf("MovedTo = %+v, want %+v", moves, want)
	}
	if got := MovedOwner(moves); got != "hivecommons" {
		t.Errorf("MovedOwner = %q, want %q", got, "hivecommons")
	}
}

// TestMovedTo_RefusesEveryAmbiguousShape pins each of the three clauses
// individually, so a regression names WHICH guard was dropped rather than only
// changing an aggregate.
//
// Every row here is a case where the answer "your repository was transferred to
// X" would be a guess. The whole value of this classifier over the 403/404
// inference it sits next to is that it does not guess; a row that starts
// returning a move is a row where it started.
func TestMovedTo_RefusesEveryAmbiguousShape(t *testing.T) {
	cases := []struct {
		name       string
		cov        InstallationCoverage
		org        string
		configured []string
		why        string
	}{
		{
			name: "the configured owner is still reachable",
			cov: InstallationCoverage{Repos: map[string]struct{}{
				"kubestellar/other": {},
				"hivecommons/hive":  {},
			}},
			org:        "kubestellar",
			configured: []string{"hive"},
			why:        "the installation still covers repos under the configured owner, so one missing repo is an ordinary unticked-scope gap — repo-not-covered's story, not a transfer",
		},
		{
			name: "two accounts own a repo with that name",
			cov: InstallationCoverage{Repos: map[string]struct{}{
				"hivecommons/hive": {},
				"someoneelse/hive": {},
			}},
			org:        "kubestellar",
			configured: []string{"hive"},
			why:        "picking one of two candidates would be a coin flip presented as a diagnosis",
		},
		{
			name: "nothing covered carries that name",
			cov: InstallationCoverage{Repos: map[string]struct{}{
				"hivecommons/console": {},
			}},
			org:        "kubestellar",
			configured: []string{"hive"},
			why:        "there is no candidate destination at all — this is simply not covered",
		},
		{
			name: "the listing was truncated",
			cov: InstallationCoverage{
				Repos:     map[string]struct{}{"hivecommons/hive": {}},
				Truncated: true,
			},
			org:        "kubestellar",
			configured: []string{"hive"},
			why:        "absence from a partial listing proves nothing, the same rule Missing already follows",
		},
		{
			name:       "no configured owner",
			cov:        InstallationCoverage{Repos: map[string]struct{}{"hivecommons/hive": {}}},
			org:        "   ",
			configured: []string{"hive"},
			why:        "with no owner there is no 'moved from' to reason about",
		},
		{
			name:       "the repo is covered where it was configured",
			cov:        InstallationCoverage{Repos: map[string]struct{}{"kubestellar/hive": {}}},
			org:        "kubestellar",
			configured: []string{"hive"},
			why:        "nothing is missing, so nothing moved",
		},
		{
			name:       "no coverage at all",
			cov:        InstallationCoverage{Repos: map[string]struct{}{}},
			org:        "kubestellar",
			configured: []string{"hive"},
			why:        "an installation that covers nothing tells us nothing about where a repo went",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if moves := tc.cov.MovedTo(tc.org, tc.configured); len(moves) != 0 {
				t.Errorf("MovedTo = %+v, want none — %s", moves, tc.why)
			}
		})
	}
}

// TestMovedTo_ReportsEveryConfiguredRepoThatMoved asserts a multi-repo hive
// gets every move named, sorted, and deduplicated. An operator repointing a
// hive needs the whole list, not the first entry to fail.
func TestMovedTo_ReportsEveryConfiguredRepoThatMoved(t *testing.T) {
	cov := InstallationCoverage{Repos: map[string]struct{}{
		"hivecommons/hive":    {},
		"hivecommons/console": {},
		"hivecommons/pluk":    {},
	}}
	// "hive" appears twice — once bare, once fully qualified — and one
	// configured repo has no destination at all.
	moves := cov.MovedTo("kubestellar", []string{"pluk", "hive", "kubestellar/hive", "vanished"})

	want := []RepoMove{
		{Configured: "kubestellar/hive", CoveredAs: "hivecommons/hive"},
		{Configured: "kubestellar/pluk", CoveredAs: "hivecommons/pluk"},
	}
	if !reflect.DeepEqual(moves, want) {
		t.Fatalf("MovedTo = %+v, want %+v (sorted, deduplicated, and silent about the repo with no destination)", moves, want)
	}
	if got := MovedOwner(moves); got != "hivecommons" {
		t.Errorf("MovedOwner = %q, want hivecommons", got)
	}
}

// TestMovedOwner_RequiresAgreement asserts copy that names ONE destination org
// is never composed from a set that names several.
func TestMovedOwner_RequiresAgreement(t *testing.T) {
	if got := MovedOwner(nil); got != "" {
		t.Errorf("MovedOwner(nil) = %q, want empty", got)
	}
	split := []RepoMove{
		{Configured: "old/hive", CoveredAs: "hivecommons/hive"},
		{Configured: "old/pluk", CoveredAs: "elsewhere/pluk"},
	}
	if got := MovedOwner(split); got != "" {
		t.Errorf("MovedOwner = %q for moves pointing at two different accounts, want empty", got)
	}
}

// TestRepoMovedMessage_DoesNotSendTheOperatorBackToTheOldOrg is the copy test
// this state exists for.
//
// AppStateRepoNotCovered's remedy — add the repository under the org's App
// configuration, repository access — is not a weaker version of the right
// answer here, it is an impossible one: the repository has left that account,
// so there is nothing there to tick. Reusing that copy costs an operator the
// same debugging time the deterministic coverage check was written to give
// back. The message must name the destination and must not carry the
// not-covered instruction.
func TestRepoMovedMessage_DoesNotSendTheOperatorBackToTheOldOrg(t *testing.T) {
	d := AppAuthDiagnosis{
		State:           AppStateRepoMoved,
		ExpectedAccount: "kubestellar",
		InstallationID:  42980,
		APIURL:          "https://api.github.com/",
		RepoMoves: []RepoMove{
			{Configured: "kubestellar/hive", CoveredAs: "hivecommons/hive"},
		},
	}
	msg := d.Message()

	for _, want := range []string{"kubestellar/hive", "hivecommons/hive", "hivecommons", "transferred"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Message() = %q, want it to contain %q", msg, want)
		}
	}
	// The not-covered remedy, verbatim fragments of it, must not appear.
	for _, lack := range []string{"repository access", "Repository access", "not ticked", "Add them under"} {
		if strings.Contains(msg, lack) {
			t.Errorf("Message() = %q, must NOT contain %q — the repository has left that account, so this instruction cannot be followed", msg, lack)
		}
	}
	// It must EXONERATE the credentials rather than merely staying quiet about
	// them. The live misdiagnosis this whole classifier family exists to
	// prevent (#4360) told an operator the private key had not reached the
	// spoke; the key had arrived, and re-uploading it could never help. Saying
	// plainly that the key is fine is what stops the next reader re-deriving
	// that theory from a message that simply omits it.
	if !strings.Contains(msg, "healthy") {
		t.Errorf("Message() = %q, want it to state the App, key and installation are healthy — silence about them invites the key-re-upload misdiagnosis", msg)
	}
	// ...and it must never issue an instruction that points at the
	// credentials, which are not what is wrong here.
	for _, lack := range []string{"hub administrator", "re-upload", "not installed", "installation_id"} {
		if strings.Contains(msg, lack) {
			t.Errorf("Message() = %q, must NOT contain %q — nothing about the credentials is at fault in this state", msg, lack)
		}
	}
	if !d.State.UserActionable() {
		t.Error("repo-moved is fixed by repointing the hive's configured org — it is the user's to fix")
	}
	if d.State.OperatorActionable() {
		t.Error("repo-moved must not be operator-actionable: no key re-upload can help, and claiming otherwise is the misattribution this state exists to prevent")
	}
}

// TestRepoMovedMessage_SurvivesADisagreeingMoveSet asserts the copy degrades to
// "the account shown above" rather than naming a destination it cannot single
// out. A confidently wrong org name is worse than none.
func TestRepoMovedMessage_SurvivesADisagreeingMoveSet(t *testing.T) {
	d := AppAuthDiagnosis{
		State:           AppStateRepoMoved,
		ExpectedAccount: "kubestellar",
		RepoMoves: []RepoMove{
			{Configured: "kubestellar/hive", CoveredAs: "hivecommons/hive"},
			{Configured: "kubestellar/pluk", CoveredAs: "elsewhere/pluk"},
		},
	}
	msg := d.Message()
	if strings.Contains(msg, "Point this hive's configured organization at 'hivecommons'") {
		t.Errorf("Message() = %q, must not name one of two disagreeing destinations as THE answer", msg)
	}
	for _, want := range []string{"hivecommons/hive", "elsewhere/pluk"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Message() = %q, want it to name %q so the operator can see both", msg, want)
		}
	}
}

// installationPermsServer serves GET /app/installations/{id} with the given
// account login and permissions map.
func installationPermsServer(t *testing.T, account string, perms map[string]string) *httptest.Server {
	t.Helper()
	body := map[string]any{
		"id":          42980,
		"account":     map[string]any{"login": account},
		"permissions": perms,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encoding installation body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDiagnoseAppAuth_RecordsWritePathGrants is the observability half of
// #5774.
//
// The migration took out every agent PR flow in the fleet while this surface
// stayed green, because the diagnosis read the Issues permission and nothing
// else: a hive that could file issues and could not push a single branch
// reported exactly what a healthy one reported. The grants a push actually runs
// on are now recorded, so the two are distinguishable BEFORE an agent tries.
//
// The test asserts the classification is UNCHANGED at the same time. Enforcing
// these would flip the read-only advisory tier — which holds Contents: read by
// design and never pushes — into a fault state for working as intended.
func TestDiagnoseAppAuth_RecordsWritePathGrants(t *testing.T) {
	cases := []struct {
		name       string
		perms      map[string]string
		wantPush   bool
		wantState  AppAuthState
		wantRender string
	}{
		{
			name: "a healthy PR-flow installation",
			perms: map[string]string{
				"issues": "write", "contents": "write", "pull_requests": "write", "workflows": "write",
			},
			wantPush:   true,
			wantState:  AppStateOK,
			wantRender: "contents=write pull_requests=write workflows=write",
		},
		{
			name: "issues only — files issues, cannot push a branch",
			perms: map[string]string{
				"issues": "write",
			},
			wantPush: false,
			// The classification MUST still be ok: this is exactly the shape
			// that reported healthy through the migration, and flipping it to a
			// fault would also condemn the read-only advisory tier.
			wantState:  AppStateOK,
			wantRender: "contents=none pull_requests=none workflows=none",
		},
		{
			name: "the advisory tier: contents read, no push",
			perms: map[string]string{
				"issues": "write", "contents": "read",
			},
			wantPush:   false,
			wantState:  AppStateOK,
			wantRender: "contents=read pull_requests=none workflows=none",
		},
		{
			name: "can push but cannot open the PR",
			perms: map[string]string{
				"issues": "write", "contents": "write",
			},
			wantPush:   false,
			wantState:  AppStateOK,
			wantRender: "contents=write pull_requests=none workflows=none",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := installationPermsServer(t, "kubestellar", tc.perms)
			auth := testAuth(t, srv.URL)
			d := auth.DiagnoseAppAuth(context.Background(), "kubestellar")

			if d.State != tc.wantState {
				t.Fatalf("State = %s, want %s — recording the write-path grants must not change the classification", d.State, tc.wantState)
			}
			if got := d.GrantsAgentPushFlow(); got != tc.wantPush {
				t.Errorf("GrantsAgentPushFlow() = %v, want %v (contents=%q pull_requests=%q)",
					got, tc.wantPush, d.ContentsPerm, d.PullRequestsPerm)
			}
			if got := d.PushFlowGrants(); got != tc.wantRender {
				t.Errorf("PushFlowGrants() = %q, want %q", got, tc.wantRender)
			}
		})
	}
}

// TestPushFlowGrants_CarriesNoIdentifyingText asserts the renderer is safe to
// log unconditionally at info level, which is the property that makes it
// emittable on the HEALTHY verdict — the one that motivated it.
func TestPushFlowGrants_CarriesNoIdentifyingText(t *testing.T) {
	d := AppAuthDiagnosis{
		ExpectedAccount:  "kubestellar",
		Account:          "hivecommons",
		Repo:             "kubestellar/secret-repo",
		ContentsPerm:     "write",
		PullRequestsPerm: "write",
	}
	got := d.PushFlowGrants()
	for _, leak := range []string{"kubestellar", "hivecommons", "secret-repo"} {
		if strings.Contains(got, leak) {
			t.Errorf("PushFlowGrants() = %q leaks %q — it must carry only permission levels", got, leak)
		}
	}
}

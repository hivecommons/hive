package advisory

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

func TestIsAppAuthFinding(t *testing.T) {
	tests := []struct {
		title string
		want  bool
	}{
		// The exact finding from #2575, and close phrasings agents use for
		// the hive's own App access.
		{"Insufficient repo permissions", true},
		{"insufficient permissions for the GitHub App", true},
		{"Repo permissions insufficient to open issues", true},
		{"GitHub App not installed on the org", true},
		{"GitHub App lacks Issues write access", true},
		{"403 Resource not accessible by integration", true},

		// Code/content findings that mention permissions must NEVER match —
		// closing them would silently retire a real finding.
		{"Workflow permissions overly broad in ci.yml", false},
		{"Add permission checks to the admin API", false},
		{"SQL injection in login handler", false},
		{"Fix flaky scheduler test", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsAppAuthFinding(tt.title); got != tt.want {
			t.Errorf("IsAppAuthFinding(%q) = %v, want %v", tt.title, got, tt.want)
		}
	}
}

// TestAdvisoryFindingHealsOnPass is the #2575 regression test: an access
// finding must appear in the digest while the condition is real, and must
// LEAVE the digest (moving to Recently Resolved) once the App has proven it
// can write.
func TestAdvisoryFindingHealsOnPass(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"scanner": store}

	if _, err := store.Create("Insufficient repo permissions", beads.TypeAdvisory, beads.PriorityHigh, "scanner", ""); err != nil {
		t.Fatal(err)
	}

	// (1) While the condition holds (no successful App post yet), the finding
	// is in the digest.
	d := BuildDigestFromBeads(stores, "busy", DigestOptions{})
	if d.TotalCount != 1 {
		t.Fatalf("before heal: TotalCount = %d, want 1", d.TotalCount)
	}
	if len(d.ByAgent["scanner"]) != 1 || d.ByAgent["scanner"][0].Title != "Insufficient repo permissions" {
		t.Fatalf("before heal: finding missing from digest: %+v", d.ByAgent)
	}

	// (2) The App proves it can write — the caller invokes the heal. The
	// finding must disappear from open findings and show as resolved.
	healed := CloseHealedAppAuthFindings(stores)
	if len(healed) != 1 || healed[0] != "Insufficient repo permissions" {
		t.Fatalf("healed = %v, want the one access finding", healed)
	}

	d = BuildDigestFromBeads(stores, "busy", DigestOptions{})
	if d.TotalCount != 0 {
		t.Errorf("after heal: TotalCount = %d, want 0", d.TotalCount)
	}
	if len(d.RecentlyResolved) != 1 || d.RecentlyResolved[0].Title != "Insufficient repo permissions" {
		t.Errorf("after heal: RecentlyResolved = %+v, want the healed finding", d.RecentlyResolved)
	}

	// (3) The zero-findings digest must still RENDER, so the pinned GitHub
	// comment gets rewritten instead of freezing on the stale finding.
	md := FormatDigestMarkdown(d, DigestOptions{Org: "acme", PrimaryRepo: "widgets"})
	if md == "" {
		t.Fatal("after heal: FormatDigestMarkdown returned empty — the stale comment would never update")
	}
	if !strings.Contains(md, "**Findings:** 0") {
		t.Errorf("all-clear digest missing zero-findings line:\n%s", md)
	}
	if !strings.Contains(md, "~~Insufficient repo permissions~~") {
		t.Errorf("all-clear digest missing resolved finding:\n%s", md)
	}

	// (4) Healing must be idempotent: nothing left to close.
	if again := CloseHealedAppAuthFindings(stores); len(again) != 0 {
		t.Errorf("second heal closed %v, want none", again)
	}
}

// TestCloseHealedAppAuthFindingsSelectivity verifies only app-access advisory
// beads are closed: code findings, non-advisory bead types and already-closed
// beads are all left alone, and the close reason is recorded.
func TestCloseHealedAppAuthFindingsSelectivity(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"scanner": store}

	access, err := store.Create("Insufficient repo permissions", beads.TypeAdvisory, beads.PriorityHigh, "scanner", "")
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.Create("Workflow permissions overly broad in ci.yml", beads.TypeAdvisory, beads.PriorityMedium, "scanner", "ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.Create("GitHub App not installed follow-up", beads.TypeTask, beads.PriorityLow, "scanner", "")
	if err != nil {
		t.Fatal(err)
	}
	already, err := store.Create("GitHub App lacks repo access", beads.TypeAdvisory, beads.PriorityHigh, "scanner", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(already.ID); err != nil {
		t.Fatal(err)
	}

	healed := CloseHealedAppAuthFindings(stores)
	if len(healed) != 1 || healed[0] != "Insufficient repo permissions" {
		t.Fatalf("healed = %v, want only the open access finding", healed)
	}

	got, _ := store.Get(access.ID)
	if got.Status != beads.StatusClosed {
		t.Errorf("access bead status = %s, want closed", got.Status)
	}
	if reason := got.Meta(closeReasonMetadataKey); reason != appAuthHealedCloseReason {
		t.Errorf("close reason = %q, want %q", reason, appAuthHealedCloseReason)
	}
	for _, b := range []*beads.Bead{code, task} {
		got, _ := store.Get(b.ID)
		if got.Status != beads.StatusOpen {
			t.Errorf("bead %q status = %s, want open (must not be auto-closed)", b.Title, got.Status)
		}
	}
}

// TestPersistAsBeadsRecreatesAfterResolve verifies a RECURRENCE is not
// suppressed: once a finding's bead is resolved, re-reporting the same title
// must create a fresh open bead, so a condition that heals and later regresses
// reappears in the digest.
func TestPersistAsBeadsRecreatesAfterResolve(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"scanner": store}
	findings := []Finding{{Agent: "scanner", Severity: "high", Title: "Insufficient repo permissions"}}

	if created := PersistAsBeads(findings, stores); created != 1 {
		t.Fatalf("first persist created %d, want 1", created)
	}
	// While open, the same title is a dup.
	if created := PersistAsBeads(findings, stores); created != 0 {
		t.Fatalf("open dup created %d, want 0", created)
	}

	if healed := CloseHealedAppAuthFindings(stores); len(healed) != 1 {
		t.Fatalf("healed %d, want 1", len(healed))
	}

	// The condition regressed and the agent reported it again: a new open
	// bead must be created.
	if created := PersistAsBeads(findings, stores); created != 1 {
		t.Fatalf("post-resolve persist created %d, want 1 (recurrence must not be suppressed)", created)
	}
	d := BuildDigestFromBeads(stores, "busy", DigestOptions{})
	if d.TotalCount != 1 {
		t.Errorf("digest after recurrence TotalCount = %d, want 1", d.TotalCount)
	}
}

// TestBuildDigestFromBeadsDoneIsResolved verifies a bead an agent marked
// "done" is treated as resolved, not as a still-open finding.
func TestBuildDigestFromBeadsDoneIsResolved(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create("fixed thing", beads.TypeAdvisory, beads.PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(b.ID, func(bd *beads.Bead) { bd.Status = beads.StatusDone }); err != nil {
		t.Fatal(err)
	}

	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "busy", DigestOptions{})
	if d.TotalCount != 0 {
		t.Errorf("TotalCount = %d, want 0 (done beads are resolved)", d.TotalCount)
	}
	if len(d.RecentlyResolved) != 1 || d.RecentlyResolved[0].Title != "fixed thing" {
		t.Errorf("RecentlyResolved = %+v, want the done bead", d.RecentlyResolved)
	}
}

// TestFormatDigestMarkdownStillEmptyWhenNothingToSay pins the unchanged
// behavior: no findings AND no recent resolutions renders nothing.
func TestFormatDigestMarkdownStillEmptyWhenNothingToSay(t *testing.T) {
	d := BuildDigestFromBeads(map[string]*beads.Store{}, "idle", DigestOptions{})
	if md := FormatDigestMarkdown(d, DigestOptions{Org: "acme", PrimaryRepo: "widgets"}); md != "" {
		t.Errorf("expected empty markdown, got:\n%s", md)
	}
}

// TestClosePRLinkedAdvisoryBeads is the "a fix retires its finding" path: a
// merged PR whose title restates the finding closes it, while an unrelated PR
// leaves every finding alone.
func TestClosePRLinkedAdvisoryBeads(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	matched, err := store.Create("pr-verifier workflow fails on every pull request", beads.TypeAdvisory, beads.PriorityHigh, "ci-maintainer", "")
	if err != nil {
		t.Fatalf("creating matched bead: %v", err)
	}
	unrelated, err := store.Create("knowledge curator promotes facts without provenance", beads.TypeAdvisory, beads.PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatalf("creating unrelated bead: %v", err)
	}
	stores := map[string]*beads.Store{"ci-maintainer": store}

	closed := ClosePRLinkedAdvisoryBeads(stores, "fix the pr-verifier workflow so it stops failing on every pull request")
	if len(closed) != 1 || closed[0] != matched.Title {
		t.Fatalf("closed = %v, want exactly [%q]", closed, matched.Title)
	}
	got, err := store.Get(matched.ID)
	if err != nil {
		t.Fatalf("re-reading matched bead: %v", err)
	}
	if got.Status != beads.StatusClosed {
		t.Errorf("matched bead status = %q, want %q", got.Status, beads.StatusClosed)
	}
	if reason := got.Meta(closeReasonMetadataKey); reason != prLinkedCloseReason {
		t.Errorf("close_reason = %q, want %q", reason, prLinkedCloseReason)
	}
	other, err := store.Get(unrelated.ID)
	if err != nil {
		t.Fatalf("re-reading unrelated bead: %v", err)
	}
	if other.Status != beads.StatusOpen {
		t.Errorf("unrelated bead status = %q, want %q — a dissimilar PR title must close nothing", other.Status, beads.StatusOpen)
	}
}

// TestClosePRLinkedAdvisoryBeadsIgnoresDissimilarAndClosed guards the two ways
// this could do damage: closing a finding a PR does not address, and touching
// beads that are not advisory findings at all.
func TestClosePRLinkedAdvisoryBeadsIgnoresDissimilarAndClosed(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	task, err := store.Create("refactor the dashboard websocket hub", beads.TypeTask, beads.PriorityMedium, "architect", "")
	if err != nil {
		t.Fatalf("creating task bead: %v", err)
	}
	stores := map[string]*beads.Store{"architect": store}

	// Title-identical to the task bead, which is NOT an advisory finding.
	if closed := ClosePRLinkedAdvisoryBeads(stores, "refactor the dashboard websocket hub"); len(closed) != 0 {
		t.Fatalf("closed = %v, want none — task beads are not advisory findings", closed)
	}
	got, err := store.Get(task.ID)
	if err != nil {
		t.Fatalf("re-reading task bead: %v", err)
	}
	if got.Status != beads.StatusOpen {
		t.Errorf("task bead status = %q, want %q", got.Status, beads.StatusOpen)
	}

	// An empty PR title carries no tokens and must be a no-op, not a mass close.
	if closed := ClosePRLinkedAdvisoryBeads(stores, ""); len(closed) != 0 {
		t.Fatalf("closed = %v on an empty PR title, want none", closed)
	}
}

// gee4veeRepoAccessTitles are the five exact guide-agent finding titles from
// #2575 (comment 5359943936) that survived the #4291 fix — the repo-access
// heal exists to retire precisely these once the read path is verified.
var gee4veeRepoAccessTitles = []struct{ title, ref string }{
	{"ACMM L2 guide agent lacks read-only repository access mechanism", "hive-infrastructure"},
	{"Critical infrastructure gap: No repository workspace provisioning for advisory agents", "hive.yaml:repos"},
	{"Critical: Repository access infrastructure missing - guide agent cannot audit documentation", "hive.yaml.runtime:repos"},
	{"Guide agent cannot access target repository - no clone mechanism available", "github.ibm.com/devx-prod/epx-vscode-ext-poc"},
	{"Guide agent blocked: No repository access mechanism in L2 advisory mode", "github.ibm.com/devx-prod/epx-vscode-ext-poc"},
}

// issue4464RepoAccessTitles are the two finding titles a live hive's advisory
// digest showed in #4464, transcribed from the screenshot on the issue. Both
// are the same family as gee4veeRepoAccessTitles, and NEITHER matched before
// this — so the healer never considered them and they could not age out of the
// digest no matter how many cycles ran. The operator read them as an error the
// hive was reporting about itself, which is what the bug report is.
var issue4464RepoAccessTitles = []struct{ title, ref string }{
	{"Repository worktree not provisioned for guide agent despite include_repos=true configuration", "/data/agents/guide (empty directory)"},
	{"Quality agent lacks read access to repository for coverage analysis", "devx-prod/epx-vscode-ext-poc"},
	// Gap 3, found live on a customer hive after the first two fixes shipped:
	// the bare repo noun, no workspace/worktree qualifier.
	{"Critical: Repository not provisioned for guide agent - cannot perform documentation audit", "/tmp/hive/epx-vscode-ext-poc"},
}

func TestIsRepoAccessFinding(t *testing.T) {
	for _, f := range gee4veeRepoAccessTitles {
		if !IsRepoAccessFinding(f.title) {
			t.Errorf("IsRepoAccessFinding(%q) = false, want true (the #2575 family must match)", f.title)
		}
	}
	for _, f := range issue4464RepoAccessTitles {
		if !IsRepoAccessFinding(f.title) {
			t.Errorf("IsRepoAccessFinding(%q) = false, want true (the #4464 family must match)", f.title)
		}
	}
	// Drift the agents plausibly produce must still match.
	drift := []string{
		"Advisory agent has no repository access path in ISSUES_ONLY mode",
		"clone mechanism unavailable for L2 guide agents",
		"Guide agent unable to fetch the target repository",
		"Agent workspace lacks a git clone mechanism",
		// #4464 drift: the passive "not/never provisioned" voice, and
		// "<verb> access TO <repo>" word order, in wordings adjacent to the
		// two the issue reported.
		"No git worktree provisioned for the guide agent",
		"Agent workspace never provisioned for documentation auditing",
		"Repository workspace never provisioned for advisory agents",
		"Missing repository checkout provisioning for advisory agents",
		"Missing repository worktree provisioning for guide agent",
		"Guide agent lacks read-only access to the repository",
		"Scanner agent has no read-only access to the repository",
		"No clone access to repo for the newcomer agent",
	}
	for _, title := range drift {
		if !IsRepoAccessFinding(title) {
			t.Errorf("IsRepoAccessFinding(%q) = false, want true (wording drift)", title)
		}
	}
	// Code findings that merely mention repository access must NEVER match —
	// closing one would silently retire a real finding.
	negatives := []string{
		"Repository access logs are not retained",
		"Public read access to repository secrets endpoint not restricted",
		"Workflow permissions overly broad in ci.yml",
		"Add repository access checks to the admin API",
		"Overly permissive repository access granted to all org members",
		"Insufficient repo permissions", // app-auth family, handled by the other healer
		// WRITE-access lack is the app-auth family too — a verified Contents
		// READ must never close it.
		"Guide agent lacks write access to repository for issue filing",
		// A worktree that exists but is dirty/stale is a real operational
		// finding, not a missing read path.
		"Repository worktree contains uncommitted changes from previous session",
		"",
		// The #4464 widening reaches for "provisioned" and "access to
		// <repo>", so these guard the two new patterns specifically: real
		// code findings that use the same words about something else.
		"Anonymous read access to repository artifacts is not gated",
		"Terraform state bucket provisioned without encryption",
		"Repository checkout action is not pinned to a commit SHA",
		"No rate limit on the repository search endpoint",
		// WRITE access is the app-auth healer's business: this healer's proof
		// is a verified READ, which cannot substantiate a write claim.
		"Guide agent lacks write access to repository for the digest",
	}
	for _, title := range negatives {
		if IsRepoAccessFinding(title) {
			t.Errorf("IsRepoAccessFinding(%q) = true, want false", title)
		}
	}
}

func TestRepoFromFindingRef(t *testing.T) {
	tests := []struct {
		ref  string
		want string
		ok   bool
	}{
		{"github.ibm.com/devx-prod/epx-vscode-ext-poc", "devx-prod/epx-vscode-ext-poc", true},
		{"acme/widgets", "acme/widgets", true},
		{"hive.yaml:repos", "", false},
		{"hive.yaml.runtime:repos", "", false},
		{"hive-infrastructure", "", false},
		{"src/main.go:12", "", false},
		{"docs/install.md", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := RepoFromFindingRef(tt.ref)
		if got != tt.want || ok != tt.ok {
			t.Errorf("RepoFromFindingRef(%q) = (%q, %v), want (%q, %v)", tt.ref, got, ok, tt.want, tt.ok)
		}
	}
}

// TestCloseHealedRepoAccessFindings is the #2575 follow-up regression test:
// each of the five surviving guide-agent findings must close once the read
// path is verified, while a finding about a repo that is GENUINELY unreadable
// must stay open, as must code findings and non-advisory beads.
func TestCloseHealedRepoAccessFindings(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"guide": store}

	for _, f := range gee4veeRepoAccessTitles {
		if _, err := store.Create(f.title, beads.TypeAdvisory, beads.PriorityHigh, "guide", f.ref); err != nil {
			t.Fatal(err)
		}
	}
	// The #4464 pair heals through the same gate: the guide finding's ref
	// ("/data/agents/guide (empty directory)") parses as no-repo → primary,
	// and the quality finding's ref names the verified GHE repo.
	for _, f := range issue4464RepoAccessTitles {
		if _, err := store.Create(f.title, beads.TypeAdvisory, beads.PriorityHigh, "guide", f.ref); err != nil {
			t.Fatal(err)
		}
	}
	// A repo-access finding about a repo the verifier CANNOT read: real
	// condition, must survive the heal.
	locked, err := store.Create("Guide agent cannot access target repository - no clone mechanism available",
		beads.TypeAdvisory, beads.PriorityHigh, "guide", "github.ibm.com/other-org/locked-repo")
	if err != nil {
		t.Fatal(err)
	}
	// A code finding mentioning access must never be considered.
	code, err := store.Create("Repository access logs are not retained", beads.TypeAdvisory, beads.PriorityMedium, "guide", "audit.go")
	if err != nil {
		t.Fatal(err)
	}

	var probed []string
	canRead := func(ownerRepo string) bool {
		probed = append(probed, ownerRepo)
		// "" = the hive's own primary repo (readable); the configured GHE
		// repo is readable; anything else is not.
		return ownerRepo == "" || ownerRepo == "devx-prod/epx-vscode-ext-poc"
	}

	healed := CloseHealedRepoAccessFindings(stores, canRead)
	wantHealed := len(gee4veeRepoAccessTitles) + len(issue4464RepoAccessTitles)
	if len(healed) != wantHealed {
		t.Fatalf("healed %d findings (%v), want the %d from #2575 + #4464", len(healed), healed, wantHealed)
	}
	for _, f := range append(append([]struct{ title, ref string }{}, gee4veeRepoAccessTitles...), issue4464RepoAccessTitles...) {
		found := false
		for _, h := range healed {
			if h == f.title {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("finding %q was not healed", f.title)
		}
	}
	// The unreadable repo's probe must have been consulted and refused.
	probedLocked := false
	for _, p := range probed {
		if p == "other-org/locked-repo" {
			probedLocked = true
		}
	}
	if !probedLocked {
		t.Error("verification was never consulted for the unreadable repo")
	}
	got, _ := store.Get(locked.ID)
	if got.Status != beads.StatusOpen {
		t.Errorf("finding about unreadable repo status = %s, want open — a real access problem must never auto-close", got.Status)
	}
	gotCode, _ := store.Get(code.ID)
	if gotCode.Status != beads.StatusOpen {
		t.Errorf("code finding status = %s, want open", gotCode.Status)
	}

	// Closed beads carry the distinct repo-access close reason.
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Status != beads.StatusClosed {
			continue
		}
		if reason := b.Meta(closeReasonMetadataKey); reason != repoAccessHealedCloseReason {
			t.Errorf("close reason for %q = %q, want %q", b.Title, reason, repoAccessHealedCloseReason)
		}
	}

	// Idempotent: nothing left to close on a second pass.
	if again := CloseHealedRepoAccessFindings(stores, canRead); len(again) != 0 {
		t.Errorf("second heal closed %v, want none", again)
	}
	// A nil verifier must close nothing, ever.
	if closed := CloseHealedRepoAccessFindings(stores, nil); closed != nil {
		t.Errorf("nil verifier closed %v, want none", closed)
	}
}

// TestCloseHealedRepoAccessFindings_Issue4464 runs the two titles a live hive
// actually displayed (#4464) end to end. Before the pattern widening both were
// invisible to IsRepoAccessFinding, so this heal was a no-op on them and the
// operator's digest kept showing an infrastructure error that nothing could
// ever retire. The refs are the ones from the report, including the guide
// finding's non-repo ref — "/data/agents/guide (empty directory)" does not
// parse as owner/repo, which must fall back to probing the hive's own primary
// repo rather than refusing to consider the bead.
func TestCloseHealedRepoAccessFindings_Issue4464(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"guide": store}
	for _, f := range issue4464RepoAccessTitles {
		if _, err := store.Create(f.title, beads.TypeAdvisory, beads.PriorityHigh, "guide", f.ref); err != nil {
			t.Fatal(err)
		}
	}

	var probed []string
	canRead := func(ownerRepo string) bool {
		probed = append(probed, ownerRepo)
		return ownerRepo == "" || ownerRepo == "devx-prod/epx-vscode-ext-poc"
	}

	healed := CloseHealedRepoAccessFindings(stores, canRead)
	if len(healed) != len(issue4464RepoAccessTitles) {
		t.Fatalf("healed %d findings (%v), want both #4464 titles", len(healed), healed)
	}
	wantProbes := map[string]bool{"": false, "devx-prod/epx-vscode-ext-poc": false}
	for _, p := range probed {
		if _, ok := wantProbes[p]; ok {
			wantProbes[p] = true
		}
	}
	for repo, seen := range wantProbes {
		if !seen {
			t.Errorf("read verification was never consulted for %q", repo)
		}
	}

	// The read gate still decides. With no readable repo, the same two
	// findings must survive — a digest entry that is TRUE has to stay.
	store2, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range issue4464RepoAccessTitles {
		if _, err := store2.Create(f.title, beads.TypeAdvisory, beads.PriorityHigh, "guide", f.ref); err != nil {
			t.Fatal(err)
		}
	}
	unreadable := func(string) bool { return false }
	if closed := CloseHealedRepoAccessFindings(map[string]*beads.Store{"guide": store2}, unreadable); len(closed) != 0 {
		t.Errorf("closed %v with an unverifiable read path, want none", closed)
	}
}

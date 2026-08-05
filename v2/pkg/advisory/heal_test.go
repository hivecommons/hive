package advisory

import (
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/beads"
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
	d := BuildDigestFromBeads(stores, "busy")
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

	d = BuildDigestFromBeads(stores, "busy")
	if d.TotalCount != 0 {
		t.Errorf("after heal: TotalCount = %d, want 0", d.TotalCount)
	}
	if len(d.RecentlyResolved) != 1 || d.RecentlyResolved[0].Title != "Insufficient repo permissions" {
		t.Errorf("after heal: RecentlyResolved = %+v, want the healed finding", d.RecentlyResolved)
	}

	// (3) The zero-findings digest must still RENDER, so the pinned GitHub
	// comment gets rewritten instead of freezing on the stale finding.
	md := FormatDigestMarkdown(d, "acme", "widgets")
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
	d := BuildDigestFromBeads(stores, "busy")
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

	d := BuildDigestFromBeads(map[string]*beads.Store{"scanner": store}, "busy")
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
	d := BuildDigestFromBeads(map[string]*beads.Store{}, "idle")
	if md := FormatDigestMarkdown(d, "acme", "widgets"); md != "" {
		t.Errorf("expected empty markdown, got:\n%s", md)
	}
}

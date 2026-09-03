package dashboard

// Tests for the dossier's split surfaces, the public permalink, the honesty
// fixes, and the collaborator collection.
//
// The most important test here is TestDossierRendersWithoutUndefinedGlobals.
// The dossier shipped broken for a whole commit cycle because renderMeCard
// referenced ccProjectName across an IIFE boundary: every render threw
// ReferenceError, and the catch reported it as "you have no contributor
// profile". No test caught it, because every existing test asserted on the
// SERVED markup, which was perfectly fine — the failure only existed at
// runtime. That gap is what this file closes.

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// ── The render bug ──────────────────────────────────────────────────────────

// TestDossierRendersWithoutUndefinedGlobals pins the cross-IIFE contract that
// broke: every identifier renderMeCard reads from the enclosing scope must be
// declared in the SAME script block it lives in (or explicitly mirrored into
// it). ccProjectName was declared only in the onboarding block, so the dossier
// renderer could never see it.
func TestDossierRendersWithoutUndefinedGlobals(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	body := w.Body.String()

	// Split the page into its script blocks; the dossier renderer lives in one
	// of them and must be self-sufficient within it.
	blocks := strings.Split(body, "<script>")
	var dossierBlock string
	for _, b := range blocks {
		if strings.Contains(b, "function renderMeCard") {
			dossierBlock = b
			break
		}
	}
	if dossierBlock == "" {
		t.Fatal("renderMeCard not found in any script block")
	}

	// Every global the dossier renderer depends on from its own closure.
	for _, ident := range []string{"ccProjectName", "EPIGRAPH", "FOOTER_QUOTE", "ME_RANK_META", "ME_LADDER", "ME_STYLE_NAMES", "ME_IS_OWNER"} {
		if !regexp.MustCompile(`var\s+` + ident + `\s*=`).MatchString(dossierBlock) {
			t.Errorf("%s is used by the dossier renderer but never declared in its script block "+
				"— this is exactly the ccProjectName ReferenceError that shipped broken", ident)
		}
	}

	// The publisher side of the bridge must also survive.
	if !strings.Contains(body, "window.ccProjectName=ccProjectName") {
		t.Error("ccProjectName is no longer republished for the dossier block")
	}
}

// TestDossierHelpersAreAllDeclared closes the other half of the ReferenceError
// gap. The globals check above only looks at `var` declarations, so a helper
// that is *called* but never defined — the usual result of hoisting a closure
// out into a shared function and forgetting to paste the definition — sails
// straight past it and throws at runtime exactly like ccProjectName did.
func TestDossierHelpersAreAllDeclared(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/contribute", nil))

	var block string
	for _, b := range strings.Split(w.Body.String(), "<script>") {
		if strings.Contains(b, "function renderMeCard") {
			block = b
			break
		}
	}
	if block == "" {
		t.Fatal("renderMeCard not found in any script block")
	}

	// Dossier helpers follow a naming convention; anything called by that name
	// must be defined in the same block, as a function or a var holding one.
	called := regexp.MustCompile(`\b(me[A-Z]\w*|renderMe\w*|wireMe\w*|loadMe\w*)\s*\(`)
	seen := map[string]bool{}
	for _, m := range called.FindAllStringSubmatch(block, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		declared := regexp.MustCompile(`(function\s+` + name + `\s*\(|var\s+` + name + `\s*=)`)
		if !declared.MatchString(block) {
			t.Errorf("%s() is called by the dossier but never declared in its script block "+
				"— it will throw ReferenceError on every render", name)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no dossier helpers detected; the convention this test relies on has changed")
	}
}

// TestRenderFailureIsNotReportedAsMissingProfile pins the diagnostic fix: a
// thrown render must never be described to the user as a missing account.
func TestRenderFailureIsNotReportedAsMissingProfile(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, "function renderMeError") {
		t.Error("no distinct render-failure state: a bug will masquerade as a missing profile again")
	}
	if !strings.Contains(body, "catch(e){console.error('renderMeCard failed',e);renderMeError(mount,e);}") {
		t.Error("renderMeCard is not guarded by a distinct error path")
	}
}

// ── The split ───────────────────────────────────────────────────────────────

func TestProfileTabIsSeparateFromLeaderboard(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	req := httptest.NewRequest(http.MethodGet, "/contribute/profile", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`data-panel="tab-profile"`,
		`id="tab-profile"`,
		`id="me-card-mount"`,     // the dossier now mounts on the Profile tab
		`id="me-standing-mount"`, // the leaderboard keeps only the standing strip
		"function loadMeStanding",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}

	// The dossier mount must be INSIDE the profile panel, not the leaderboard.
	lbIdx := strings.Index(body, `id="tab-leaderboard"`)
	profIdx := strings.Index(body, `id="tab-profile"`)
	cardIdx := strings.Index(body, `id="me-card-mount"`)
	if cardIdx <= profIdx || profIdx <= lbIdx {
		t.Errorf("dossier is not inside the profile panel (lb=%d profile=%d card=%d)", lbIdx, profIdx, cardIdx)
	}
}

// ── The public permalink ────────────────────────────────────────────────────

func TestPublicDossierPermalink(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute/dossier/castrojo", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("permalink code %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "ME_VIEW_USERNAME") {
		t.Error("permalink does not carry the viewed-username detection")
	}
	if !strings.Contains(body, "var ME_IS_OWNER") {
		t.Error("owner gating absent")
	}

	// A malformed username must never reach the profile store.
	for _, bad := range []string{"/contribute/dossier/..", "/contribute/dossier/a%2Fb"} {
		rw := httptest.NewRecorder()
		s.mux.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, bad, nil))
		if rw.Code == http.StatusOK {
			t.Errorf("%s should not render a dossier (code %d)", bad, rw.Code)
		}
	}
}

// TestOwnerOnlyControlsAreGated proves the edit/invite/quota affordances are
// withheld from a visitor reading someone else's record.
func TestOwnerOnlyControlsAreGated(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	req := httptest.NewRequest(http.MethodGet, "/contribute/profile", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	body := w.Body.String()

	for _, want := range []string{
		"function meDossierForm(p){\n  if(!ME_IS_OWNER)return '';",
		"function meInviteSection(p){\n  if(!ME_IS_OWNER)return '';",
		"if(ME_IS_OWNER){",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("owner-only control is not gated: missing %q", want)
		}
	}
}

// ── Honesty fixes ───────────────────────────────────────────────────────────

func TestDeedsGridOmitsFailureCount(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/contribute", nil))
	body := w.Body.String()

	if strings.Contains(body, `[String(p.tasks_failed||0),'failed','']`) {
		t.Error("the failure count is back in the public honour grid")
	}
}

func TestLadderHasNoUnreachableRung(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/contribute", nil))
	body := w.Body.String()

	// The invariant is REACHABILITY, not the presence or absence of any one
	// designation: every rung before the aspirational cut must be somewhere a
	// real trust tier can actually stand. (WARDEN was once unreachable because
	// ME_LADDER_AT skipped index 3; it is reachable again now that the advisor
	// tier maps to it. Asserting the property rather than the name means this
	// test keeps working as tiers are added upstream.)
	m := regexp.MustCompile(`var ME_LADDER=\[([^\]]*)\]`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("ME_LADDER not found")
	}
	rungs := strings.Count(m[1], ",") + 1

	am := regexp.MustCompile(`var ME_LADDER_AT=\{([^}]*)\}`).FindStringSubmatch(body)
	if am == nil {
		t.Fatal("ME_LADDER_AT not found")
	}
	asp := regexp.MustCompile(`var ME_LADDER_ASPIRATIONAL_AT=(\d+)`).FindStringSubmatch(body)
	if asp == nil {
		t.Fatal("ME_LADDER_ASPIRATIONAL_AT not found")
	}
	aspirationalAt, err := strconv.Atoi(asp[1])
	if err != nil {
		t.Fatalf("aspirational index unparseable: %v", err)
	}

	occupied := map[int]bool{}
	for _, pair := range strings.Split(am[1], ",") {
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			t.Fatalf("ladder index for %q unparseable: %v", parts[0], err)
		}
		if idx >= rungs {
			t.Errorf("tier %q maps to rung %d, past the end of the ladder (%d rungs)", parts[0], idx, rungs)
		}
		occupied[idx] = true
	}
	for i := 0; i < aspirationalAt; i++ {
		if !occupied[i] {
			t.Errorf("rung %d (%s) is unreachable: no trust tier maps to it",
				i, strings.Split(m[1], ",")[i])
		}
	}

	// Every real trust tier must have a designation + metal, or the dossier
	// silently renders that contributor as the lowest rank (which is how the
	// merger tier read as RECRUIT when it was added upstream).
	for _, tier := range []string{"newcomer", "contributor", "trusted", "merger", "advisor"} {
		if !regexp.MustCompile(`(?m)^\s*` + tier + `:\[`).MatchString(body) {
			t.Errorf("trust tier %q has no ME_RANK_META entry: it will render as RECRUIT", tier)
		}
		if !strings.Contains(am[1], tier+":") {
			t.Errorf("trust tier %q has no ME_LADDER_AT entry: it will stand on rung 0", tier)
		}
	}

	// Gold is VANGUARD's metal and must stay unclaimed: no trust tier may map to
	// a rung at or beyond the aspirational cut, and no tier may carry the gold.
	if strings.Contains(body, "'#e8c15a'") {
		t.Error("gold has been granted to a trust tier; the spec reserves it for an unclaimed VANGUARD")
	}
	for idx := range occupied {
		if idx >= aspirationalAt {
			t.Errorf("a trust tier maps to rung %d, which is meant to be aspirational", idx)
		}
	}
}

func TestTheatersListNoHiveWithoutEvidence(t *testing.T) {
	setupContributeEnv(t)
	saveContributorProfile(&ContributorProfile{
		GitHubUsername: "alice", ContributorID: "c-a", TrustTier: "contributor",
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	})
	reg := loadFederationRegistry()
	reg.Hives = append(reg.Hives,
		FederationHive{ID: "h1", ProjectName: "Named", ActiveContributorNames: []string{"alice"}},
		FederationHive{ID: "h2", ProjectName: "Unnamed"},
		FederationHive{ID: "h3", ProjectName: "SomeoneElse", ActiveContributorNames: []string{"bob"}},
	)
	if err := saveFederationRegistry(reg); err != nil {
		t.Fatalf("save registry: %v", err)
	}

	hives := buildContributorHives("alice", "")
	if len(hives) != 1 || hives[0].ID != "h1" {
		t.Fatalf("only hives naming the contributor may be listed, got %+v", hives)
	}
}

// ── Collaborators ───────────────────────────────────────────────────────────

func seedCollabProfile(t *testing.T, name string) {
	t.Helper()
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: name, ContributorID: "c-" + name, TrustTier: "contributor",
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

func TestRecordCollaborationIsSymmetric(t *testing.T) {
	setupContributeEnv(t)
	seedCollabProfile(t, "alice")
	seedCollabProfile(t, "bob")

	recordCollaboration("alice", "bob", collabHowIssue, time.Now())

	for _, pair := range [][2]string{{"alice", "bob"}, {"bob", "alice"}} {
		p, _ := loadContributorProfile(pair[0])
		if len(p.Collaborators) != 1 || p.Collaborators[0].Username != pair[1] {
			t.Fatalf("%s should have %s on record, got %+v", pair[0], pair[1], p.Collaborators)
		}
		if p.Collaborators[0].Occasions != 1 || p.Collaborators[0].How != collabHowIssue {
			t.Fatalf("unexpected record for %s: %+v", pair[0], p.Collaborators[0])
		}
	}
}

func TestRecordCollaborationAccumulatesAndUpgradesHow(t *testing.T) {
	setupContributeEnv(t)
	seedCollabProfile(t, "alice")
	seedCollabProfile(t, "bob")

	first := time.Now().Add(-48 * time.Hour)
	recordCollaboration("alice", "bob", collabHowPresence, first)
	recordCollaboration("alice", "bob", collabHowIssue, time.Now())

	p, _ := loadContributorProfile("alice")
	if len(p.Collaborators) != 1 {
		t.Fatalf("meeting again must not duplicate the person: %+v", p.Collaborators)
	}
	c := p.Collaborators[0]
	if c.Occasions != 2 {
		t.Errorf("occasions = %d, want 2", c.Occasions)
	}
	// The stronger way of meeting wins; first_met_at never moves.
	if c.How != collabHowIssue {
		t.Errorf("how = %q, want the stronger %q", c.How, collabHowIssue)
	}
	if !strings.HasPrefix(c.FirstMetAt, first.UTC().Format("2006-01-02")) {
		t.Errorf("first_met_at moved: %q", c.FirstMetAt)
	}
}

func TestRecordCollaborationRejectsSelf(t *testing.T) {
	setupContributeEnv(t)
	seedCollabProfile(t, "alice")
	recordCollaboration("alice", "alice", collabHowIssue, time.Now())
	recordCollaboration("alice", "ALICE", collabHowIssue, time.Now())
	p, _ := loadContributorProfile("alice")
	if len(p.Collaborators) != 0 {
		t.Fatalf("nobody collaborates with themselves: %+v", p.Collaborators)
	}
}

func TestCollaboratorsAreBounded(t *testing.T) {
	setupContributeEnv(t)
	seedCollabProfile(t, "alice")
	for i := 0; i < maxCollaborators+10; i++ {
		other := "peer" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		seedCollabProfile(t, other)
		recordCollaboration("alice", other, collabHowPresence, time.Now())
	}
	p, _ := loadContributorProfile("alice")
	if len(p.Collaborators) > maxCollaborators {
		t.Fatalf("collaborators unbounded: %d > %d", len(p.Collaborators), maxCollaborators)
	}
}

func TestBackfillInviteCollaborationsIsIdempotent(t *testing.T) {
	setupContributeEnv(t)
	seedCollabProfile(t, "sponsor")
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: "invitee", ContributorID: "c-i", TrustTier: "newcomer",
		InvitedBy: "sponsor", RegisteredAt: time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed invitee: %v", err)
	}

	if n := backfillInviteCollaborations(); n != 1 {
		t.Fatalf("first backfill recorded %d pairs, want 1", n)
	}
	// Running again must record nothing and must not inflate the count.
	if n := backfillInviteCollaborations(); n != 0 {
		t.Fatalf("second backfill recorded %d pairs, want 0", n)
	}
	p, _ := loadContributorProfile("invitee")
	if len(p.Collaborators) != 1 || p.Collaborators[0].Occasions != 1 {
		t.Fatalf("backfill is not idempotent: %+v", p.Collaborators)
	}
	if p.Collaborators[0].How != collabHowInvite {
		t.Errorf("how = %q, want %q", p.Collaborators[0].How, collabHowInvite)
	}
	// Symmetric: the sponsor has the invitee too.
	sp, _ := loadContributorProfile("sponsor")
	if len(sp.Collaborators) != 1 || sp.Collaborators[0].Username != "invitee" {
		t.Fatalf("sponsor side missing: %+v", sp.Collaborators)
	}
}

func TestCollaboratorsSurfaceInProfileResponse(t *testing.T) {
	setupContributeEnv(t)
	seedCollabProfile(t, "alice")
	seedCollabProfile(t, "bob")
	recordCollaboration("alice", "bob", collabHowIssue, time.Now())

	s := NewServer(0, slog.Default())
	resp := s.BuildContributorProfile("alice")
	if !resp.Found {
		t.Fatal("alice not found")
	}
	if len(resp.Collaborators) != 1 || resp.Collaborators[0].Username != "bob" {
		t.Fatalf("collaborators absent from the profile response: %+v", resp.Collaborators)
	}
}

func TestSortedCollaboratorsIsDeterministic(t *testing.T) {
	p := &ContributorProfile{Collaborators: []CollaboratorRecord{
		{Username: "c", Occasions: 1, FirstMetAt: "2026-01-03T00:00:00Z"},
		{Username: "a", Occasions: 5, FirstMetAt: "2026-01-02T00:00:00Z"},
		{Username: "b", Occasions: 1, FirstMetAt: "2026-01-01T00:00:00Z"},
	}}
	got := sortedCollaborators(p)
	want := []string{"a", "b", "c"} // most occasions, then earliest met
	for i := range want {
		if got[i].Username != want[i] {
			t.Fatalf("order = %v, want %v", []CollaboratorRecord{got[0], got[1], got[2]}, want)
		}
	}
}

func TestCollaboratorsZoneRendersRealRecords(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/contribute", nil))
	body := w.Body.String()

	if !strings.Contains(body, "function meCollaborators") {
		t.Fatal("collaborators zone has no renderer")
	}
	if strings.Contains(body, "These honors cannot be earned alone.") {
		t.Error("the dead hardcoded empty state is back")
	}
	if !strings.Contains(body, "p.collaborators") {
		t.Error("the zone does not read the server-provided collaborators")
	}
}

// The dossier interpolates stored and upstream-fetched values into ATTRIBUTE
// context (value="..." on the edit form, src/href="..." on Credly heraldry).
// The original esc() was a textContent→innerHTML round-trip, which escapes only
// &<> — a value containing a double quote could therefore close the attribute
// and inject markup, reachable on the public permalink via a crafted Credly
// badge URL. esc() must escape quotes too.
func TestEscEscapesQuotesForAttributeContext(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/contribute", nil))
	body := w.Body.String()

	m := strings.Index(body, "function esc(s)")
	if m < 0 {
		t.Fatal("esc() not found")
	}
	// Take the function body, however it is spelled, and require quote handling.
	escBody := body[m:]
	if len(escBody) > 800 {
		escBody = escBody[:800]
	}
	if !strings.Contains(escBody, "&quot;") {
		t.Error("esc() does not escape double quotes: attribute-context interpolation is injectable")
	}
	if strings.Contains(escBody, "textContent") {
		t.Error("esc() is back to the textContent round-trip, which leaves quotes unescaped")
	}
}

// localHiveIdentity was wired ONLY for tests (and no test ever set the override),
// so in production it returned "" and the contributor's own hive silently
// vanished from Theaters unless the federation registry happened to name them.
// It must resolve from the configured project name.
func TestLocalHiveIdentityResolvesFromConfig(t *testing.T) {
	setupContributeEnv(t)
	prev := hiveIdentityForTest
	hiveIdentityForTest = ""
	t.Cleanup(func() { hiveIdentityForTest = prev })

	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{Config: &config.Config{
		Project: config.ProjectConfig{Name: "Bluefin"},
	}}

	if got := s.localHiveIdentity(); got != "Bluefin" {
		t.Fatalf("local hive identity = %q, want %q — Theaters will omit the contributor's own hive", got, "Bluefin")
	}

	// And it must actually surface the local hive, without the registry naming anyone.
	reg := loadFederationRegistry()
	reg.Hives = append(reg.Hives,
		FederationHive{ID: "local", ProjectName: "Bluefin"},
		FederationHive{ID: "other", ProjectName: "Elsewhere"},
	)
	if err := saveFederationRegistry(reg); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	hives := buildContributorHives("alice", s.localHiveIdentity())
	if len(hives) != 1 || hives[0].ID != "local" {
		t.Fatalf("own hive not listed from config identity, got %+v", hives)
	}

	// An unnamed project stays honest: no identity, no invented theatre.
	s.deps.Config.Project.Name = ""
	if got := s.localHiveIdentity(); got != "" {
		t.Fatalf("unnamed project must yield no identity, got %q", got)
	}
}

// The Golden Path header sits directly above the progress bars, so announcing an
// aspirational rung as the "next designation" implies those bars count toward it.
// Nothing counts toward VANGUARD — it is conferred — and inventing a gate would
// break the rule that nothing on the dossier is ever fabricated.
func TestAspirationalRungIsNotAnnouncedAsNext(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/contribute", nil))
	body := w.Body.String()

	if !strings.Contains(body, "nextIsConferred") {
		t.Error("the Golden Path header no longer distinguishes conferred rungs from attainable ones")
	}
	if !strings.Contains(body, "is conferred, not counted toward") {
		t.Error("an aspirational rung must not be presented as a countable next designation")
	}
	if !strings.Contains(body, "var nextIsConferred=nextIdx>=ME_LADDER_ASPIRATIONAL_AT") {
		t.Error("the conferred test must key off the aspirational cut, not a hardcoded rung name")
	}
}

// ── Review fixes ────────────────────────────────────────────────────────────

// The local hive must be listed on the strength of the profile alone. A hive
// does not register itself into its OWN federation registry, so gating the
// local theatre on a registry match left the zone permanently empty on every
// ordinary deployment.
func TestTheatersAlwaysIncludesLocalHive(t *testing.T) {
	setupContributeEnv(t)
	reg := loadFederationRegistry()
	reg.Hives = append(reg.Hives, FederationHive{ID: "h-remote", ProjectName: "Elsewhere"})
	if err := saveFederationRegistry(reg); err != nil {
		t.Fatalf("save registry: %v", err)
	}

	hives := buildContributorHives("alice", "KubeStellar")
	if len(hives) != 1 || hives[0].ProjectName != "KubeStellar" {
		t.Fatalf("the local hive must be listed without a registry entry, got %+v", hives)
	}
	// ...and never twice when the registry DOES carry it.
	reg = loadFederationRegistry()
	reg.Hives = append(reg.Hives, FederationHive{ID: "hive-ks", ProjectName: "KubeStellar", Org: "kubestellar"})
	if err := saveFederationRegistry(reg); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	hives = buildContributorHives("alice", "KubeStellar")
	if len(hives) != 1 || hives[0].Org != "kubestellar" {
		t.Fatalf("the registry entry must not be duplicated by the synthetic one, got %+v", hives)
	}
}

// ActiveContributorNames needs a producer, or the cross-hive half of Theaters
// is dead code: no remote hive can ever qualify.
func TestHeartbeatAcceptsContributorNames(t *testing.T) {
	setupContributeEnv(t)
	reg := loadFederationRegistry()
	reg.Hives = append(reg.Hives, FederationHive{ID: "hive-x", ProjectName: "X"})
	if err := saveFederationRegistry(reg); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	w := doPostRaw(s, "/api/hives/hive-x/heartbeat",
		`{"active_contributors":2,"active_contributor_names":["alice","bad name!",""]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat status %d: %s", w.Code, w.Body.String())
	}

	got := loadFederationRegistry()
	var names []string
	for _, h := range got.Hives {
		if h.ID == "hive-x" {
			names = h.ActiveContributorNames
		}
	}
	if len(names) != 1 || names[0] != "alice" {
		t.Fatalf("heartbeat must store only valid usernames, got %+v", names)
	}
	if hives := buildContributorHives("alice", ""); len(hives) != 1 || hives[0].ID != "hive-x" {
		t.Fatalf("a heartbeat-named contributor must see that theatre, got %+v", hives)
	}
}

// The collaborator graph's SYMMETRIC invariant must survive a username case
// mismatch: profile filenames carry register-time case, while invite/session
// attribution carries OAuth-session case.
func TestRecordCollaborationSurvivesCaseMismatch(t *testing.T) {
	setupContributeEnv(t)
	seedCollabProfile(t, "Alice")
	seedCollabProfile(t, "bob")

	recordCollaboration("alice", "BOB", collabHowIssue, time.Now())

	for _, pair := range [][2]string{{"Alice", "bob"}, {"bob", "Alice"}} {
		p, err := loadContributorProfile(pair[0])
		if err != nil {
			t.Fatalf("load %s: %v", pair[0], err)
		}
		if len(p.Collaborators) != 1 || !strings.EqualFold(p.Collaborators[0].Username, pair[1]) {
			t.Fatalf("%s should have %s on record, got %+v", pair[0], pair[1], p.Collaborators)
		}
		// The canonical stored spelling is recorded, never the caller's casing.
		if p.Collaborators[0].Username != pair[1] {
			t.Errorf("%s recorded %q, want canonical %q", pair[0], p.Collaborators[0].Username, pair[1])
		}
	}
}

// A dossier write must not let a signed-in contributor stream an unbounded body
// into memory before the sanitisers truncate it.
func TestDossierRejectsOversizeBody(t *testing.T) {
	s := newDossierTestServer(t)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername: "alice", ContributorID: "c-a", TrustTier: "contributor",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	huge := `{"testimony":"` + strings.Repeat("A", maxRequestBodyBytes*2) + `"}`
	if w := postDossier(t, s, "alice", huge); w.Code != http.StatusBadRequest {
		t.Fatalf("an oversize body must be refused, got %d", w.Code)
	}
}

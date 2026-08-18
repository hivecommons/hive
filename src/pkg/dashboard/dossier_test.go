package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newDossierTestServer(t *testing.T) *Server {
	t.Helper()
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	return s
}

func postDossier(t *testing.T, s *Server, caller, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/dossier", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if caller != "" {
		req.Header.Set("X-Hive-User", caller)
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	return w
}

// Anonymous callers cannot write a dossier; the identity is never body-supplied.
func TestDossier_AnonymousRejected(t *testing.T) {
	s := newDossierTestServer(t)
	if w := postDossier(t, s, "", `{"archetype":"x"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// A signed-in caller with no contributor profile gets 403, not a fresh profile.
func TestDossier_NoProfileRejected(t *testing.T) {
	s := newDossierTestServer(t)
	if w := postDossier(t, s, "ghost", `{"archetype":"x"}`); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// A full update round-trips: values are sanitised, bounded, and persisted.
func TestDossier_UpdateSanitizesAndPersists(t *testing.T) {
	s := newDossierTestServer(t)
	setInviteTier(t, "dossieruser", "newcomer")

	body := `{
		"archetype":"  Community <b>Steward</b>  ",
		"specializations":["Docs","docs","triage","CI ","","a","b","c","d","e","f"],
		"testimony":"` + strings.Repeat("y", 300) + `",
		"equipped_title":"WOLFHERDER",
		"credly_name":"Jorge_Castro!!-99",
		"emblem_seed":"abcdef0123456789XX"
	}`
	w := postDossier(t, s, "dossieruser", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	p, err := loadContributorProfile("dossieruser")
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	// Sanitised as TEXT (markup stored verbatim, escaped at render) and trimmed.
	if p.Archetype != "Community <b>Steward</b>" {
		t.Errorf("archetype = %q", p.Archetype)
	}
	if len(p.Specializations) != 8 {
		t.Errorf("specializations capped at 8, got %d: %v", len(p.Specializations), p.Specializations)
	}
	if p.Specializations[0] != "docs" || p.Specializations[2] != "ci" {
		t.Errorf("specializations not normalised: %v", p.Specializations)
	}
	if len([]rune(p.Testimony)) != maxDossierTestimonyLen {
		t.Errorf("testimony not truncated to %d, got %d", maxDossierTestimonyLen, len([]rune(p.Testimony)))
	}
	if p.EquippedTitle != "WOLFHERDER" {
		t.Errorf("equipped_title = %q", p.EquippedTitle)
	}
	// Credly name keeps only [a-z0-9-].
	if p.CredlyName != "jorgecastro-99" {
		t.Errorf("credly_name = %q", p.CredlyName)
	}
	if len(p.EmblemSeed) != maxDossierEmblemSeedLen {
		t.Errorf("emblem_seed not truncated: %q", p.EmblemSeed)
	}
}

// Partial update: absent fields are untouched; explicit empties clear.
func TestDossier_PartialUpdate(t *testing.T) {
	s := newDossierTestServer(t)
	setInviteTier(t, "partialuser", "newcomer")

	if w := postDossier(t, s, "partialuser", `{"archetype":"Builder","testimony":"Shipping beats perfect."}`); w.Code != http.StatusOK {
		t.Fatalf("first save: %d", w.Code)
	}
	// Only touch testimony: archetype must survive; empty string clears.
	if w := postDossier(t, s, "partialuser", `{"testimony":""}`); w.Code != http.StatusOK {
		t.Fatalf("second save: %d", w.Code)
	}
	p, _ := loadContributorProfile("partialuser")
	if p.Archetype != "Builder" {
		t.Errorf("archetype lost on partial update: %q", p.Archetype)
	}
	if p.Testimony != "" {
		t.Errorf("testimony not cleared: %q", p.Testimony)
	}
}

// The Me-profile response exposes loadout/sponsor/dossier fields, and honours
// the 14-day recency firewall on last_active (absence is never rendered).
func TestMeProfile_DossierExposureAndRecencyFirewall(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())

	p := &ContributorProfile{
		GitHubUsername:  "loadout",
		ContributorID:   "c-loadout",
		TrustTier:       "contributor",
		RegisteredAt:    time.Now().UTC().Format(time.RFC3339),
		CLIBackend:      "claude-code",
		Model:           "claude-fable-5",
		InvitedBy:       "castrojo",
		Archetype:       "Scholar",
		Specializations: []string{"docs", "ci"},
		Testimony:       "Docs are load-bearing.",
		EquippedTitle:   "CARTOGRAPHER",
		CredlyName:      "some-name",
		LastActive:      time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
	}
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save: %v", err)
	}

	resp := s.BuildContributorProfile("loadout")
	if !resp.Found {
		t.Fatal("profile not found")
	}
	if resp.CLIBackend != "claude-code" || resp.Model != "claude-fable-5" || resp.InvitedBy != "castrojo" {
		t.Errorf("loadout/sponsor not exposed: %+v", resp)
	}
	if resp.Archetype != "Scholar" || resp.EquippedTitle != "CARTOGRAPHER" || resp.Testimony == "" || resp.CredlyName != "some-name" {
		t.Errorf("dossier fields not exposed: %+v", resp)
	}
	if resp.LastActive == "" {
		t.Error("recent last_active should be included")
	}

	// Older than 14 days → OMITTED entirely (never "inactive since").
	p.LastActive = time.Now().UTC().Add(-15 * 24 * time.Hour).Format(time.RFC3339)
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save: %v", err)
	}
	resp = s.BuildContributorProfile("loadout")
	if resp.LastActive != "" {
		t.Errorf("stale last_active must be omitted, got %q", resp.LastActive)
	}
}

// The public leaderboard rows carry the equipped title.
func TestLeaderboard_EquippedTitle(t *testing.T) {
	setupContributeEnv(t)
	p := &ContributorProfile{
		GitHubUsername: "titled",
		ContributorID:  "c-titled",
		TrustTier:      "contributor",
		RegisteredAt:   time.Now().UTC().Format(time.RFC3339),
		EquippedTitle:  "LAMPLIGHTER",
	}
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save: %v", err)
	}
	entries := buildLeaderboard()
	if len(entries) != 1 || entries[0].EquippedTitle != "LAMPLIGHTER" {
		t.Fatalf("equipped title missing from leaderboard: %+v", entries)
	}
}

// Heraldry: unlinked (no credly_name) and unknown users both yield the calm
// {"badges":[],"linked":false} shape — never an error the card must special-case.
func TestHeraldry_UnlinkedAndUnknown(t *testing.T) {
	s := newDossierTestServer(t)
	setInviteTier(t, "nolink", "newcomer")

	for _, u := range []string{"nolink", "nobody"} {
		req := httptest.NewRequest(http.MethodGet, "/api/leaderboard/contributor/"+u+"/heraldry", nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", u, w.Code)
		}
		var resp struct {
			Badges []HeraldryBadge `json:"badges"`
			Linked bool            `json:"linked"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: bad JSON: %v", u, err)
		}
		if resp.Linked || len(resp.Badges) != 0 {
			t.Errorf("%s: expected unlinked empty, got %+v", u, resp)
		}
	}
}

// Heraldry: a linked profile mirrors the PUBLIC badges from Credly's JSON
// (trimmed shape), skips private ones, and caches the fetch (one upstream hit).
func TestHeraldry_FetchTrimAndCache(t *testing.T) {
	s := newDossierTestServer(t)

	hits := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/users/some-vanity/badges.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[
			{"id":"abc","issued_at":"2021-10-01T00:00:00.000-05:00","image_url":"https://images.credly.com/x.png","public":true,
			 "badge_template":{"name":"Speaker · KubeCon NA"},"issuer":{"summary":"issued by The Linux Foundation"}},
			{"id":"priv","issued_at":"2022-01-01T00:00:00.000-05:00","image_url":"https://images.credly.com/y.png","public":false,
			 "badge_template":{"name":"Hidden"},"issuer":{"summary":"x"}}
		]}`))
	}))
	defer stub.Close()

	origBase := heraldryBaseURL
	heraldryBaseURL = stub.URL
	t.Cleanup(func() {
		heraldryBaseURL = origBase
		heraldryCacheMu.Lock()
		delete(heraldryCache, "some-vanity")
		heraldryCacheMu.Unlock()
	})

	setInviteTier(t, "linked", "newcomer")
	p, _ := loadContributorProfile("linked")
	p.CredlyName = "some-vanity"
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save: %v", err)
	}

	get := func() (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/leaderboard/contributor/linked/heraldry", nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	code, body := get()
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	var resp struct {
		Badges []HeraldryBadge `json:"badges"`
		Linked bool            `json:"linked"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if !resp.Linked || len(resp.Badges) != 1 {
		t.Fatalf("expected 1 public badge, got %+v", resp)
	}
	b := resp.Badges[0]
	if b.Name != "Speaker · KubeCon NA" ||
		b.IssuerSummary != "issued by The Linux Foundation" ||
		b.ImageURL != "https://images.credly.com/x.png" ||
		b.PublicURL != "https://www.credly.com/badges/abc" {
		t.Errorf("trimmed badge wrong: %+v", b)
	}

	// Second request is served from the 6h cache — no second upstream hit.
	get()
	if hits != 1 {
		t.Errorf("expected 1 upstream fetch (cached), got %d", hits)
	}
}

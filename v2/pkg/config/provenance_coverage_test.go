package config

import (
	"testing"
)

// ============================================================
// Provenance — Set / SetPrefix / LayerOf / note / Report
// ============================================================

func TestNewProvenance(t *testing.T) {
	p := NewProvenance()
	if p == nil {
		t.Fatal("NewProvenance returned nil")
	}
	if p.Fields == nil {
		t.Fatal("Fields map not initialized")
	}
	if p.alsoSet == nil {
		t.Fatal("alsoSet map not initialized")
	}
}

func TestProvenance_Set(t *testing.T) {
	p := NewProvenance()
	p.Set("github.app_id", LayerSeed)
	if got := p.Fields["github.app_id"]; got != LayerSeed {
		t.Fatalf("expected LayerSeed, got %v", got)
	}
}

func TestProvenance_Set_NilReceiver(t *testing.T) {
	var p *Provenance
	// Should not panic.
	p.Set("github.app_id", LayerSeed)
}

func TestProvenance_SetPrefix(t *testing.T) {
	p := NewProvenance()
	p.Set("github.app_id", LayerSeed)
	p.Set("github.api_url", LayerSeed)
	p.Set("hub.url", LayerSeed)

	p.SetPrefix("github.", LayerDashboardOverlay)

	if got := p.Fields["github.app_id"]; got != LayerDashboardOverlay {
		t.Fatalf("github.app_id should be overlay, got %v", got)
	}
	if got := p.Fields["github.api_url"]; got != LayerDashboardOverlay {
		t.Fatalf("github.api_url should be overlay, got %v", got)
	}
	if got := p.Fields["hub.url"]; got != LayerSeed {
		t.Fatalf("hub.url should remain seed, got %v", got)
	}
}

func TestProvenance_SetPrefix_NilReceiver(t *testing.T) {
	var p *Provenance
	p.SetPrefix("github.", LayerDashboardOverlay) // must not panic
}

func TestProvenance_LayerOf(t *testing.T) {
	p := NewProvenance()
	p.Set("hub.url", LayerDashboardOverlay)

	if got := p.LayerOf("hub.url"); got != LayerDashboardOverlay {
		t.Fatalf("expected LayerDashboardOverlay, got %v", got)
	}
	if got := p.LayerOf("nonexistent"); got != LayerUnset {
		t.Fatalf("expected LayerUnset for missing field, got %v", got)
	}
}

func TestProvenance_LayerOf_NilReceiver(t *testing.T) {
	var p *Provenance
	if got := p.LayerOf("anything"); got != LayerUnset {
		t.Fatalf("nil Provenance should return LayerUnset, got %v", got)
	}
}

func TestProvenance_RecordAll_NilConfig(t *testing.T) {
	p := NewProvenance()
	p.recordAll(nil, LayerSeed) // must not panic
	if len(p.Fields) != 0 {
		t.Fatal("expected no fields for nil config")
	}
}

func TestProvenance_RecordAll_NilProvenance(t *testing.T) {
	var p *Provenance
	cfg := &Config{HiveID: "test-hive"}
	p.recordAll(cfg, LayerSeed) // must not panic
}

func TestProvenance_RecordAll_PopulatesFields(t *testing.T) {
	p := NewProvenance()
	level := 3
	cfg := &Config{
		HiveID:    "test-hive",
		ACMMLevel: &level,
		Project: ProjectConfig{
			Org:         "acme",
			PrimaryRepo: "main",
		},
		Hub: HubConfig{
			URL: "https://hub.example.com",
		},
	}

	p.recordAll(cfg, LayerSeed)

	if got := p.LayerOf("hive_id"); got != LayerSeed {
		t.Errorf("hive_id should be LayerSeed, got %v", got)
	}
	if got := p.LayerOf("project.org"); got != LayerSeed {
		t.Errorf("project.org should be LayerSeed, got %v", got)
	}
	if got := p.LayerOf("acmm_level"); got != LayerSeed {
		t.Errorf("acmm_level should be LayerSeed, got %v", got)
	}
	if got := p.LayerOf("hub.url"); got != LayerSeed {
		t.Errorf("hub.url should be LayerSeed, got %v", got)
	}
}

func TestProvenance_RecordAll_OverwritesWithHigherLayer(t *testing.T) {
	p := NewProvenance()
	cfg1 := &Config{HiveID: "from-seed"}
	cfg2 := &Config{HiveID: "from-overlay"}

	p.recordAll(cfg1, LayerSeed)
	p.recordAll(cfg2, LayerDashboardOverlay)

	if got := p.LayerOf("hive_id"); got != LayerDashboardOverlay {
		t.Fatalf("last recordAll should win, got %v", got)
	}
}

func TestProvenance_Note_Deduplicates(t *testing.T) {
	p := NewProvenance()
	p.note("hub.url", LayerSeed)
	p.note("hub.url", LayerSeed)
	p.note("hub.url", LayerDashboardOverlay)

	if len(p.alsoSet["hub.url"]) != 2 {
		t.Fatalf("expected 2 unique layers, got %d", len(p.alsoSet["hub.url"]))
	}
}

func TestProvenance_Report(t *testing.T) {
	p := NewProvenance()
	level := 2
	cfg := &Config{
		HiveID:    "my-hive",
		ACMMLevel: &level,
		Project: ProjectConfig{
			Org:         "myorg",
			PrimaryRepo: "myrepo",
		},
		Hub: HubConfig{
			URL: "https://hub.test",
		},
	}

	// Simulate a real merge: seed sets everything, overlay wins on some.
	p.recordAll(cfg, LayerSeed)
	p.recordAll(cfg, LayerDashboardOverlay)

	report := p.Report(cfg)

	if len(report) == 0 {
		t.Fatal("Report should produce at least one entry")
	}

	// All entries should have Layer set.
	for _, entry := range report {
		if entry.Layer == "" {
			t.Errorf("entry %q has empty Layer", entry.Field)
		}
		if entry.Field == "" {
			t.Error("entry has empty Field")
		}
	}

	// Should be sorted by Field.
	for i := 1; i < len(report); i++ {
		if report[i].Field < report[i-1].Field {
			t.Errorf("report not sorted: %q before %q", report[i-1].Field, report[i].Field)
		}
	}
}

func TestProvenance_Report_AlsoSetBy(t *testing.T) {
	p := NewProvenance()
	cfg := &Config{
		HiveID: "test",
		Hub:    HubConfig{URL: "https://hub.test"},
	}

	// Simulate: seed sets hub.url, then overlay overwrites it.
	p.recordAll(cfg, LayerSeed)
	p.recordAll(cfg, LayerDashboardOverlay)

	report := p.Report(cfg)

	var hubEntry *FieldOrigin
	for i := range report {
		if report[i].Field == "hub.url" {
			hubEntry = &report[i]
			break
		}
	}
	if hubEntry == nil {
		t.Fatal("hub.url not in report")
	}
	if hubEntry.Layer != LayerDashboardOverlay.String() {
		t.Errorf("hub.url layer = %s, want dashboard overlay", hubEntry.Layer)
	}
	// AlsoSetBy should mention the seed.
	found := false
	for _, s := range hubEntry.AlsoSetBy {
		if s == LayerSeed.String() {
			found = true
		}
	}
	if !found {
		t.Errorf("expected AlsoSetBy to mention seed, got %v", hubEntry.AlsoSetBy)
	}
}

func TestProvenance_Report_NilProvenance(t *testing.T) {
	var p *Provenance
	cfg := &Config{HiveID: "x"}
	// Should not panic with nil provenance.
	report := p.Report(cfg)
	if report == nil {
		// nil is acceptable — just shouldn't panic.
		return
	}
}

// ============================================================
// enumerateFields
// ============================================================

func TestEnumerateFields_NilConfig(t *testing.T) {
	fields := enumerateFields(nil)
	if fields != nil {
		t.Fatalf("expected nil, got %v", fields)
	}
}

func TestEnumerateFields_SkipsEmptyValues(t *testing.T) {
	cfg := &Config{} // all zero values
	fields := enumerateFields(cfg)
	for _, f := range fields {
		if f.Value == "" {
			t.Errorf("field %q has empty value, should be skipped", f.Field)
		}
	}
}

func TestEnumerateFields_IncludesPopulatedFields(t *testing.T) {
	level := 4
	cfg := &Config{
		HiveID:    "hive-abc",
		ACMMLevel: &level,
		Project:   ProjectConfig{Org: "kubestellar", PrimaryRepo: "hive"},
		GitHub:    GitHubConfig{AppID: 123, AppSlug: "my-app", APIURL: "https://api.github.com"},
		Hub:       HubConfig{URL: "https://hub.example.com"},
		Agents:    map[string]AgentConfig{"scanner": {}},
	}

	fields := enumerateFields(cfg)

	fieldNames := make(map[string]bool)
	for _, f := range fields {
		fieldNames[f.Field] = true
	}

	expected := []string{"hive_id", "acmm_level", "project.org", "project.primary_repo", "github.app_id", "github.app_slug", "github.api_url", "hub.url", "agents.scanner"}
	for _, name := range expected {
		if !fieldNames[name] {
			t.Errorf("expected field %q not found in enumeration", name)
		}
	}
}

func TestEnumerateFields_RedactsToken(t *testing.T) {
	cfg := &Config{
		GitHub: GitHubConfig{Token: "ghp_secret123"},
	}

	fields := enumerateFields(cfg)
	for _, f := range fields {
		if f.Field == "github.token" && f.Value != redacted {
			t.Errorf("github.token should be redacted, got %q", f.Value)
		}
	}
}

// ============================================================
// forgeOfAppID / slugOfAppID
// ============================================================

func TestForgeOfAppID(t *testing.T) {
	tests := []struct {
		appID int64
		want  string
	}{
		{PublicGitHubAppID, "github.com"},
		{EnterpriseGitHubAppID, "github.ibm.com"},
		{0, ""},
		{999999, ""},
	}
	for _, tc := range tests {
		got := forgeOfAppID(tc.appID)
		if got != tc.want {
			t.Errorf("forgeOfAppID(%d) = %q, want %q", tc.appID, got, tc.want)
		}
	}
}

func TestSlugOfAppID(t *testing.T) {
	tests := []struct {
		appID int64
		want  string
	}{
		{PublicGitHubAppID, DefaultGitHubAppSlug},
		{EnterpriseGitHubAppID, EnterpriseGitHubAppSlug},
		{0, ""},
		{42, ""},
	}
	for _, tc := range tests {
		got := SlugOfAppID(tc.appID)
		if got != tc.want {
			t.Errorf("SlugOfAppID(%d) = %q, want %q", tc.appID, got, tc.want)
		}
	}
}

// ============================================================
// displayOrEmpty
// ============================================================

func TestDisplayOrEmpty(t *testing.T) {
	if got := displayOrEmpty(""); got == "" {
		t.Error("empty string should produce a visible placeholder")
	}
	if got := displayOrEmpty("https://api.github.com"); got != "https://api.github.com" {
		t.Errorf("non-empty should pass through, got %q", got)
	}
}

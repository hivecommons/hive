package config

import "testing"

// review.fix_human_prs (hivecommons/hive#8421): the upgrade step keeps a hive
// that was already pushing fixes to everyone's PRs doing so, and touches
// nothing else.

func TestReviewFixHumanPRsDefaultsOff(t *testing.T) {
	var rc ReviewConfig
	if rc.FixHumanPRs != nil || rc.FixHumanPRsEnabled() {
		t.Fatalf("fix_human_prs must default off (nil): %+v", rc)
	}
	off := false
	rc.FixHumanPRs = &off
	if rc.FixHumanPRsEnabled() {
		t.Fatal("explicit false read as enabled")
	}
	on := true
	rc.FixHumanPRs = &on
	if !rc.FixHumanPRsEnabled() {
		t.Fatal("explicit true read as disabled")
	}
}

func TestMigrateReviewFixHumanPRs_AllAuthorsOnAndUnsetBecomesTrue(t *testing.T) {
	cfg := &Config{Review: ReviewConfig{AllAuthors: true, FixerAgent: "scanner", PostComments: true, RequireApproval: true}}
	if !cfg.MigrateReviewFixHumanPRs() {
		t.Fatal("migration reported no change")
	}
	if !cfg.Review.FixHumanPRsEnabled() {
		t.Fatalf("all_authors on with fix_human_prs unset must migrate to true: %+v", cfg.Review)
	}
	// Every other review field is left exactly as set.
	if !cfg.Review.AllAuthors || cfg.Review.FixerAgent != "scanner" || !cfg.Review.PostComments || !cfg.Review.RequireApproval {
		t.Fatalf("migration touched other review fields: %+v", cfg.Review)
	}
	// Idempotent: a second pass finds the explicit value and does nothing.
	if cfg.MigrateReviewFixHumanPRs() {
		t.Fatal("migration re-ran on an already-migrated config")
	}
}

func TestMigrateReviewFixHumanPRs_AllAuthorsOffStaysFalse(t *testing.T) {
	cfg := &Config{Review: ReviewConfig{AllAuthors: false}}
	if cfg.MigrateReviewFixHumanPRs() {
		t.Fatal("migration changed a config with all_authors off")
	}
	if cfg.Review.FixHumanPRs != nil || cfg.Review.FixHumanPRsEnabled() {
		t.Fatalf("new hive must come out with fix_human_prs off: %+v", cfg.Review)
	}
}

func TestMigrateReviewFixHumanPRs_ExplicitFalseNeverOverwritten(t *testing.T) {
	off := false
	cfg := &Config{Review: ReviewConfig{AllAuthors: true, FixHumanPRs: &off}}
	if cfg.MigrateReviewFixHumanPRs() {
		t.Fatal("migration overwrote an explicit false")
	}
	if cfg.Review.FixHumanPRsEnabled() {
		t.Fatalf("explicit fix_human_prs: false was overwritten: %+v", cfg.Review)
	}
}

// TestLoadMigratesReviewFixHumanPRs proves the step is wired into Load, which
// is the path every boot and reload takes.
func TestLoadMigratesReviewFixHumanPRs(t *testing.T) {
	base := `
project:
  org: my-org
  repos: [repo-a]
github:
  token: ghp_tok
agents:
  w:
    backend: claude
`
	cases := []struct {
		name   string
		review string
		want   bool
		set    bool
	}{
		{name: "all_authors_unset_toggle", review: "review:\n  all_authors: true\n", want: true, set: true},
		{name: "all_authors_off", review: "review:\n  all_authors: false\n", want: false, set: false},
		{name: "no_review_block", review: "", want: false, set: false},
		{name: "explicit_false_kept", review: "review:\n  all_authors: true\n  fix_human_prs: false\n", want: false, set: true},
		{name: "explicit_true_kept", review: "review:\n  all_authors: false\n  fix_human_prs: true\n", want: true, set: true},
	}
	for _, tc := range cases {
		cfg, err := Load(writeTempConfig(t, base+tc.review))
		if err != nil {
			t.Fatalf("%s: Load() error = %v", tc.name, err)
		}
		if got := cfg.Review.FixHumanPRsEnabled(); got != tc.want {
			t.Fatalf("%s: fix_human_prs enabled = %v, want %v (%+v)", tc.name, got, tc.want, cfg.Review)
		}
		if set := cfg.Review.FixHumanPRs != nil; set != tc.set {
			t.Fatalf("%s: fix_human_prs set = %v, want %v", tc.name, set, tc.set)
		}
	}
}

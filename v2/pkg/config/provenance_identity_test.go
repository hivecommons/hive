package config

import (
	"errors"
	"strings"
	"testing"
)

// Tests for IdentitySetIssues, RejectIdentitySet, and identity-set edge cases
// in provenance.go. The core provenance API (Set, Report, enumerateFields, etc.)
// is already covered in provenance_coverage_test.go.

// ─────────────────────────────────────────────────────────────────────────────
// IdentitySetIssues — valid configurations (no issues expected)
// ─────────────────────────────────────────────────────────────────────────────

func TestIdentitySetIssues_ValidPublicConfig(t *testing.T) {
	// Healthy public hive: both URLs empty (defaults to github.com).
	gh := GitHubConfig{
		AppID:   PublicGitHubAppID,
		AppSlug: DefaultGitHubAppSlug,
		APIURL:  "",
		BaseURL: "",
	}
	issues := IdentitySetIssues(gh)
	if len(issues) != 0 {
		t.Errorf("expected no issues for valid public config, got: %v", issues)
	}
}

func TestIdentitySetIssues_ValidPublicExplicitURLs(t *testing.T) {
	gh := GitHubConfig{
		AppID:   PublicGitHubAppID,
		AppSlug: DefaultGitHubAppSlug,
		APIURL:  "https://api.github.com",
		BaseURL: "https://github.com",
	}
	issues := IdentitySetIssues(gh)
	if len(issues) != 0 {
		t.Errorf("expected no issues for explicit public URLs, got: %v", issues)
	}
}

func TestIdentitySetIssues_ValidGHEConfig(t *testing.T) {
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		APIURL:  EnterpriseGitHubAPIURL,
		BaseURL: EnterpriseGitHubBaseURL,
	}
	issues := IdentitySetIssues(gh)
	if len(issues) != 0 {
		t.Errorf("expected no issues for valid GHE config, got: %v", issues)
	}
}

func TestIdentitySetIssues_ValidGHEOnlyAPIURL(t *testing.T) {
	// GHE hive with api_url set but base_url empty — legitimate fleet config.
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		APIURL:  EnterpriseGitHubAPIURL,
		BaseURL: "",
	}
	issues := IdentitySetIssues(gh)
	if len(issues) != 0 {
		t.Errorf("expected no issues for GHE with empty base_url, got: %v", issues)
	}
}

func TestIdentitySetIssues_ValidGHEOnlyBaseURL(t *testing.T) {
	// GHE hive with base_url set but api_url empty — vllm-d cluster shape.
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		APIURL:  "",
		BaseURL: EnterpriseGitHubBaseURL,
	}
	issues := IdentitySetIssues(gh)
	if len(issues) != 0 {
		t.Errorf("expected no issues for GHE with only base_url, got: %v", issues)
	}
}

func TestIdentitySetIssues_EmptySlugIsNotMismatch(t *testing.T) {
	// Empty slug resolves via default — should NOT trigger a mismatch.
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: "",
		APIURL:  EnterpriseGitHubAPIURL,
		BaseURL: EnterpriseGitHubBaseURL,
	}
	issues := IdentitySetIssues(gh)
	for _, issue := range issues {
		if strings.Contains(issue, "slug") {
			t.Errorf("empty slug should not trigger mismatch, got: %s", issue)
		}
	}
}

func TestIdentitySetIssues_UnknownAppIDPassesValidation(t *testing.T) {
	// Unknown app_id must not be flagged — a third-party forge is legitimate.
	gh := GitHubConfig{
		AppID:   77777,
		AppSlug: "custom-app",
		APIURL:  "https://custom.example.com/api/v3",
		BaseURL: "https://custom.example.com",
	}
	issues := IdentitySetIssues(gh)
	if len(issues) != 0 {
		t.Errorf("unknown app_id should produce no issues, got: %v", issues)
	}
}

func TestIdentitySetIssues_ZeroAppID(t *testing.T) {
	gh := GitHubConfig{AppID: 0}
	issues := IdentitySetIssues(gh)
	if len(issues) != 0 {
		t.Errorf("zero app_id should produce no issues, got: %v", issues)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// IdentitySetIssues — invalid configurations (issues expected)
// ─────────────────────────────────────────────────────────────────────────────

func TestIdentitySetIssues_GHEAppIDEmptyURLs(t *testing.T) {
	// THE INCIDENT: GHE app_id with empty URLs (defaults to github.com).
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		APIURL:  "",
		BaseURL: "",
	}
	issues := IdentitySetIssues(gh)
	if len(issues) == 0 {
		t.Fatal("expected issues for GHE app_id with empty URLs (the incident shape)")
	}
	combined := strings.Join(issues, " ")
	if !strings.Contains(combined, "404") {
		t.Errorf("issue should mention 404, got: %v", issues)
	}
}

func TestIdentitySetIssues_WrongSlugForGHEAppID(t *testing.T) {
	// GHE app_id but public slug — assembled from two different Apps.
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: DefaultGitHubAppSlug,
		APIURL:  EnterpriseGitHubAPIURL,
		BaseURL: EnterpriseGitHubBaseURL,
	}
	issues := IdentitySetIssues(gh)
	if len(issues) == 0 {
		t.Fatal("expected slug mismatch issue")
	}
	combined := strings.Join(issues, " ")
	if !strings.Contains(combined, "slug") {
		t.Errorf("issue should mention slug, got: %v", issues)
	}
}

func TestIdentitySetIssues_WrongSlugForPublicAppID(t *testing.T) {
	gh := GitHubConfig{
		AppID:   PublicGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		APIURL:  "",
		BaseURL: "",
	}
	issues := IdentitySetIssues(gh)
	if len(issues) == 0 {
		t.Fatal("expected slug mismatch for public app with GHE slug")
	}
}

func TestIdentitySetIssues_ConflictingBaseAndAPIURLs(t *testing.T) {
	// base_url=GHE but api_url=public — both populated, directly conflicting.
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		APIURL:  "https://api.github.com",
		BaseURL: "https://github.ibm.com",
	}
	issues := IdentitySetIssues(gh)
	if len(issues) == 0 {
		t.Fatal("expected conflict between base_url and api_url")
	}
	combined := strings.Join(issues, " ")
	if !strings.Contains(combined, "same forge") {
		t.Errorf("issue should mention forge mismatch, got: %v", issues)
	}
}

func TestIdentitySetIssues_GHESlugWithPublicAPI(t *testing.T) {
	// GHE slug but explicit public api_url — marker disagreement.
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		APIURL:  "https://api.github.com",
		BaseURL: "",
	}
	issues := IdentitySetIssues(gh)
	if len(issues) == 0 {
		t.Fatal("expected issue for GHE slug with public api_url")
	}
}

func TestIdentitySetIssues_MultipleIssues(t *testing.T) {
	// Config that triggers both app_id forge mismatch AND slug mismatch.
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: DefaultGitHubAppSlug,
		APIURL:  "",
		BaseURL: "",
	}
	issues := IdentitySetIssues(gh)
	if len(issues) < 2 {
		t.Errorf("expected at least 2 issues, got %d: %v", len(issues), issues)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RejectIdentitySet tests
// ─────────────────────────────────────────────────────────────────────────────

func TestRejectIdentitySet_ValidConfig(t *testing.T) {
	gh := GitHubConfig{
		AppID:   PublicGitHubAppID,
		AppSlug: DefaultGitHubAppSlug,
	}
	if err := RejectIdentitySet(gh); err != nil {
		t.Errorf("expected nil error for valid config, got: %v", err)
	}
}

func TestRejectIdentitySet_InvalidConfig_ReturnsError(t *testing.T) {
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		APIURL:  "",
		BaseURL: "",
	}
	err := RejectIdentitySet(gh)
	if err == nil {
		t.Fatal("expected error for invalid identity set")
	}
	if !errors.Is(err, ErrIdentitySetMismatch) {
		t.Errorf("error should wrap ErrIdentitySetMismatch, got: %v", err)
	}
}

func TestRejectIdentitySet_ErrorContainsAllIssues(t *testing.T) {
	// Triggers both forge mismatch and slug mismatch.
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: DefaultGitHubAppSlug,
		APIURL:  "",
		BaseURL: "",
	}
	err := RejectIdentitySet(gh)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "slug") {
		t.Errorf("error should mention slug, got: %s", msg)
	}
	if !strings.Contains(msg, "404") {
		t.Errorf("error should mention 404, got: %s", msg)
	}
}

func TestRejectIdentitySet_ErrorIsUnwrappable(t *testing.T) {
	gh := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		APIURL:  "",
		BaseURL: "",
	}
	err := RejectIdentitySet(gh)
	if !errors.Is(err, ErrIdentitySetMismatch) {
		t.Errorf("expected ErrIdentitySetMismatch via errors.Is, got: %v", err)
	}
}

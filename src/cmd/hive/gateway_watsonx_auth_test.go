package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/watsonx"
)

// swapDefaultMinter points watsonx.DefaultMinter at endpoint for the duration
// of the test, restoring the shared process-wide minter afterwards — the same
// seam pkg/dashboard's gateway tests use, so cmd/hive never mints against the
// real IBM IAM host.
func swapDefaultMinter(t *testing.T, endpoint string) {
	t.Helper()
	orig := watsonx.DefaultMinter
	watsonx.DefaultMinter = watsonx.NewTokenMinterForTest(endpoint, nil)
	t.Cleanup(func() { watsonx.DefaultMinter = orig })
}

// On the watsonx path, resolveGatewayAuth must hand the agent the MINTED IAM
// bearer — never the raw IBM Cloud API key — and attach the project header
// when a project is configured. Leaking the raw key to the upstream request
// would both fail auth and put the long-lived key on the wire per-request.
func TestResolveGatewayAuthWatsonxMintsBearer(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"iam-bearer-token","expires_in":3600}`))
	}))
	defer iam.Close()
	swapDefaultMinter(t, iam.URL+"/identity/token")

	const keyEnv = "HIVE_TEST_WATSONX_KEY"
	t.Setenv(keyEnv, "raw-ibm-cloud-key")
	gw := &config.GatewayConfig{
		Name:      "watsonx",
		Kind:      config.GatewayKindWatsonx,
		APIKeyEnv: keyEnv,
		ProjectID: "proj-123",
	}

	key, headers := resolveGatewayAuth(gw, "scanner", "watsonx", restoreTestLogger())

	if key != "iam-bearer-token" {
		t.Errorf("key = %q, want the minted IAM bearer, never the raw API key", key)
	}
	if got := headers[watsonx.ProjectIDHeader]; got != "proj-123" {
		t.Errorf("headers[%s] = %q, want %q", watsonx.ProjectIDHeader, got, "proj-123")
	}
}

// The kind comparison is case-insensitive: an operator who writes
// `kind: WatsonX` must still get the IAM mint, not a silent raw-key
// passthrough that fails at the upstream with an undiagnosable 401.
func TestResolveGatewayAuthWatsonxKindCaseInsensitive(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"cased-bearer","expires_in":3600}`))
	}))
	defer iam.Close()
	swapDefaultMinter(t, iam.URL+"/identity/token")

	const keyEnv = "HIVE_TEST_WATSONX_CASED_KEY"
	t.Setenv(keyEnv, "raw-key")
	gw := &config.GatewayConfig{Name: "watsonx", Kind: "WatsonX", APIKeyEnv: keyEnv}

	key, headers := resolveGatewayAuth(gw, "scanner", "watsonx", restoreTestLogger())
	if key != "cased-bearer" {
		t.Errorf("key = %q, want the minted bearer for a case-varied watsonx kind", key)
	}
	if headers != nil {
		t.Errorf("headers = %v, want nil when no project_id is configured", headers)
	}
}

// When the IAM mint fails, resolveGatewayAuth must fall back to the RAW key
// (so watsonx returns a clear upstream 401) rather than dropping the route,
// and must still attach the project header. This is the documented contract
// in resolveGatewayAuth's failure branch.
func TestResolveGatewayAuthWatsonxMintFailureFallsBackToRawKey(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errorMessage":"key rejected"}`, http.StatusBadRequest)
	}))
	defer iam.Close()
	swapDefaultMinter(t, iam.URL+"/identity/token")

	const keyEnv = "HIVE_TEST_WATSONX_BAD_KEY"
	t.Setenv(keyEnv, "raw-rejected-key")
	gw := &config.GatewayConfig{
		Name:      "watsonx",
		Kind:      config.GatewayKindWatsonx,
		APIKeyEnv: keyEnv,
		ProjectID: "proj-456",
	}

	key, headers := resolveGatewayAuth(gw, "scanner", "watsonx", restoreTestLogger())

	if key != "raw-rejected-key" {
		t.Errorf("key = %q, want the raw key passed through on mint failure", key)
	}
	if got := headers[watsonx.ProjectIDHeader]; got != "proj-456" {
		t.Errorf("headers[%s] = %q, want %q even when the mint failed", watsonx.ProjectIDHeader, got, "proj-456")
	}
}

// A watsonx gateway with no project_id must not fabricate the project header:
// watsonx deployments using space-scoped credentials pass the id elsewhere,
// and an empty X-IBM-Project-ID would be rejected upstream.
func TestResolveGatewayAuthWatsonxNoProjectIDNoHeaders(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"bearer-no-proj","expires_in":3600}`))
	}))
	defer iam.Close()
	swapDefaultMinter(t, iam.URL+"/identity/token")

	const keyEnv = "HIVE_TEST_WATSONX_NOPROJ_KEY"
	t.Setenv(keyEnv, "raw-key-2")
	gw := &config.GatewayConfig{Name: "watsonx", Kind: config.GatewayKindWatsonx, APIKeyEnv: keyEnv}

	key, headers := resolveGatewayAuth(gw, "scanner", "watsonx", restoreTestLogger())
	if key != "bearer-no-proj" {
		t.Errorf("key = %q, want the minted bearer", key)
	}
	if headers != nil {
		t.Errorf("headers = %v, want nil when project_id is empty", headers)
	}
}

// curatorConfigFromHive is the only bridge between the operator-facing
// knowledge_curator config block and the knowledge package's CuratorConfig.
// Every field must survive the copy: a silently dropped field here means an
// operator setting (e.g. auto_promote_threshold) is parsed but never acted on.
func TestCuratorConfigFromHiveCopiesEveryField(t *testing.T) {
	enabled := true
	in := config.KnowledgeCurator{
		Enabled:              &enabled,
		Schedule:             "0 3 * * *",
		ExtractFrom:          []string{"retro", "beads"},
		AutoPromoteThreshold: 0.75,
		PromoteFrom:          "hive",
		PromoteTo:            "org",
	}

	got := curatorConfigFromHive(in)

	want := knowledge.CuratorConfig{
		Enabled:              &enabled,
		Schedule:             "0 3 * * *",
		ExtractFrom:          []string{"retro", "beads"},
		AutoPromoteThreshold: 0.75,
		PromoteFrom:          "hive",
		PromoteTo:            "org",
	}
	if got.Enabled == nil || *got.Enabled != *want.Enabled {
		t.Errorf("Enabled = %v, want %v", got.Enabled, want.Enabled)
	}
	if got.Schedule != want.Schedule {
		t.Errorf("Schedule = %q, want %q", got.Schedule, want.Schedule)
	}
	if len(got.ExtractFrom) != 2 || got.ExtractFrom[0] != "retro" || got.ExtractFrom[1] != "beads" {
		t.Errorf("ExtractFrom = %v, want %v", got.ExtractFrom, want.ExtractFrom)
	}
	if got.AutoPromoteThreshold != want.AutoPromoteThreshold {
		t.Errorf("AutoPromoteThreshold = %v, want %v", got.AutoPromoteThreshold, want.AutoPromoteThreshold)
	}
	if got.PromoteFrom != want.PromoteFrom || got.PromoteTo != want.PromoteTo {
		t.Errorf("Promote from/to = %q/%q, want %q/%q", got.PromoteFrom, got.PromoteTo, want.PromoteFrom, want.PromoteTo)
	}
	if zero := curatorConfigFromHive(config.KnowledgeCurator{}); zero.Enabled != nil || zero.Schedule != "" || zero.ExtractFrom != nil {
		t.Errorf("zero-value input produced non-zero CuratorConfig: %+v", zero)
	}
}

package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Cluster IDs used by the fallback tests. fallbackClusterID stands in for the
// second public pool (lke648397 in production); unregisteredClusterID is a
// pool that has placeholders on disk but no clusters.json entry.
const (
	fallbackClusterID     = "lke648397"
	secondFallbackCluster = "aks-eu"
	unregisteredClusterID = "ghost-cluster"
)

// placeholderSpec is one available placeholder to seed for a test case.
type placeholderSpec struct {
	id      string
	cluster string
}

func seedPlaceholders(t *testing.T, specs []placeholderSpec) {
	t.Helper()
	for _, p := range specs {
		if err := saveSaaSHive(&SaaSHive{ID: p.id, Owner: hubAdminUsername, Status: statusAvailable, ClusterID: p.cluster}); err != nil {
			t.Fatal(err)
		}
	}
}

func newFallbackTestHub() *HubServer {
	return &HubServer{
		logger:    slog.Default(),
		hubSecret: testHubSecret,
		saveCh:    make(chan struct{}, 1),
		clusters: map[string]ClusterConfig{
			defaultClusterID:      *dynamicCluster(),
			fallbackClusterID:     {ID: fallbackClusterID, Name: "lke"},
			secondFallbackCluster: {ID: secondFallbackCluster, Name: "aks"},
			gpuClusterID:          {ID: gpuClusterID, Name: "gpu", PullOnly: true},
		},
	}
}

// TestAutoPickPlaceholderFallback is the table over (preferred pool empty /
// non-empty) x (other pools empty / non-empty) plus the hard pins that must
// never fall back.
func TestAutoPickPlaceholderFallback(t *testing.T) {
	tests := []struct {
		name         string
		preferred    string
		placeholders []placeholderSpec
		wantID       string
		wantPool     string
	}{
		{
			name:      "preferred non-empty, others non-empty -> preferred wins",
			preferred: defaultClusterID,
			placeholders: []placeholderSpec{
				{"oke-1", defaultClusterID},
				{"lke-1", fallbackClusterID},
				{"lke-2", fallbackClusterID},
			},
			wantID:   "oke-1",
			wantPool: defaultClusterID,
		},
		{
			name:         "preferred empty, others empty -> nothing",
			preferred:    defaultClusterID,
			placeholders: nil,
			wantID:       "",
			wantPool:     defaultClusterID,
		},
		{
			name:      "preferred empty, one other pool non-empty -> fall back",
			preferred: defaultClusterID,
			placeholders: []placeholderSpec{
				{"lke-2", fallbackClusterID},
				{"lke-1", fallbackClusterID},
			},
			wantID:   "lke-1",
			wantPool: fallbackClusterID,
		},
		{
			name:      "preferred empty, several pools -> most available first",
			preferred: defaultClusterID,
			placeholders: []placeholderSpec{
				{"aks-1", secondFallbackCluster},
				{"lke-1", fallbackClusterID},
				{"lke-2", fallbackClusterID},
			},
			wantID:   "lke-1",
			wantPool: fallbackClusterID,
		},
		{
			name:      "preferred empty, equal counts -> tie-break by cluster id",
			preferred: defaultClusterID,
			placeholders: []placeholderSpec{
				{"lke-1", fallbackClusterID},
				{"aks-1", secondFallbackCluster},
			},
			wantID:   "aks-1",
			wantPool: secondFallbackCluster,
		},
		{
			name:      "preferred empty, only GPU pool has placeholders -> no fallback onto GPU",
			preferred: defaultClusterID,
			placeholders: []placeholderSpec{
				{"gpu-1", gpuClusterID},
			},
			wantID:   "",
			wantPool: defaultClusterID,
		},
		{
			name:      "preferred empty, only unregistered cluster has placeholders -> skipped",
			preferred: defaultClusterID,
			placeholders: []placeholderSpec{
				{"ghost-1", unregisteredClusterID},
			},
			wantID:   "",
			wantPool: defaultClusterID,
		},
		{
			name:      "private request: GPU pool empty, public pools full -> stays pinned",
			preferred: gpuClusterID,
			placeholders: []placeholderSpec{
				{"oke-1", defaultClusterID},
				{"lke-1", fallbackClusterID},
			},
			wantID:   "",
			wantPool: gpuClusterID,
		},
		{
			name:      "private request: GPU pool non-empty -> GPU placeholder",
			preferred: gpuClusterID,
			placeholders: []placeholderSpec{
				{"gpu-1", gpuClusterID},
				{"oke-1", defaultClusterID},
			},
			wantID:   "gpu-1",
			wantPool: gpuClusterID,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()
			mkUser(t, hubAdminUsername)
			seedPlaceholders(t, tc.placeholders)

			s := newFallbackTestHub()
			gotID, gotPool := s.autoPickPlaceholder(tc.preferred)
			if gotID != tc.wantID || gotPool != tc.wantPool {
				t.Errorf("autoPickPlaceholder(%q) = (%q, %q), want (%q, %q)", tc.preferred, gotID, gotPool, tc.wantID, tc.wantPool)
			}
		})
	}
}

// TestHandleApproveProvisionPoolFallback drives the handler end to end: a
// public request with an empty default pool is served from another pool and
// the claimed hive keeps that pool's cluster_id; an admin's explicit pick is
// still strict (409 when the chosen placeholder is not available) and a fully
// empty fleet still 409s with a message that names the fallback scope.
func TestHandleApproveProvisionPoolFallback(t *testing.T) {
	pending := func(user string) *ProvisionRequest {
		return &ProvisionRequest{
			Username: user, Org: "acme", Repos: "repo", PrimaryRepo: "repo", ACMMLevel: 2,
			AuthMethod: "public", RequestedAt: time.Now().UTC().Format(time.RFC3339), Status: provisionStatusPending,
		}
	}

	tests := []struct {
		name         string
		placeholders []placeholderSpec
		body         string
		wantCode     int
		wantHive     string
		wantCluster  string
		wantBodyHas  string
	}{
		{
			name:         "auto-pick falls back to the other pool",
			placeholders: []placeholderSpec{{"lke-1", fallbackClusterID}},
			body:         "",
			wantCode:     http.StatusOK,
			wantHive:     "lke-1",
			wantCluster:  fallbackClusterID,
		},
		{
			name:         "auto-pick with every pool empty -> 409 naming the fallback scope",
			placeholders: nil,
			body:         "",
			wantCode:     http.StatusConflict,
			wantBodyHas:  "or any other registered pool",
		},
		{
			name:         "explicit pick of an unavailable placeholder stays strict even though another pool has capacity",
			placeholders: []placeholderSpec{{"lke-1", fallbackClusterID}},
			body:         `{"hive_id":"oke-missing"}`,
			wantCode:     http.StatusConflict,
			wantBodyHas:  "selected placeholder is not available",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()
			mkUser(t, hubAdminUsername)
			seedPlaceholders(t, tc.placeholders)
			if err := saveProvisionRequest(pending("bob")); err != nil {
				t.Fatal(err)
			}

			s := newFallbackTestHub()
			rec := httptest.NewRecorder()
			req := setPathValue(reqWithUser(http.MethodPut, "/approve-prov", tc.body, hubAdminUsername), "username", "bob")
			s.handleApproveProvision(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantBodyHas != "" && !strings.Contains(rec.Body.String(), tc.wantBodyHas) {
				t.Errorf("body = %s, want it to contain %q", rec.Body.String(), tc.wantBodyHas)
			}
			if tc.wantHive == "" {
				return
			}
			got := loadSaaSHive(tc.wantHive)
			if got == nil {
				t.Fatalf("hive %q missing after approve", tc.wantHive)
			}
			if got.Owner != "bob" || got.Status != statusAssigned {
				t.Errorf("hive %q owner/status = %q/%q, want bob/%s", tc.wantHive, got.Owner, got.Status, statusAssigned)
			}
			if got.ClusterID != tc.wantCluster {
				t.Errorf("hive %q ClusterID = %q, want %q (fallback must keep the placeholder's real pool)", tc.wantHive, got.ClusterID, tc.wantCluster)
			}
		})
	}
}

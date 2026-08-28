package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func engagementTestHub() *HubServer {
	return &HubServer{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestMarkUsersEngaged_BasicMark(t *testing.T) {
	s := engagementTestHub()
	s.markUsersEngaged([]string{"alice", "bob"})

	s.liveHiveUsersMu.RLock()
	defer s.liveHiveUsersMu.RUnlock()
	if len(s.engagedHiveUsers) != 2 {
		t.Fatalf("engagedHiveUsers has %d entries, want 2", len(s.engagedHiveUsers))
	}
	for _, name := range []string{"alice", "bob"} {
		if _, ok := s.engagedHiveUsers[name]; !ok {
			t.Errorf("user %q not in engagedHiveUsers", name)
		}
	}
}

func TestMarkUsersEngaged_SkipsInvalid(t *testing.T) {
	s := engagementTestHub()
	s.markUsersEngaged([]string{"", "../etc/passwd", "valid-user"})

	s.liveHiveUsersMu.RLock()
	defer s.liveHiveUsersMu.RUnlock()
	if len(s.engagedHiveUsers) != 1 {
		t.Fatalf("engagedHiveUsers has %d entries, want 1", len(s.engagedHiveUsers))
	}
}

func TestMarkUsersEngaged_EmptySliceNoOp(t *testing.T) {
	s := engagementTestHub()
	s.markUsersEngaged(nil)
	s.markUsersEngaged([]string{})

	s.liveHiveUsersMu.RLock()
	defer s.liveHiveUsersMu.RUnlock()
	if len(s.engagedHiveUsers) != 0 {
		t.Error("empty call should not create map entries")
	}
}

func TestEngagedHiveUsernames_ReturnsFreshOnly(t *testing.T) {
	s := engagementTestHub()
	s.markUsersEngaged([]string{"alice", "bob"})

	s.liveHiveUsersMu.Lock()
	s.engagedHiveUsers["bob"] = time.Now().Add(-2 * liveHiveUserStaleness)
	s.liveHiveUsersMu.Unlock()

	engaged := s.engagedHiveUsernames()
	if len(engaged) != 1 || engaged[0] != "alice" {
		t.Errorf("engagedHiveUsernames() = %v, want [alice]", engaged)
	}

	s.liveHiveUsersMu.RLock()
	_, bobExists := s.engagedHiveUsers["bob"]
	s.liveHiveUsersMu.RUnlock()
	if bobExists {
		t.Error("stale engaged entry should have been pruned")
	}
}

func TestEngagedHiveUsernames_ReturnsSorted(t *testing.T) {
	s := engagementTestHub()
	s.markUsersEngaged([]string{"charlie", "alice", "bob"})
	engaged := s.engagedHiveUsernames()
	if len(engaged) != 3 || engaged[0] != "alice" || engaged[1] != "bob" || engaged[2] != "charlie" {
		t.Errorf("engagedHiveUsernames() = %v, want sorted", engaged)
	}
}

func TestEngagedHiveUsernames_EmptyOnNoEngagement(t *testing.T) {
	s := engagementTestHub()
	engaged := s.engagedHiveUsernames()
	if len(engaged) != 0 {
		t.Errorf("engagedHiveUsernames() = %v, want empty", engaged)
	}
}

func TestIsUserEngagedNow_TrueForFreshUser(t *testing.T) {
	s := engagementTestHub()
	s.markUsersEngaged([]string{"alice"})
	if !s.isUserEngagedNow("alice") {
		t.Error("isUserEngagedNow(alice) = false, want true")
	}
}

func TestIsUserEngagedNow_FalseForStaleUser(t *testing.T) {
	s := engagementTestHub()
	s.markUsersEngaged([]string{"alice"})
	s.liveHiveUsersMu.Lock()
	s.engagedHiveUsers["alice"] = time.Now().Add(-2 * liveHiveUserStaleness)
	s.liveHiveUsersMu.Unlock()
	if s.isUserEngagedNow("alice") {
		t.Error("isUserEngagedNow(alice) = true, want false")
	}
}

func TestIsUserEngagedNow_FalseForUnknownUser(t *testing.T) {
	s := engagementTestHub()
	if s.isUserEngagedNow("nobody") {
		t.Error("isUserEngagedNow(nobody) = true, want false")
	}
}

func TestApplyUserLastActions_UpdatesLastActionAt(t *testing.T) {
	orig := saasUsersDir
	saasUsersDir = filepath.Join(t.TempDir(), "users")
	os.MkdirAll(saasUsersDir, 0o755)
	t.Cleanup(func() { saasUsersDir = orig })

	saveSaaSUser(&SaaSUser{GitHubUsername: "alice"})

	s := engagementTestHub()
	ts := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	s.applyUserLastActions(map[string]string{"alice": ts})

	u := loadSaaSUser("alice")
	if u == nil {
		t.Fatal("alice not found")
	}
	if u.LastActionAt != ts {
		t.Errorf("LastActionAt = %q, want %q", u.LastActionAt, ts)
	}
}

func TestApplyUserLastActions_NeverMovesBackward(t *testing.T) {
	orig := saasUsersDir
	saasUsersDir = filepath.Join(t.TempDir(), "users")
	os.MkdirAll(saasUsersDir, 0o755)
	t.Cleanup(func() { saasUsersDir = orig })

	newer := time.Now().UTC().Format(time.RFC3339)
	older := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", LastActionAt: newer})

	s := engagementTestHub()
	s.applyUserLastActions(map[string]string{"alice": older})

	u := loadSaaSUser("alice")
	if u.LastActionAt != newer {
		t.Errorf("LastActionAt moved backward: %q, want %q", u.LastActionAt, newer)
	}
}

func TestApplyUserLastActions_SkipsUnknownUser(t *testing.T) {
	orig := saasUsersDir
	saasUsersDir = filepath.Join(t.TempDir(), "users")
	os.MkdirAll(saasUsersDir, 0o755)
	t.Cleanup(func() { saasUsersDir = orig })

	s := engagementTestHub()
	ts := time.Now().UTC().Format(time.RFC3339)
	s.applyUserLastActions(map[string]string{"ghost": ts})

	if u := loadSaaSUser("ghost"); u != nil {
		t.Error("must not create unknown users")
	}
}

func TestApplyUserLastActions_SkipsInvalidTimestamps(t *testing.T) {
	orig := saasUsersDir
	saasUsersDir = filepath.Join(t.TempDir(), "users")
	os.MkdirAll(saasUsersDir, 0o755)
	t.Cleanup(func() { saasUsersDir = orig })

	saveSaaSUser(&SaaSUser{GitHubUsername: "alice"})

	s := engagementTestHub()
	s.applyUserLastActions(map[string]string{"alice": "not-a-date"})

	u := loadSaaSUser("alice")
	if u.LastActionAt != "" {
		t.Errorf("LastActionAt = %q, want empty", u.LastActionAt)
	}
}

func TestApplyUserLastActions_NilMapNoOp(t *testing.T) {
	s := engagementTestHub()
	s.applyUserLastActions(nil)
	s.applyUserLastActions(map[string]string{})
}

func TestApplyUserLastActions_SkipsInvalidUsernames(t *testing.T) {
	orig := saasUsersDir
	saasUsersDir = filepath.Join(t.TempDir(), "users")
	os.MkdirAll(saasUsersDir, 0o755)
	t.Cleanup(func() { saasUsersDir = orig })

	s := engagementTestHub()
	ts := time.Now().UTC().Format(time.RFC3339)
	s.applyUserLastActions(map[string]string{
		"":              ts,
		"../etc/passwd": ts,
		"has space":     ts,
	})
	entries, _ := os.ReadDir(saasUsersDir)
	if len(entries) != 0 {
		t.Errorf("expected no user files, found %d", len(entries))
	}
}

func TestFindContributeHive_ReturnsFirstEligible(t *testing.T) {
	// L3: keep this hermetic — the eligible hive's public DashboardURL must
	// resolve without an external DNS lookup.
	stubPrivateURLResolver(t, "example.com")
	s := engagementTestHub()
	s.registry.Hives = []RegistryEntry{
		{ID: "offline", Online: false, IsPublic: true, DashboardURL: "https://example.com", Owner: "u1"},
		{ID: "private", Online: true, IsPublic: false, DashboardURL: "https://example.com", Owner: "u2"},
		{ID: "no-url", Online: true, IsPublic: true, DashboardURL: "", Owner: "u3"},
		{ID: "no-owner", Online: true, IsPublic: true, DashboardURL: "https://example.com", Owner: ""},
		{ID: "eligible", Online: true, IsPublic: true, DashboardURL: "https://example.com", Owner: "u5"},
	}

	h := s.findContributeHive()
	if h == nil {
		t.Fatal("findContributeHive returned nil")
	}
	if h.ID != "eligible" {
		t.Errorf("got %q, want eligible", h.ID)
	}
}

func TestFindContributeHive_ReturnsNilWhenNoneEligible(t *testing.T) {
	s := engagementTestHub()
	s.registry.Hives = []RegistryEntry{
		{ID: "offline", Online: false, IsPublic: true, DashboardURL: "https://example.com", Owner: "u1"},
	}
	if h := s.findContributeHive(); h != nil {
		t.Errorf("expected nil, got %+v", h)
	}
}

func TestFindContributeHive_RejectsPrivateURLs(t *testing.T) {
	s := engagementTestHub()
	s.registry.Hives = []RegistryEntry{
		{ID: "local", Online: true, IsPublic: true, DashboardURL: "https://localhost:8080", Owner: "u1"},
		{ID: "internal", Online: true, IsPublic: true, DashboardURL: "https://192.168.1.1", Owner: "u2"},
	}
	if h := s.findContributeHive(); h != nil {
		t.Errorf("should reject private URLs, got %q", h.ID)
	}
}

func TestHandleContributeStatus_NoHiveAvailable(t *testing.T) {
	s := engagementTestHub()
	s.registry.Hives = nil

	req := httptest.NewRequest("GET", "/api/contribute/status", nil)
	w := httptest.NewRecorder()
	s.handleContributeStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["available"] != false {
		t.Errorf("available = %v, want false", resp["available"])
	}
}

func TestHandleContributeStatus_HiveAvailable(t *testing.T) {
	// L3: keep this hermetic — prod-1's public DashboardURL must resolve
	// without an external DNS lookup.
	stubPrivateURLResolver(t, "example.com")
	s := engagementTestHub()
	s.registry.Hives = []RegistryEntry{
		{ID: "prod-1", Name: "ProdHive", Org: "acme", Online: true, IsPublic: true,
			DashboardURL: "https://example.com", Owner: "op"},
	}

	req := httptest.NewRequest("GET", "/api/contribute/status", nil)
	w := httptest.NewRecorder()
	s.handleContributeStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["available"] != true {
		t.Errorf("available = %v, want true", resp["available"])
	}
	if resp["hive_id"] != "prod-1" {
		t.Errorf("hive_id = %v, want prod-1", resp["hive_id"])
	}
}

func TestHandleContributeProxy_NoHive(t *testing.T) {
	s := engagementTestHub()
	s.registry.Hives = nil

	req := httptest.NewRequest("POST", "/api/contribute/register", nil)
	w := httptest.NewRecorder()
	s.handleContributeProxy(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestIsPrivateURL_DetectsPrivateAddresses(t *testing.T) {
	privates := []string{
		"https://localhost:8080/path",
		"http://127.0.0.1:3000",
		"https://10.0.0.1",
		"http://172.16.0.1",
		"http://192.168.1.100/api",
		"https://169.254.0.1",
		"http://0.0.0.0",
	}
	for _, u := range privates {
		if !isPrivateURL(context.TODO(), u) {
			t.Errorf("isPrivateURL(%q) = false, want true", u)
		}
	}
}

func TestCreditActiveSessionTime_EngagedUsersGetEngagedSeconds(t *testing.T) {
	orig := saasUsersDir
	saasUsersDir = filepath.Join(t.TempDir(), "users")
	os.MkdirAll(saasUsersDir, 0o755)
	t.Cleanup(func() { saasUsersDir = orig })

	saveSaaSUser(&SaaSUser{GitHubUsername: "alice"})
	saveSaaSUser(&SaaSUser{GitHubUsername: "bob"})

	s := engagementTestHub()
	prev := &RegistryEntry{LastHeartbeat: time.Now().Add(-120 * time.Second).UTC().Format(time.RFC3339)}
	s.creditActiveSessionTime(prev, true, []string{"alice", "bob"}, []string{"alice"})

	alice := loadSaaSUser("alice")
	bob := loadSaaSUser("bob")
	if alice.SessionSeconds < 110 {
		t.Errorf("alice SessionSeconds = %d, want ~120", alice.SessionSeconds)
	}
	if alice.EngagedSeconds < 110 {
		t.Errorf("alice EngagedSeconds = %d, want ~120", alice.EngagedSeconds)
	}
	if alice.LastEngagedAt == "" {
		t.Error("alice LastEngagedAt should be set")
	}
	if bob.SessionSeconds < 110 {
		t.Errorf("bob SessionSeconds = %d, want ~120", bob.SessionSeconds)
	}
	if bob.EngagedSeconds != 0 {
		t.Errorf("bob EngagedSeconds = %d, want 0", bob.EngagedSeconds)
	}
}

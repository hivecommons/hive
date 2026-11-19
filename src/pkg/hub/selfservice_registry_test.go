package hub

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newSelfServiceTestStore returns a store on a temp file with a controllable
// clock.
func newSelfServiceTestStore(t *testing.T) (*selfServiceStore, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	st := newSelfServiceStore(filepath.Join(t.TempDir(), "hub-selfservice.json"))
	st.now = func() time.Time { return now }
	return st, &now
}

func TestSelfServiceStoreRegisterRoundTrip(t *testing.T) {
	st, _ := newSelfServiceTestStore(t)
	raw, err := st.register(SelfServiceHive{ID: "byo-alice-lab", Owner: "alice", Org: "alice", Repos: []string{"alice/lab"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if raw == "" {
		t.Fatal("register returned an empty key")
	}
	if !st.verify("byo-alice-lab", raw) {
		t.Fatal("fresh key does not verify")
	}

	reloaded := newSelfServiceStore(st.path)
	if err := reloaded.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	h, ok := reloaded.get("byo-alice-lab")
	if !ok {
		t.Fatal("record lost across reload")
	}
	if h.Owner != "alice" || len(h.Repos) != 1 || h.KeyHash != hashSelfServiceKey(raw) {
		t.Fatalf("record mangled across reload: %+v", h)
	}
	if !reloaded.verify("byo-alice-lab", raw) {
		t.Fatal("key does not verify after reload")
	}
}

func TestSelfServiceStoreRefusesBadInput(t *testing.T) {
	st, _ := newSelfServiceTestStore(t)
	if _, err := st.register(SelfServiceHive{ID: "hosted-alice", Owner: "alice"}); err != errSelfServiceNotByoID {
		t.Fatalf("non-byo ID accepted: err=%v", err)
	}
	if _, err := st.register(SelfServiceHive{ID: "byo-alice-lab"}); err != errSelfServiceEmptyOwner {
		t.Fatalf("empty owner accepted: err=%v", err)
	}
	if _, err := st.register(SelfServiceHive{ID: "byo-alice-lab", Owner: "alice"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := st.register(SelfServiceHive{ID: "BYO-Alice-Lab", Owner: "mallory"}); err != errSelfServiceExists {
		t.Fatalf("duplicate (case-folded) ID accepted: err=%v", err)
	}
	if h, _ := st.get("byo-alice-lab"); h.Owner != "alice" {
		t.Fatalf("duplicate registration overwrote the owner: %+v", h)
	}
	var nilStore *selfServiceStore
	if _, err := nilStore.register(SelfServiceHive{ID: "byo-x", Owner: "x"}); err != errSelfServiceNilStore {
		t.Fatalf("nil store register err=%v", err)
	}
	if nilStore.verify("byo-x", "anything") {
		t.Fatal("nil store verified a key")
	}
}

func TestSelfServiceStoreRotationOverlap(t *testing.T) {
	st, now := newSelfServiceTestStore(t)
	old, err := st.register(SelfServiceHive{ID: "byo-alice-lab", Owner: "alice"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	fresh, err := st.rotate("byo-alice-lab")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if fresh == old {
		t.Fatal("rotate returned the same key")
	}
	if !st.verify("byo-alice-lab", fresh) || !st.verify("byo-alice-lab", old) {
		t.Fatal("inside the overlap window both keys must verify")
	}
	*now = now.Add(selfServiceKeyRotationOverlap - time.Second)
	if !st.verify("byo-alice-lab", old) {
		t.Fatal("old key rejected before the overlap window ended")
	}
	*now = now.Add(2 * time.Second)
	if st.verify("byo-alice-lab", old) {
		t.Fatal("old key still verifies after the overlap window")
	}
	if !st.verify("byo-alice-lab", fresh) {
		t.Fatal("new key stopped verifying")
	}
	if _, err := st.rotate("byo-nobody"); err != errSelfServiceNotFound {
		t.Fatalf("rotate unknown: err=%v", err)
	}
}

func TestSelfServiceStoreRevoke(t *testing.T) {
	st, _ := newSelfServiceTestStore(t)
	raw, err := st.register(SelfServiceHive{ID: "byo-alice-lab", Owner: "alice"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	removed, err := st.revoke("byo-alice-lab")
	if err != nil || !removed {
		t.Fatalf("revoke: removed=%v err=%v", removed, err)
	}
	if st.verify("byo-alice-lab", raw) {
		t.Fatal("revoked key still verifies")
	}
	if removed, _ := st.revoke("byo-alice-lab"); removed {
		t.Fatal("second revoke reported a removal")
	}
	reloaded := newSelfServiceStore(st.path)
	if err := reloaded.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(reloaded.list()) != 0 {
		t.Fatal("revoked entry survived on disk")
	}
}

// TestSelfServiceStoreNeverPersistsRawKey is the invariant the design rests on:
// leaked hub state must not let anyone reconstruct a key.
func TestSelfServiceStoreNeverPersistsRawKey(t *testing.T) {
	st, _ := newSelfServiceTestStore(t)
	raw, err := st.register(SelfServiceHive{ID: "byo-alice-lab", Owner: "alice"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	rotated, err := st.rotate("byo-alice-lab")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	data, err := os.ReadFile(st.path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	for name, key := range map[string]string{"registered": raw, "rotated": rotated} {
		if strings.Contains(string(data), key) {
			t.Fatalf("%s raw key persisted to disk", name)
		}
	}
	if !strings.Contains(string(data), hashSelfServiceKey(rotated)) {
		t.Fatal("store does not hold the hash of the current key")
	}
	info, err := os.Stat(st.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != selfServiceStoreFileMode {
		t.Fatalf("store file mode = %o, want %o", perm, selfServiceStoreFileMode)
	}
}

// newSelfServiceHeartbeatHub is newHeartbeatHub with a master secret set (so
// the bearer check runs), no master generations (single-master path), a
// self-service store on a temp file and a captured logger.
func newSelfServiceHeartbeatHub(t *testing.T) (*HubServer, *bytes.Buffer) {
	t.Helper()
	s := newHeartbeatHub()
	s.hubSecret = testHubSecret
	s.selfService = newSelfServiceStore(filepath.Join(t.TempDir(), "hub-selfservice.json"))
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return s, &buf
}

func postHeartbeatWithBearer(t *testing.T, s *HubServer, hiveID, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(`{"hive_id":"`+hiveID+`","org":"alice"}`))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.handleHeartbeat(rec, req)
	return rec
}

// TestSelfServiceHeartbeatBearerIsBoundToOneHive covers the four F2 invariants
// for the byo- lane through the real handler.
func TestSelfServiceHeartbeatBearerIsBoundToOneHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, logBuf := newSelfServiceHeartbeatHub(t)

	const id = "byo-alice-lab"
	raw, err := s.selfService.register(SelfServiceHive{ID: id, Owner: "alice"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.selfService.register(SelfServiceHive{ID: "byo-bob-lab", Owner: "bob"}); err != nil {
		t.Fatalf("register bob: %v", err)
	}

	if rec := postHeartbeatWithBearer(t, s, id, raw); rec.Code != http.StatusOK {
		t.Fatalf("stored key: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postHeartbeatWithBearer(t, s, id, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: status=%d, want 401", rec.Code)
	}
	masterDerived := derivePerHiveKey(s.hubSecret, infoHeartbeatKey, id)
	if rec := postHeartbeatWithBearer(t, s, id, masterDerived); rec.Code != http.StatusUnauthorized {
		t.Fatalf("master-derived key authenticated a byo- hive: status=%d", rec.Code)
	}
	if rec := postHeartbeatWithBearer(t, s, id, s.hubSecret); rec.Code != http.StatusUnauthorized {
		t.Fatalf("raw master authenticated a byo- hive: status=%d", rec.Code)
	}
	if rec := postHeartbeatWithBearer(t, s, "byo-bob-lab", raw); rec.Code != http.StatusUnauthorized {
		t.Fatalf("alice's key authenticated bob's byo- hive: status=%d", rec.Code)
	}
	if rec := postHeartbeatWithBearer(t, s, "alice-lab", raw); rec.Code != http.StatusUnauthorized {
		t.Fatalf("self-service key authenticated an operator-listed hive: status=%d", rec.Code)
	}
	if rec := postHeartbeatWithBearer(t, s, "hosted-alice-lab", raw); rec.Code != http.StatusUnauthorized {
		t.Fatalf("self-service key authenticated a hosted hive: status=%d", rec.Code)
	}
	// The operator-listed lane is untouched: a master-derived key still works
	// for a non-byo ID.
	if rec := postHeartbeatWithBearer(t, s, "alice-lab", derivePerHiveKey(s.hubSecret, infoHeartbeatKey, "alice-lab")); rec.Code != http.StatusOK {
		t.Fatalf("master-derived key for an operator-listed hive: status=%d body=%s", rec.Code, rec.Body.String())
	}

	if strings.Contains(logBuf.String(), raw) {
		t.Fatal("raw self-service key appeared in hub logs")
	}
	if strings.Contains(logBuf.String(), s.hubSecret) {
		t.Fatal("master secret appeared in hub logs")
	}
}

func TestSelfServiceHeartbeatResponseCarriesNoKeyMaterial(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, _ := newSelfServiceHeartbeatHub(t)
	const id = "byo-alice-lab"
	raw, err := s.selfService.register(SelfServiceHive{ID: id, Owner: "alice"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	s.pendingGitHubAppConfigs[id] = &HeartbeatGitHubAppConfig{AppID: 42, InstallationID: 7, PrivateKey: "-----BEGIN KEY-----"}
	rec := postHeartbeatWithBearer(t, s, id, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for name, needle := range map[string]string{
		"raw key":       raw,
		"key hash":      hashSelfServiceKey(raw),
		"master secret": s.hubSecret,
		"App key":       "BEGIN KEY",
	} {
		if strings.Contains(body, needle) {
			t.Fatalf("%s appeared in the heartbeat response", name)
		}
	}
}

func TestSelfServiceHeartbeatStampsAcceptedBeatOnly(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s, _ := newSelfServiceHeartbeatHub(t)
	const id = "byo-alice-lab"
	raw, err := s.selfService.register(SelfServiceHive{ID: id, Owner: "alice"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if rec := postHeartbeatWithBearer(t, s, id, "wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status=%d", rec.Code)
	}
	if h, _ := s.selfService.get(id); !h.LastAcceptedHeartbeatAt.IsZero() {
		t.Fatal("a rejected heartbeat stamped LastAcceptedHeartbeatAt")
	}
	if rec := postHeartbeatWithBearer(t, s, id, raw); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	h, _ := s.selfService.get(id)
	if h.LastAcceptedHeartbeatAt.IsZero() {
		t.Fatal("accepted heartbeat did not stamp LastAcceptedHeartbeatAt")
	}
	reloaded := newSelfServiceStore(s.selfService.path)
	if err := reloaded.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if rh, _ := reloaded.get(id); rh.LastAcceptedHeartbeatAt.IsZero() {
		t.Fatal("first accepted heartbeat stamp was not persisted")
	}
}

func TestSelfServiceStoreStampPersistIsDebounced(t *testing.T) {
	st, now := newSelfServiceTestStore(t)
	if _, err := st.register(SelfServiceHive{ID: "byo-alice-lab", Owner: "alice"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	first := *now
	if err := st.noteAcceptedHeartbeat("byo-alice-lab", first); err != nil {
		t.Fatal(err)
	}
	second := first.Add(selfServiceHeartbeatStampInterval / 2)
	if err := st.noteAcceptedHeartbeat("byo-alice-lab", second); err != nil {
		t.Fatal(err)
	}
	onDisk := newSelfServiceStore(st.path)
	if err := onDisk.load(); err != nil {
		t.Fatal(err)
	}
	if h, _ := onDisk.get("byo-alice-lab"); !h.LastAcceptedHeartbeatAt.Equal(first) {
		t.Fatalf("stamp inside the interval was persisted: disk=%v want %v", h.LastAcceptedHeartbeatAt, first)
	}
	if h, _ := st.get("byo-alice-lab"); !h.LastAcceptedHeartbeatAt.Equal(second) {
		t.Fatalf("in-memory stamp not current: %v", h.LastAcceptedHeartbeatAt)
	}
	third := first.Add(selfServiceHeartbeatStampInterval + time.Second)
	if err := st.noteAcceptedHeartbeat("byo-alice-lab", third); err != nil {
		t.Fatal(err)
	}
	if err := onDisk.load(); err != nil {
		t.Fatal(err)
	}
	if h, _ := onDisk.get("byo-alice-lab"); !h.LastAcceptedHeartbeatAt.Equal(third) {
		t.Fatalf("stamp past the interval was not persisted: disk=%v want %v", h.LastAcceptedHeartbeatAt, third)
	}
	if err := st.noteAcceptedHeartbeat("byo-unknown", third); err != nil {
		t.Fatalf("unknown ID stamp returned an error: %v", err)
	}
}

// TestSelfServiceStoreRegisterCappedConcurrent pins #18166 at the store: many
// concurrent registrations by one owner never exceed maxPerOwner, and another
// owner's hives do not count against it.
func TestSelfServiceStoreRegisterCappedConcurrent(t *testing.T) {
	st := newSelfServiceStore(filepath.Join(t.TempDir(), "hub-selfservice.json"))
	if _, err := st.registerCapped(SelfServiceHive{ID: "byo-bob--lab", Owner: "bob"}, 3); err != nil {
		t.Fatal(err)
	}
	const attempts, limit = 10, 3
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.registerCapped(SelfServiceHive{ID: fmt.Sprintf("byo-alice--h%d", i), Owner: "alice"}, limit)
		}(i)
	}
	wg.Wait()
	ok, capped := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, errSelfServiceCapReached):
			capped++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != limit || capped != attempts-limit {
		t.Fatalf("registered %d, capped %d; want %d and %d", ok, capped, limit, attempts-limit)
	}
	if n := len(st.list()); n != limit+1 {
		t.Fatalf("store holds %d hives, want %d", n, limit+1)
	}
}

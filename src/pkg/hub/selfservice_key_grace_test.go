package hub

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// graceTestPolicy is the grace policy with a window long enough to tell it
// apart from the legacy overlap.
var graceTestPolicy = selfServiceKeyRotationPolicy{window: 2 * time.Hour, replacePending: true}

func TestSelfServiceKeyGraceBothKeysInsideWindowOldRefusedAfter(t *testing.T) {
	st, now := newSelfServiceTestStore(t)
	old, err := st.register(SelfServiceHive{ID: "byo-alice--lab", Owner: "alice"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	fresh, info, err := st.rotateWithPolicy("byo-alice--lab", "", graceTestPolicy)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if info.replacedPending {
		t.Fatal("first rotation reported a replaced pending key")
	}
	if want := now.Add(graceTestPolicy.window); !info.oldKeyExpiresAt.Equal(want) {
		t.Fatalf("old key expires at %v, want %v", info.oldKeyExpiresAt, want)
	}
	if !st.verify("byo-alice--lab", old) || !st.verify("byo-alice--lab", fresh) {
		t.Fatal("inside the grace window both keys must verify")
	}
	// Past the legacy overlap but inside the grace window: still both.
	*now = now.Add(selfServiceKeyRotationOverlap + time.Minute)
	if !st.verify("byo-alice--lab", old) {
		t.Fatal("old key refused inside the grace window")
	}
	*now = now.Add(graceTestPolicy.window)
	if st.verify("byo-alice--lab", old) {
		t.Fatal("old key still verifies after the grace window")
	}
	if !st.verify("byo-alice--lab", fresh) {
		t.Fatal("new key stopped verifying after the grace window")
	}
}

func TestSelfServiceKeyGraceSecondRotationReplacesPending(t *testing.T) {
	st, now := newSelfServiceTestStore(t)
	old, err := st.register(SelfServiceHive{ID: "byo-alice--lab", Owner: "alice"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	pending, _, err := st.rotateWithPolicy("byo-alice--lab", "", graceTestPolicy)
	if err != nil {
		t.Fatalf("first rotate: %v", err)
	}
	*now = now.Add(30 * time.Minute)
	second, info, err := st.rotateWithPolicy("byo-alice--lab", "", graceTestPolicy)
	if err != nil {
		t.Fatalf("second rotate: %v", err)
	}
	if !info.replacedPending {
		t.Fatal("second rotation inside the window did not report replacing the pending key")
	}
	if !st.verify("byo-alice--lab", old) {
		t.Fatal("the original key (still deployed) was dropped by the second rotation")
	}
	if st.verify("byo-alice--lab", pending) {
		t.Fatal("the replaced pending key still verifies")
	}
	if !st.verify("byo-alice--lab", second) {
		t.Fatal("the newest key does not verify")
	}
	// The window restarts from the second rotation.
	*now = now.Add(graceTestPolicy.window - time.Second)
	if !st.verify("byo-alice--lab", old) {
		t.Fatal("old key refused before the restarted window ended")
	}
	*now = now.Add(2 * time.Second)
	if st.verify("byo-alice--lab", old) {
		t.Fatal("old key still verifies after the restarted window")
	}
	// A rotation after the window closed retires the current key as usual.
	third, info, err := st.rotateWithPolicy("byo-alice--lab", "", graceTestPolicy)
	if err != nil {
		t.Fatalf("third rotate: %v", err)
	}
	if info.replacedPending {
		t.Fatal("rotation after the window reported a replaced pending key")
	}
	if !st.verify("byo-alice--lab", second) || !st.verify("byo-alice--lab", third) {
		t.Fatal("after a fresh rotation the previous and new key must both verify")
	}
}

func TestSelfServiceKeyGraceOffKeepsLegacyRotation(t *testing.T) {
	t.Setenv(selfServiceKeyGraceEnv, "")
	t.Setenv(selfServiceKeyGraceWindowEnv, "48h")
	policy := currentSelfServiceKeyRotationPolicy()
	if policy.window != selfServiceKeyRotationOverlap || policy.replacePending {
		t.Fatalf("flag off policy = %+v, want the legacy overlap", policy)
	}
	st, _ := newSelfServiceTestStore(t)
	old, err := st.register(SelfServiceHive{ID: "byo-alice--lab", Owner: "alice"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	first, _, err := st.rotateWithPolicy("byo-alice--lab", "", policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, info, err := st.rotateWithPolicy("byo-alice--lab", "", policy); err != nil || info.replacedPending {
		t.Fatalf("legacy second rotate: info=%+v err=%v", info, err)
	}
	if st.verify("byo-alice--lab", old) {
		t.Fatal("legacy second rotation must retire the original key, as before")
	}
	if !st.verify("byo-alice--lab", first) {
		t.Fatal("legacy second rotation must keep the first rotated key in its overlap")
	}
}

func TestSelfServiceKeyGracePolicyFromEnv(t *testing.T) {
	cases := []struct {
		flag, window string
		want         time.Duration
	}{
		{"true", "", selfServiceKeyGraceWindowDefault},
		{"on", "90m", 90 * time.Minute},
		{"1", "not-a-duration", selfServiceKeyGraceWindowDefault},
		{"yes", "-5m", selfServiceKeyGraceWindowDefault},
		{"true", "9000h", selfServiceKeyGraceWindowCeiling},
	}
	for _, c := range cases {
		t.Setenv(selfServiceKeyGraceEnv, c.flag)
		t.Setenv(selfServiceKeyGraceWindowEnv, c.window)
		got := currentSelfServiceKeyRotationPolicy()
		if got.window != c.want || !got.replacePending {
			t.Errorf("flag=%q window=%q: policy %+v, want window %v with replacePending", c.flag, c.window, got, c.want)
		}
	}
}

func TestHubV1RotateKeyGraceWindow(t *testing.T) {
	env := newHubV1TestEnv(t)
	t.Setenv(selfServiceKeyGraceEnv, "true")
	t.Setenv(selfServiceKeyGraceWindowEnv, "3h")
	id, old := env.registerHappy(t)

	rotate := func() (string, string) {
		t.Helper()
		rec := env.v1(t, selfServiceTestUser, http.MethodPost, hubV1BasePath+"/registrations/"+id+"/keys", "", nil, env.s.handleHubV1RotateKey)
		if rec.Code != http.StatusCreated {
			t.Fatalf("v1 rotate: %d %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Key     string `json:"key"`
			Overlap string `json:"overlap"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Key, resp.Overlap
	}
	pending, overlap := rotate()
	if overlap != (3 * time.Hour).String() {
		t.Fatalf("overlap = %q, want the configured window", overlap)
	}
	*env.now = env.now.Add(selfServiceKeyRotationOverlap + time.Minute)
	if _, ok := env.s.verifyHeartbeatBearerGeneration(old, id); !ok {
		t.Fatal("old key refused inside the grace window")
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(pending, id); !ok {
		t.Fatal("new key refused inside the grace window")
	}
	newest, _ := rotate()
	if _, ok := env.s.verifyHeartbeatBearerGeneration(old, id); !ok {
		t.Fatal("second rotation dropped the original key")
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(pending, id); ok {
		t.Fatal("replaced pending key still authenticates")
	}
	*env.now = env.now.Add(3*time.Hour + time.Second)
	if _, ok := env.s.verifyHeartbeatBearerGeneration(old, id); ok {
		t.Fatal("old key still authenticates after the grace window")
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(newest, id); !ok {
		t.Fatal("newest key stopped authenticating")
	}

	logs := env.logs.String()
	if !strings.Contains(logs, "audit: selfservice_hive_rotated") || !strings.Contains(logs, "replaced_pending=true") || !strings.Contains(logs, "grace=true") {
		t.Fatalf("rotation audit line missing grace fields: %s", logs)
	}
	stored, _ := env.s.selfService.get(id)
	for _, secret := range []string{old, pending, newest, stored.KeyHash, stored.PrevKeyHash, hashSelfServiceKey(pending)} {
		if secret == "" {
			t.Fatal("empty secret under test")
		}
		if strings.Contains(logs, secret) {
			t.Fatal("a key or a key hash reached the logs")
		}
	}
}

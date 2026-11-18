package hub

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Store-level tests for the v1 registration protocol (#18642): per-repo proof
// times, the revision counter behind the ETag, and the in-place update.

func registerV1TestHive(t *testing.T, st *selfServiceStore) (SelfServiceHive, string) {
	t.Helper()
	proved := time.Date(2026, 11, 13, 11, 0, 0, 0, time.UTC)
	raw, err := st.register(SelfServiceHive{
		ID: "byo-alice--lab", Owner: "alice", Org: "alice",
		Repos: []string{"alice/lab", "alice/tools"}, DashboardURL: "https://lab.example.org/",
		OwnershipCheckedAt: proved,
		RepoProofs:         map[string]time.Time{"alice/lab": proved, "alice/tools": proved},
	})
	if err != nil {
		t.Fatal(err)
	}
	h, ok := st.get("byo-alice--lab")
	if !ok {
		t.Fatal("registered hive missing")
	}
	return h, raw
}

func TestSelfServiceRevisionStartsAtOneAndBumpsOnOwnerVisibleChanges(t *testing.T) {
	st, _ := newSelfServiceTestStore(t)
	h, _ := registerV1TestHive(t, st)
	if h.Revision != 1 {
		t.Fatalf("revision at create = %d, want 1", h.Revision)
	}
	if _, err := st.rotate(h.ID); err != nil {
		t.Fatal(err)
	}
	rotated, _ := st.get(h.ID)
	if rotated.Revision != 2 {
		t.Fatalf("revision after rotate = %d, want 2", rotated.Revision)
	}
	updated, err := st.update(h.ID, "", func(r *SelfServiceHive) error { r.DashboardURL = ""; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 3 {
		t.Fatalf("revision after update = %d, want 3", updated.Revision)
	}
}

// A live spoke heartbeats constantly. If that moved the ETag, an operator's
// If-Match could never survive, and the ETag would leak liveness.
func TestSelfServiceHeartbeatDoesNotBumpRevisionOrETag(t *testing.T) {
	st, now := newSelfServiceTestStore(t)
	h, _ := registerV1TestHive(t, st)
	etag := selfServiceETag(h)

	for i := 0; i < 3; i++ {
		*now = now.Add(selfServiceHeartbeatStampInterval + time.Second)
		if err := st.noteAcceptedHeartbeat(h.ID, *now); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := st.get(h.ID)
	if after.LastAcceptedHeartbeatAt.IsZero() {
		t.Fatal("heartbeat stamp was not recorded")
	}
	if after.Revision != h.Revision || selfServiceETag(after) != etag {
		t.Fatalf("heartbeats moved the revision to %d (ETag %s -> %s)", after.Revision, etag, selfServiceETag(after))
	}
	if !selfServiceETagMatches(after, etag) {
		t.Fatal("an ETag read before the heartbeats no longer matches")
	}
}

// Records written by a binary from before the field have no revision. They
// must load as 0 with a stable, valid ETag, and the first change moves it.
func TestSelfServiceRevisionFallbackForOldRecords(t *testing.T) {
	st, _ := newSelfServiceTestStore(t)
	h, _ := registerV1TestHive(t, st)

	// An old binary's file: no revision and no repo_proofs.
	data, err := json.Marshal(map[string]any{"hives": []map[string]any{{
		"id": h.ID, "owner": h.Owner, "repos": h.Repos, "key_hash": h.KeyHash,
		"created_at": h.CreatedAt, "ownership_checked_at": h.OwnershipCheckedAt,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "revision") {
		t.Fatal("fixture carries a revision")
	}
	if err := os.WriteFile(st.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := newSelfServiceStore(st.path)
	if err := reloaded.load(); err != nil {
		t.Fatal(err)
	}
	old, ok := reloaded.get(h.ID)
	if !ok {
		t.Fatal("old record lost")
	}
	if old.Revision != 0 || len(old.RepoProofs) != 0 {
		t.Fatalf("old record loaded as revision %d proofs %v", old.Revision, old.RepoProofs)
	}
	etag := selfServiceETag(old)
	if etag == "" || !selfServiceETagMatches(old, etag) {
		t.Fatalf("revision-0 ETag %q does not match its record", etag)
	}
	next, err := reloaded.update(h.ID, etag, func(r *SelfServiceHive) error { r.DashboardURL = "https://x.example.org/"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != 1 || selfServiceETag(next) == etag {
		t.Fatalf("first change after revision 0: revision %d, ETag unchanged %v", next.Revision, selfServiceETag(next) == etag)
	}
}

// The new fields are additive: a binary that predates them decodes the file
// and ignores them (it has no DisallowUnknownFields on the registry file).
func TestSelfServiceNewFieldsAreAdditiveJSON(t *testing.T) {
	st, _ := newSelfServiceTestStore(t)
	h, _ := registerV1TestHive(t, st)
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		ID    string   `json:"id"`
		Owner string   `json:"owner"`
		Repos []string `json:"repos"`
	}
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatalf("a reader of the old shape cannot decode the record: %v", err)
	}
	if old.ID != h.ID || len(old.Repos) != 2 {
		t.Fatalf("old-shape decode = %+v", old)
	}
	for _, field := range []string{`"revision"`, `"repo_proofs"`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("record JSON lacks %s", field)
		}
	}
}

func TestSelfServiceUpdatePreservesCredentialAndIdentity(t *testing.T) {
	st, now := newSelfServiceTestStore(t)
	h, raw := registerV1TestHive(t, st)
	*now = now.Add(time.Minute)
	if _, err := st.rotate(h.ID); err != nil { // gives the record a previous key too
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	beat := *now
	if err := st.noteAcceptedHeartbeat(h.ID, beat); err != nil {
		t.Fatal(err)
	}
	before, _ := st.get(h.ID)

	// A mutate that tries to rewrite everything an edit must not touch.
	after, err := st.update(h.ID, selfServiceETag(before), func(r *SelfServiceHive) error {
		r.ID, r.Owner = "byo-mallory--x", "mallory"
		r.CreatedAt = time.Unix(0, 0).UTC()
		r.KeyHash, r.PrevKeyHash = "attacker", "attacker"
		r.PrevKeyExpiresAt = time.Time{}
		r.LastAcceptedHeartbeatAt = time.Unix(0, 0).UTC()
		r.Revision = 999
		r.Repos = []string{"alice/docs"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.Owner != before.Owner || !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("update changed identity: %+v", after)
	}
	if after.KeyHash != before.KeyHash || after.PrevKeyHash != before.PrevKeyHash || !after.PrevKeyExpiresAt.Equal(before.PrevKeyExpiresAt) {
		t.Fatal("update changed the key hashes")
	}
	if !after.LastAcceptedHeartbeatAt.Equal(beat) {
		t.Fatalf("update lost the heartbeat stamp: %v, want %v", after.LastAcceptedHeartbeatAt, beat)
	}
	if after.Revision != before.Revision+1 {
		t.Fatalf("revision = %d, want %d", after.Revision, before.Revision+1)
	}
	if len(after.Repos) != 1 || after.Repos[0] != "alice/docs" {
		t.Fatalf("the permitted change was not applied: %v", after.Repos)
	}
	if !st.verify(h.ID, raw) {
		// raw was the key before the rotate; it is the previous key now and
		// must still verify inside the overlap.
		t.Fatal("previous key stopped verifying after the update")
	}
	// Persisted, not only in memory.
	reloaded := newSelfServiceStore(st.path)
	if err := reloaded.load(); err != nil {
		t.Fatal(err)
	}
	if got, _ := reloaded.get(h.ID); got.Revision != after.Revision || got.KeyHash != before.KeyHash {
		t.Fatalf("persisted record = %+v", got)
	}
}

func TestSelfServiceUpdateRefusals(t *testing.T) {
	st, _ := newSelfServiceTestStore(t)
	h, _ := registerV1TestHive(t, st)
	noop := func(*SelfServiceHive) error { return nil }

	if _, err := st.update("byo-nobody--x", "", noop); !errors.Is(err, errSelfServiceNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	for _, stale := range []string{`"nope"`, `W/` + selfServiceETag(h), `"` + strings.Repeat("0", 24) + `"`} {
		if _, err := st.update(h.ID, stale, noop); !errors.Is(err, errSelfServiceStale) {
			t.Fatalf("If-Match %q: %v, want stale", stale, err)
		}
	}
	// Each form is built from the record as it is NOW; every success bumps
	// the revision, so a tag captured once would be stale for the next form.
	accepted := []func(SelfServiceHive) string{
		func(SelfServiceHive) string { return "" },
		func(SelfServiceHive) string { return "*" },
		func(cur SelfServiceHive) string { return selfServiceETag(cur) },
		func(cur SelfServiceHive) string { return `"nope", ` + selfServiceETag(cur) },
	}
	for i, build := range accepted {
		cur, _ := st.get(h.ID)
		if _, err := st.update(h.ID, build(cur), noop); err != nil {
			t.Fatalf("accepted If-Match form %d refused: %v", i, err)
		}
	}
	h, _ = st.get(h.ID)

	// A mutate error leaves the record as it was.
	before, _ := st.get(h.ID)
	boom := errors.New("boom")
	if _, err := st.update(h.ID, "", func(r *SelfServiceHive) error { r.DashboardURL = "x"; return boom }); !errors.Is(err, boom) {
		t.Fatalf("mutate error = %v", err)
	}
	after, _ := st.get(h.ID)
	if after.DashboardURL != before.DashboardURL || after.Revision != before.Revision {
		t.Fatalf("a failed mutate changed the record: %+v", after)
	}
}

func TestSelfServiceRotateAndRevokeIfMatch(t *testing.T) {
	st, _ := newSelfServiceTestStore(t)
	h, _ := registerV1TestHive(t, st)
	if _, err := st.rotateIfMatch(h.ID, `"stale"`); !errors.Is(err, errSelfServiceStale) {
		t.Fatalf("rotate with a stale ETag: %v", err)
	}
	if _, err := st.revokeIfMatch(h.ID, `"stale"`); !errors.Is(err, errSelfServiceStale) {
		t.Fatalf("revoke with a stale ETag: %v", err)
	}
	if _, ok := st.get(h.ID); !ok {
		t.Fatal("a refused revoke removed the record")
	}
	if _, err := st.rotateIfMatch(h.ID, selfServiceETag(h)); err != nil {
		t.Fatalf("rotate with the current ETag: %v", err)
	}
	cur, _ := st.get(h.ID)
	if removed, err := st.revokeIfMatch(h.ID, selfServiceETag(cur)); err != nil || !removed {
		t.Fatalf("revoke with the current ETag: %v %v", removed, err)
	}
}

func TestSelfServiceETagIsStrongAndKeyBlind(t *testing.T) {
	h := SelfServiceHive{ID: "byo-alice--lab", CreatedAt: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC), Revision: 4, KeyHash: "aaaa"}
	tag := selfServiceETag(h)
	if !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) || strings.HasPrefix(tag, "W/") {
		t.Fatalf("ETag %q is not a strong tag", tag)
	}
	other := h
	other.KeyHash = "bbbb"
	if selfServiceETag(other) != tag {
		t.Fatal("the ETag depends on the key hash")
	}
	other = h
	other.Revision++
	if selfServiceETag(other) == tag {
		t.Fatal("a revision change did not change the ETag")
	}
	other = h
	other.CreatedAt = other.CreatedAt.Add(time.Second)
	if selfServiceETag(other) == tag {
		t.Fatal("delete and re-register of the same ID would reuse an ETag")
	}
	if selfServiceETagMatches(h, "W/"+tag) {
		t.Fatal("a weak tag matched under strong comparison")
	}
}

func TestSelfServiceProofTimes(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	fallback := t0.Add(-time.Hour)
	prev := map[string]time.Time{"alice/lab": t0, "alice/gone": t0}
	got := selfServiceProofTimes(prev, []string{"Alice/Lab", "alice/new", "alice/old"}, fallback, t1, []string{"alice/new"})
	want := map[string]time.Time{"alice/lab": t0, "alice/new": t1, "alice/old": fallback}
	if len(got) != len(want) {
		t.Fatalf("proofs = %v, want %v", got, want)
	}
	for k, v := range want {
		if !got[k].Equal(v) {
			t.Errorf("proof[%s] = %v, want %v", k, got[k], v)
		}
	}
	if len(prev) != 2 || !prev["alice/lab"].Equal(t0) {
		t.Fatal("selfServiceProofTimes modified its input")
	}
	if none := selfServiceProofTimes(nil, []string{"alice/x"}, time.Time{}, t1, nil); len(none) != 0 {
		t.Fatalf("a repo with no proof and no fallback got a time: %v", none)
	}
}

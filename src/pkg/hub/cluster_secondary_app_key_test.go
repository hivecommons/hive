package hub

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests exist because #4815 changes a store ~55 live hives authenticate
// from. Every one of them asserts an INVARIANT of the existing primary path,
// not merely that the new code runs, and each carries a positive control so it
// cannot pass because nothing happened.

// --- INVARIANT 1: the primary key path is byte-identical to today ---

// TestPrimaryAppKeyPathIsUnchanged pins the EXACT string. It is deliberately
// hostile to refactoring: the path is the fleet's on-disk contract, and a
// "harmless" rename of it is an auth outage for every spoke that reads a key
// the hub no longer writes there. If this test needs updating, the change is
// wrong.
func TestPrimaryAppKeyPathIsUnchanged(t *testing.T) {
	orig := clusterAppKeyDir
	clusterAppKeyDir = "/data/saas/app-keys"
	t.Cleanup(func() { clusterAppKeyDir = orig })

	got, ok := clusterAppKeyPath("vllm-d")
	if !ok {
		t.Fatal("clusterAppKeyPath refused a valid cluster id")
	}
	const want = "/data/saas/app-keys/vllm-d.pem"
	if got != want {
		t.Fatalf("primary app key path = %q, want %q — this path is the live fleet's on-disk contract", got, want)
	}

	// POSITIVE CONTROL: the builder really is producing a path from its input,
	// so the assertion above is not passing against a constant.
	other, ok := clusterAppKeyPath("kubestellar-hive-ghe")
	if !ok || other != "/data/saas/app-keys/kubestellar-hive-ghe.pem" {
		t.Fatalf("positive control: path for a second cluster = %q, ok=%v", other, ok)
	}
	if other == got {
		t.Fatal("positive control: two different cluster ids produced the same path")
	}
}

// TestSecondaryAppKeyPathIsDistinctFromPrimary proves the new layout lands
// ALONGSIDE the primary rather than on top of it — the single fact that makes
// holding two keys possible at all.
func TestSecondaryAppKeyPathIsDistinctFromPrimary(t *testing.T) {
	orig := clusterAppKeyDir
	clusterAppKeyDir = "/data/saas/app-keys"
	t.Cleanup(func() { clusterAppKeyDir = orig })

	primary, _ := clusterAppKeyPath("vllm-d")
	secondary, ok := secondaryAppKeyPath("vllm-d", 4729416)
	if !ok {
		t.Fatal("secondaryAppKeyPath refused a valid (cluster, app) pair")
	}
	if secondary == primary {
		t.Fatal("the secondary key path IS the primary path — a second-app upload would overwrite the fleet's key")
	}
	const want = "/data/saas/app-keys/vllm-d-app-4729416.pem"
	if secondary != want {
		t.Fatalf("secondary path = %q, want %q", secondary, want)
	}
	// Same directory: the atomic temp-then-rename requires the temp file to
	// land on the same filesystem as its target.
	if filepath.Dir(secondary) != filepath.Dir(primary) {
		t.Fatalf("secondary key is stored outside the key dir: %q", secondary)
	}
}

// --- INVARIANT 6: path traversal via the App ID component ---

// TestSecondaryAppKeyPathRejectsTraversal covers the NEW attack surface. The App
// ID is an int64, so the traversal cases that matter are the ones a caller can
// actually express; the cluster half is re-checked here so the reuse of
// clusterAppKeyPath's guards is asserted rather than assumed.
func TestSecondaryAppKeyPathRejectsTraversal(t *testing.T) {
	withTempAppKeyDir(t)

	cases := []struct {
		name      string
		clusterID string
		appID     int64
	}{
		{"parent traversal in cluster id", "../../etc/x", 4729416},
		{"slash in cluster id", "a/b", 4729416},
		{"dot cluster id", ".", 4729416},
		{"dotdot cluster id", "..", 4729416},
		{"empty cluster id", "", 4729416},
		{"whitespace cluster id", "   ", 4729416},
		{"zero app id", "vllm-d", 0},
		{"negative app id", "vllm-d", -1},
		{"traversal-shaped negative app id", "vllm-d", -4729416},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := secondaryAppKeyPath(tc.clusterID, tc.appID); ok {
				t.Fatalf("accepted a hostile path component: %q", got)
			}
			// The store must refuse too, not merely the path builder.
			if err := storeSecondaryAppKey(tc.clusterID, tc.appID, 5686, testAppKeyPEM(t)); err == nil {
				t.Fatal("storeSecondaryAppKey accepted a hostile path component")
			}
		})
	}

	// POSITIVE CONTROL: a legitimate pair IS accepted, so the rejections above
	// are not the function refusing everything.
	if _, ok := secondaryAppKeyPath("vllm-d", 4729416); !ok {
		t.Fatal("positive control: a legitimate (cluster, app) pair was rejected")
	}
}

// TestSecondaryAppKeyFilenameNeverEscapesTheStore is the belt-and-braces check:
// whatever the builder returns must be a direct child of the key dir.
func TestSecondaryAppKeyFilenameNeverEscapesTheStore(t *testing.T) {
	dir := withTempAppKeyDir(t)
	for _, appID := range []int64{1, 5686, 4729416, 9223372036854775807} {
		p, ok := secondaryAppKeyPath("oke-260812-8yiz", appID)
		if !ok {
			t.Fatalf("app %d: path refused", appID)
		}
		if filepath.Dir(p) != dir {
			t.Fatalf("app %d: path %q is not a direct child of %q", appID, p, dir)
		}
		if strings.Contains(filepath.Base(p), "/") || strings.Contains(filepath.Base(p), "..") {
			t.Fatalf("app %d: filename %q contains a path separator or traversal", appID, filepath.Base(p))
		}
	}
}

// --- INVARIANT 3: a second-App upload never disturbs the primary key ---

// TestSecondaryStoreLeavesPrimaryFingerprintUnchanged is the acceptance
// criterion named in the issue, at the store level.
func TestSecondaryStoreLeavesPrimaryFingerprintUnchanged(t *testing.T) {
	withTempAppKeyDir(t)
	const (
		clusterID    = "vllm-d"
		primaryAppID = int64(5686)
		secondAppID  = int64(4729416)
	)

	primaryPEM := testAppKeyPEM(t)
	if err := storeClusterAppKey(clusterID, primaryPEM); err != nil {
		t.Fatalf("store primary: %v", err)
	}
	beforeFP := clusterAppKeyFingerprint(clusterID)
	if beforeFP == "" {
		t.Fatal("primary key did not store")
	}

	secondPEM := testAppKeyPEM(t)
	if secondPEM == primaryPEM {
		t.Fatal("test setup generated the same key twice")
	}
	if err := storeSecondaryAppKey(clusterID, secondAppID, primaryAppID, secondPEM); err != nil {
		t.Fatalf("store secondary: %v", err)
	}

	// THE INVARIANT.
	if after := clusterAppKeyFingerprint(clusterID); after != beforeFP {
		t.Fatalf("primary key fingerprint changed from %q to %q — a second-app upload clobbered the fleet's key", beforeFP, after)
	}
	if loadClusterAppKey(clusterID) != strings.TrimSpace(primaryPEM) {
		t.Fatal("primary key material changed")
	}

	// POSITIVE CONTROL: the second key really WAS written and is readable, so
	// the invariant above did not hold merely because nothing happened.
	gotSecond := loadSecondaryAppKey(clusterID, secondAppID)
	if gotSecond != strings.TrimSpace(secondPEM) {
		t.Fatal("positive control: the secondary key was not stored or does not read back")
	}
	secondFP := secondaryAppKeyFingerprint(clusterID, secondAppID)
	if secondFP == "" || secondFP == beforeFP {
		t.Fatalf("positive control: secondary fingerprint = %q (primary %q)", secondFP, beforeFP)
	}
}

// --- INVARIANT 6: the new store has the same security properties ---

func TestSecondaryStorePreservesFileModes(t *testing.T) {
	dir := withTempAppKeyDir(t)
	// Remove the dir so the store has to create it, exercising the 0700 path.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := storeSecondaryAppKey("vllm-d", 4729416, 5686, testAppKeyPEM(t)); err != nil {
		t.Fatalf("store: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != fs.FileMode(clusterAppKeyDirMode) {
		t.Errorf("key dir mode = %v, want %v", got, fs.FileMode(clusterAppKeyDirMode))
	}
	p, _ := secondaryAppKeyPath("vllm-d", 4729416)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != fs.FileMode(clusterAppKeyFileMode) {
		t.Errorf("secondary key file mode = %v, want %v — key material must never be group/world readable", got, fs.FileMode(clusterAppKeyFileMode))
	}
	// No temp file left behind: a .tmp holding key material would outlive the
	// rename's guarantees.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp key file left behind: %s", e.Name())
		}
	}
}

func TestSecondaryStoreRejectsUnusableKeys(t *testing.T) {
	withTempAppKeyDir(t)
	bad := []string{
		"",
		"not a key",
		"-----BEGIN RSA PRIVATE KEY-----\nZZZ\n-----END RSA PRIVATE KEY-----",
	}
	for _, b := range bad {
		if err := storeSecondaryAppKey("vllm-d", 4729416, 5686, b); err == nil {
			t.Errorf("accepted an unusable key: %q", b)
		}
		if loadSecondaryAppKey("vllm-d", 4729416) != "" {
			t.Fatal("an unusable key was persisted")
		}
	}
	// POSITIVE CONTROL.
	if err := storeSecondaryAppKey("vllm-d", 4729416, 5686, testAppKeyPEM(t)); err != nil {
		t.Fatalf("positive control: a valid key was rejected: %v", err)
	}
}

// TestSecondaryStoreRefusesThePrimaryApp is the defence in depth behind the
// handler's rejection: even a future caller that skips the HTTP guard cannot
// fork a shadow copy of the primary key under a second name.
func TestSecondaryStoreRefusesThePrimaryApp(t *testing.T) {
	withTempAppKeyDir(t)
	const primaryAppID = int64(5686)
	if err := storeSecondaryAppKey("vllm-d", primaryAppID, primaryAppID, testAppKeyPEM(t)); err == nil {
		t.Fatal("storeSecondaryAppKey wrote a secondary copy of the cluster's PRIMARY app key")
	}
	if p, ok := secondaryAppKeyPath("vllm-d", primaryAppID); ok {
		if _, err := os.Stat(p); err == nil {
			t.Fatal("a shadow copy of the primary key exists on disk")
		}
	}
	// POSITIVE CONTROL: a genuinely different App is accepted.
	if err := storeSecondaryAppKey("vllm-d", 4729416, primaryAppID, testAppKeyPEM(t)); err != nil {
		t.Fatalf("positive control: a non-primary app was rejected: %v", err)
	}
}

// --- INVARIANT 2 + 3: the upload endpoint's contract ---

// TestPutClusterAppKeyExistingShapeIsUnchanged asserts the contract every live
// caller depends on: a body with NO for_app_id writes the primary file, at the
// primary path, with the right fingerprint — exactly as before this change.
func TestPutClusterAppKeyExistingShapeIsUnchanged(t *testing.T) {
	withTempAppKeyDir(t)
	keyPEM := testAppKeyPEM(t)
	wantFP, err := config.AppKeyFingerprint(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	s := newSecondaryAppTestServer(t)

	rec := putClusterAppKey(t, s, "vllm-d", clusterAppKeyRequest{PrivateKey: keyPEM})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	// Written to the PRIMARY path, not anywhere else.
	primaryPath, _ := clusterAppKeyPath("vllm-d")
	if _, statErr := os.Stat(primaryPath); statErr != nil {
		t.Fatalf("primary key file missing at %s: %v", primaryPath, statErr)
	}
	if got := clusterAppKeyFingerprint("vllm-d"); got != wantFP {
		t.Fatalf("primary fingerprint = %q, want %q", got, wantFP)
	}
	var status clusterAppKeyStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.HasKey || status.Fingerprint != wantFP {
		t.Fatalf("response = %+v, want has_key with fingerprint %q", status, wantFP)
	}
	// No new field is populated for a cluster with no second App, so the
	// response is byte-identical to today's for every existing caller.
	if status.SecondaryKeys != nil {
		t.Fatalf("secondary_keys populated on a cluster with no second app: %+v", status.SecondaryKeys)
	}
	if strings.Contains(rec.Body.String(), "secondary_keys") {
		t.Fatal("response JSON gained a secondary_keys field for a cluster that has none")
	}
}

// TestPutClusterAppKeySecondAppDoesNotTouchPrimary is the end-to-end form of the
// issue's acceptance criterion.
func TestPutClusterAppKeySecondAppDoesNotTouchPrimary(t *testing.T) {
	withTempAppKeyDir(t)
	s := newSecondaryAppTestServer(t)

	primaryPEM := testAppKeyPEM(t)
	if rec := putClusterAppKey(t, s, "vllm-d", clusterAppKeyRequest{PrivateKey: primaryPEM}); rec.Code != http.StatusOK {
		t.Fatalf("primary upload: %d %s", rec.Code, rec.Body.String())
	}
	primaryFP := clusterAppKeyFingerprint("vllm-d")
	if primaryFP == "" {
		t.Fatal("primary key did not store")
	}

	secondPEM := testAppKeyPEM(t)
	rec := putClusterAppKey(t, s, "vllm-d", clusterAppKeyRequest{
		ForAppID:   4729416,
		PrivateKey: secondPEM,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("secondary upload: %d %s", rec.Code, rec.Body.String())
	}

	// THE INVARIANT.
	if got := clusterAppKeyFingerprint("vllm-d"); got != primaryFP {
		t.Fatalf("primary fingerprint changed from %q to %q across a second-app upload", primaryFP, got)
	}

	// POSITIVE CONTROL: the second key landed and is reported.
	if secondaryAppKeyFingerprint("vllm-d", 4729416) == "" {
		t.Fatal("positive control: the second app key was not stored")
	}
	var status clusterAppKeyStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.SecondaryKeys) != 1 || status.SecondaryKeys[0].AppID != 4729416 {
		t.Fatalf("secondary_keys = %+v, want one entry for app 4729416", status.SecondaryKeys)
	}
	if status.Fingerprint != primaryFP {
		t.Fatalf("response reports primary fingerprint %q, want the unchanged %q", status.Fingerprint, primaryFP)
	}
}

// TestPutClusterAppKeyRejectsSecondUploadNamingThePrimaryApp is the guard that
// makes the silent overwrite impossible. Rejected, NOT applied.
func TestPutClusterAppKeyRejectsSecondUploadNamingThePrimaryApp(t *testing.T) {
	withTempAppKeyDir(t)
	s := newSecondaryAppTestServer(t)

	primaryPEM := testAppKeyPEM(t)
	if rec := putClusterAppKey(t, s, "vllm-d", clusterAppKeyRequest{PrivateKey: primaryPEM}); rec.Code != http.StatusOK {
		t.Fatalf("primary upload: %d", rec.Code)
	}
	primaryFP := clusterAppKeyFingerprint("vllm-d")

	// vllm-d's primary app is 5686. Naming it under for_app_id must fail.
	rec := putClusterAppKey(t, s, "vllm-d", clusterAppKeyRequest{
		ForAppID:   5686,
		PrivateKey: testAppKeyPEM(t),
	})
	if rec.Code == http.StatusOK {
		t.Fatal("a second-app upload naming the PRIMARY app was accepted — this is the silent overwrite #4815 exists to prevent")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d with a clear error (body %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "primary") {
		t.Fatalf("error does not explain the conflict: %s", rec.Body.String())
	}
	// Nothing was applied, anywhere.
	if got := clusterAppKeyFingerprint("vllm-d"); got != primaryFP {
		t.Fatalf("primary fingerprint changed to %q despite the rejection", got)
	}
	if secondaryAppKeyFingerprint("vllm-d", 5686) != "" {
		t.Fatal("a shadow secondary copy of the primary app's key was written despite the rejection")
	}

	// POSITIVE CONTROL: the same request shape for a DIFFERENT app succeeds, so
	// the rejection is about the app id and not about for_app_id being set.
	if ok := putClusterAppKey(t, s, "vllm-d", clusterAppKeyRequest{
		ForAppID:   4729416,
		PrivateKey: testAppKeyPEM(t),
	}); ok.Code != http.StatusOK {
		t.Fatalf("positive control: a legitimate second-app upload was rejected: %d %s", ok.Code, ok.Body.String())
	}
}

func TestPutClusterAppKeySecondAppRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name       string
		clusterID  string
		body       clusterAppKeyRequest
		wantStatus int
	}{
		{
			name:       "negative for_app_id",
			clusterID:  "vllm-d",
			body:       clusterAppKeyRequest{ForAppID: -1},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "app_id combined with for_app_id",
			clusterID:  "vllm-d",
			body:       clusterAppKeyRequest{ForAppID: 4729416, AppID: 9999},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "cluster with no primary app id",
			clusterID:  "no-app",
			body:       clusterAppKeyRequest{ForAppID: 4729416},
			wantStatus: http.StatusConflict,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTempAppKeyDir(t)
			s := newSecondaryAppTestServer(t)
			body := tc.body
			if body.PrivateKey == "" {
				body.PrivateKey = testAppKeyPEM(t)
			}
			rec := putClusterAppKey(t, s, tc.clusterID, body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if secondaryAppKeyFingerprint(tc.clusterID, 4729416) != "" {
				t.Fatal("a key was stored despite the rejection")
			}
		})
	}
}

// --- INVARIANT 7: no key material ever leaves the hub except to the spoke ---

// TestSecondaryAppKeyNeverAppearsInResponsesOrLogs sweeps every surface this
// change adds. The upload response, the inventory response and every log line
// the handlers emit are searched for the key's own bytes.
func TestSecondaryAppKeyNeverAppearsInResponsesOrLogs(t *testing.T) {
	withTempAppKeyDir(t)
	var logs strings.Builder
	s := newSecondaryAppTestServer(t)
	s.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	primaryPEM := testAppKeyPEM(t)
	secondPEM := testAppKeyPEM(t)
	putClusterAppKey(t, s, "vllm-d", clusterAppKeyRequest{PrivateKey: primaryPEM})
	putRec := putClusterAppKey(t, s, "vllm-d", clusterAppKeyRequest{ForAppID: 4729416, PrivateKey: secondPEM})

	getRec := httptest.NewRecorder()
	s.handleGetClusterAppKeys(getRec, httptest.NewRequest(http.MethodGet, "/api/saas/admin/cluster-app-keys", nil))

	// The PEM body without its armour lines — the actual secret bytes.
	secretBody := keyBodyLines(t, secondPEM)
	primaryBody := keyBodyLines(t, primaryPEM)

	surfaces := map[string]string{
		"upload response":    putRec.Body.String(),
		"inventory response": getRec.Body.String(),
		"logs":               logs.String(),
	}
	for name, text := range surfaces {
		if strings.Contains(text, "PRIVATE KEY") {
			t.Errorf("%s contains a PEM armour line", name)
		}
		for _, line := range secretBody {
			if strings.Contains(text, line) {
				t.Errorf("%s leaked secondary key material", name)
				break
			}
		}
		for _, line := range primaryBody {
			if strings.Contains(text, line) {
				t.Errorf("%s leaked primary key material", name)
				break
			}
		}
	}

	// POSITIVE CONTROL: the surfaces are not empty, and the search really does
	// find these strings when they ARE present.
	if logs.Len() == 0 || getRec.Body.Len() == 0 {
		t.Fatal("positive control: a surface under test was empty")
	}
	if len(secretBody) == 0 {
		t.Fatal("positive control: no key body lines to search for")
	}
	if !strings.Contains(secondPEM, secretBody[0]) {
		t.Fatal("positive control: the search string is not actually part of the key")
	}
	// And the non-secret fingerprint IS reported, so the omission above is the
	// key specifically, not the endpoint saying nothing.
	if !strings.Contains(getRec.Body.String(), secondaryAppKeyFingerprint("vllm-d", 4729416)) {
		t.Fatal("positive control: the inventory does not report the secondary fingerprint")
	}
}

// --- INVARIANT 4: decideAppKeySync is untouched ---

// TestDecideAppKeySyncUnchangedWithoutSecondApp re-runs the primary state
// machine across every input shape it distinguishes and pins the decision. It
// exists to fail loudly if this change ever reaches into decideAppKeySync: a
// hive with no second App must see byte-identical heartbeat behaviour.
func TestDecideAppKeySyncUnchangedWithoutSecondApp(t *testing.T) {
	const (
		clusterFP = "sha256:cccccccccccccccccccccccccccccccc"
		otherFP   = "sha256:dddddddddddddddddddddddddddddddd"
		appID     = int64(5686)
	)
	withKey := &clusterAppIdentity{AppID: appID, PrivateKey: "pem", Fingerprint: clusterFP}
	noKey := &clusterAppIdentity{AppID: appID}

	cases := []struct {
		name              string
		spokeFP           string
		hasPerHiveKey     bool
		hivePublicPinned  bool
		spokeOnWrongForge bool
		spokeAppID        int64
		identity          *clusterAppIdentity
		wantPush          bool
		wantPushKey       bool
		wantReason        string
	}{
		{"nil identity", clusterFP, false, false, false, appID, nil, false, false, appKeyReasonNoClusterKey},
		{"public pinned", otherFP, false, true, false, appID, withKey, false, false, appKeyReasonPublicHiveOnGHECluster},
		{"placeholder sentinel with key", otherFP, false, false, false, config.PlaceholderAppID, withKey, true, true, appKeyReasonPlaceholderAppID},
		{"placeholder sentinel without key", otherFP, false, false, false, config.PlaceholderAppID, noKey, true, false, appKeyReasonPlaceholderAppID},
		{"wrong forge app", otherFP, false, false, true, 4242, withKey, true, true, appKeyReasonWrongForgeApp},
		{"wrong forge app without key", otherFP, false, false, true, 4242, noKey, true, false, appKeyReasonWrongForgeApp},
		{"cluster has no key", otherFP, false, false, false, appID, noKey, false, false, appKeyReasonNoClusterKey},
		{"per-hive key already matches", clusterFP, true, false, false, appID, withKey, false, false, appKeyReasonMatch},
		{"per-hive key unusable for claimed app", otherFP, true, false, false, appID, withKey, true, true, appKeyReasonPerHiveUnusable},
		{"per-hive override", otherFP, true, false, false, 0, withKey, false, false, appKeyReasonPerHiveOverride},
		{"deliberate different app pin", otherFP, true, false, false, 4242, withKey, false, false, appKeyReasonDifferentApp},
		{"spoke has no key", "", false, false, false, appID, withKey, true, true, appKeyReasonSpokeHasNoKey},
		{"fingerprint mismatch", otherFP, false, false, false, appID, withKey, true, true, appKeyReasonMismatch},
		{"already matches", clusterFP, false, false, false, appID, withKey, false, false, appKeyReasonMatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideAppKeySync(tc.spokeFP, tc.hasPerHiveKey, tc.hivePublicPinned, tc.spokeOnWrongForge, tc.spokeAppID, tc.identity)
			if got.Push != tc.wantPush || got.PushKey != tc.wantPushKey || got.Reason != tc.wantReason {
				t.Fatalf("decision = {Push:%v PushKey:%v Reason:%q}, want {Push:%v PushKey:%v Reason:%q}",
					got.Push, got.PushKey, got.Reason, tc.wantPush, tc.wantPushKey, tc.wantReason)
			}
		})
	}

	// POSITIVE CONTROL: the table really does discriminate — at least one case
	// pushes and at least one does not.
	pushes, holds := 0, 0
	for _, tc := range cases {
		if tc.wantPush {
			pushes++
		} else {
			holds++
		}
	}
	if pushes == 0 || holds == 0 {
		t.Fatalf("positive control: table is degenerate (%d push, %d hold)", pushes, holds)
	}
	// COUNT FLOOR: if a branch is ever deleted from decideAppKeySync this table
	// must not silently shrink with it.
	if len(cases) < 14 {
		t.Fatalf("the primary decision table lost coverage: %d cases", len(cases))
	}
}

// --- INVARIANT 5 + 8: targeted delivery, never a broadcast ---

// TestHeartbeatUnchangedForHiveWithoutSecondApp is the "no regression for
// everyone else" assertion: the entire fleet today has SecondaryAppID == 0.
func TestHeartbeatUnchangedForHiveWithoutSecondApp(t *testing.T) {
	withTempAppKeyDir(t)
	withTempHivesDir(t)
	s := newSecondaryAppTestServer(t)

	// A hive with NO second App, on a cluster that nonetheless holds a
	// secondary key for another App — the state that would tempt a broadcast.
	if err := storeSecondaryAppKey("vllm-d", 4729416, 5686, testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	mustSaveHive(t, &SaaSHive{ID: "hive-a", ClusterID: "vllm-d"})

	got := s.secondaryAppKeyForHeartbeat(&HeartbeatPayload{HiveID: "hive-a"})
	if got != nil {
		t.Fatalf("a hive with no second app was handed a key for app %d — this is the cross-tenant broadcast reintroduced", got.AppID)
	}

	// POSITIVE CONTROL: assigning the App makes the SAME call deliver, so the
	// nil above is the authorization check and not a dead code path.
	mustSaveHive(t, &SaaSHive{ID: "hive-a", ClusterID: "vllm-d", SecondaryAppID: 4729416})
	if got := s.secondaryAppKeyForHeartbeat(&HeartbeatPayload{HiveID: "hive-a"}); got == nil {
		t.Fatal("positive control: an assigned hive received nothing")
	} else if got.AppID != 4729416 {
		t.Fatalf("positive control: delivered app %d, want 4729416", got.AppID)
	}
}

// TestSecondaryKeyIsNotDeliveredToAnUnauthorizedHive is the CWE-200/639
// regression test. It puts two hives on the same cluster, assigns the key to
// one, and proves the other cannot obtain it — including by REPORTING that it
// holds a stale copy, the one spoke-supplied input this path reads.
func TestSecondaryKeyIsNotDeliveredToAnUnauthorizedHive(t *testing.T) {
	withTempAppKeyDir(t)
	withTempHivesDir(t)
	s := newSecondaryAppTestServer(t)

	if err := storeSecondaryAppKey("vllm-d", 4729416, 5686, testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	mustSaveHive(t, &SaaSHive{ID: "authorized", ClusterID: "vllm-d", SecondaryAppID: 4729416})
	mustSaveHive(t, &SaaSHive{ID: "tenant-b", ClusterID: "vllm-d"})

	// tenant-b asserts every input it controls: its own hive_id, and a held-key
	// report claiming a STALE fingerprint for the App it wants.
	hostile := &HeartbeatPayload{
		HiveID:            "tenant-b",
		GitHubAppKeysHeld: map[string]string{"4729416": "sha256:stale"},
	}
	if got := s.secondaryAppKeyForHeartbeat(hostile); got != nil {
		t.Fatalf("an unauthorized hive obtained app %d's private key by asking for it", got.AppID)
	}

	// POSITIVE CONTROL: the authorized hive, with the identical hostile-shaped
	// held report, DOES receive it.
	authorized := &HeartbeatPayload{
		HiveID:            "authorized",
		GitHubAppKeysHeld: map[string]string{"4729416": "sha256:stale"},
	}
	if got := s.secondaryAppKeyForHeartbeat(authorized); got == nil {
		t.Fatal("positive control: the authorized hive received nothing")
	}
}

// TestSecondaryDeliveryNeverPopulatesAdditionalKeys pins invariant 5 directly:
// the removed broadcast producer stays removed.
func TestSecondaryDeliveryNeverPopulatesAdditionalKeys(t *testing.T) {
	withTempAppKeyDir(t)
	withTempHivesDir(t)
	s := newSecondaryAppTestServer(t)

	if err := storeSecondaryAppKey("vllm-d", 4729416, 5686, testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	mustSaveHive(t, &SaaSHive{ID: "hive-a", ClusterID: "vllm-d", SecondaryAppID: 4729416})

	key := s.secondaryAppKeyForHeartbeat(&HeartbeatPayload{HiveID: "hive-a"})
	if key == nil {
		t.Fatal("setup: no key delivered")
	}
	// The delivery is a SINGLE key on its own field. Assembling the response the
	// way handleHeartbeat does must leave AdditionalKeys nil.
	cfg := &HeartbeatGitHubAppConfig{}
	cfg.SecondaryKey = key
	if cfg.AdditionalKeys != nil {
		t.Fatal("AdditionalKeys was populated — the CWE-200/639 broadcast is back")
	}
	// And the field is singular by TYPE, so there is no shape in which it can
	// carry a second tenant's key.
	if cfg.SecondaryKey.AppID != 4729416 {
		t.Fatalf("delivered app %d, want 4729416", cfg.SecondaryKey.AppID)
	}
}

// TestDecideSecondaryAppKeySyncIsIdempotent mirrors decideAppKeySync's
// fingerprint-comparison contract: a key the spoke already holds is never
// re-pushed, so a steady-state hive produces no key traffic.
func TestDecideSecondaryAppKeySyncIsIdempotent(t *testing.T) {
	keyPEM := testAppKeyPEM(t)
	fp, err := config.AppKeyFingerprint(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	const appID = int64(4729416)
	appKey := fmt.Sprintf("%d", appID)

	cases := []struct {
		name        string
		pem         string
		appID       int64
		held        map[string]string
		wantDeliver bool
		wantReason  string
	}{
		{"no assignment", keyPEM, 0, nil, false, secondaryReasonNoAssignment},
		{"hub holds no key", "", appID, nil, false, secondaryReasonNoKey},
		{"unfingerprintable key", "-----BEGIN RSA PRIVATE KEY-----\nZZZ\n-----END RSA PRIVATE KEY-----", appID, nil, false, secondaryReasonNoKey},
		{"spoke holds nothing", keyPEM, appID, nil, true, secondaryReasonDeliver},
		{"spoke holds a stale key", keyPEM, appID, map[string]string{appKey: "sha256:stale"}, true, secondaryReasonDeliver},
		{"spoke holds another app's key", keyPEM, appID, map[string]string{"5686": fp}, true, secondaryReasonDeliver},
		{"spoke already holds it", keyPEM, appID, map[string]string{appKey: fp}, false, secondaryReasonHeld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideSecondaryAppKeySync(tc.pem, tc.appID, tc.held)
			if got.Deliver != tc.wantDeliver || got.Reason != tc.wantReason {
				t.Fatalf("decision = {Deliver:%v Reason:%q}, want {Deliver:%v Reason:%q}",
					got.Deliver, got.Reason, tc.wantDeliver, tc.wantReason)
			}
			// The decision never carries key material.
			if strings.Contains(got.Reason+got.ToFingerprint+got.HeldFingerprint, "PRIVATE KEY") {
				t.Fatal("the decision carries key material")
			}
		})
	}
}

// TestSecondaryAppAssignmentEndpoint covers the one path that writes
// SecondaryAppID.
func TestSecondaryAppAssignmentEndpoint(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	withTempAppKeyDir(t)
	s := newHandlerHub()
	s.clusters = secondaryAppTestClusters()
	mkUser(t, hubAdminUsername)
	mkUser(t, "bob")
	mustSaveHive(t, &SaaSHive{ID: "hive-a", ClusterID: "vllm-d"})

	// This field IS the authorization decision for key delivery, so anything
	// that can write it can direct key material at a hive. Non-admins may not.
	if rec := putSecondaryApp(t, s, "bob", "hive-a", 4729416); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: status = %d, want %d (%s)", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if h := loadSaaSHive("hive-a"); h.SecondaryAppID != 0 {
		t.Fatalf("a non-admin recorded a secondary app: %d", h.SecondaryAppID)
	}

	// Assigning the app this hive already authenticates as is refused: one App
	// cannot be both primary and secondary.
	if rec := putSecondaryApp(t, s, hubAdminUsername, "hive-a", 5686); rec.Code != http.StatusConflict {
		t.Fatalf("assigning the primary app: status = %d, want %d (%s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if h := loadSaaSHive("hive-a"); h.SecondaryAppID != 0 {
		t.Fatalf("secondary app was recorded despite the rejection: %d", h.SecondaryAppID)
	}

	// POSITIVE CONTROL: a genuine second App is recorded.
	if rec := putSecondaryApp(t, s, hubAdminUsername, "hive-a", 4729416); rec.Code != http.StatusOK {
		t.Fatalf("positive control: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if h := loadSaaSHive("hive-a"); h.SecondaryAppID != 4729416 {
		t.Fatalf("secondary_app_id = %d, want 4729416", h.SecondaryAppID)
	}

	// Clearing is explicit and reversible.
	if rec := putSecondaryApp(t, s, hubAdminUsername, "hive-a", 0); rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if h := loadSaaSHive("hive-a"); h.SecondaryAppID != 0 {
		t.Fatalf("secondary_app_id = %d after clear, want 0", h.SecondaryAppID)
	}

	// Unknown hive.
	if rec := putSecondaryApp(t, s, hubAdminUsername, "nope", 4729416); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown hive: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestSecondaryAppKeysForClusterIgnoresForeignFiles proves the inventory scan
// attributes a key only to the cluster whose name it was constructed from, and
// never mis-parses a primary key file as a secondary one.
func TestSecondaryAppKeysForClusterIgnoresForeignFiles(t *testing.T) {
	dir := withTempAppKeyDir(t)

	if err := storeClusterAppKey("vllm-d", testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	if err := storeClusterAppKey("vllm-d-app-999", testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	if err := storeSecondaryAppKey("vllm-d", 4729416, 5686, testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	if err := storeSecondaryAppKey("other", 5945, 5686, testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	// Non-canonical names that parse as integers but are not ours.
	for _, name := range []string{"vllm-d-app-0004729416.pem", "vllm-d-app-.pem", "vllm-d-app-x.pem"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(testAppKeyPEM(t)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := secondaryAppKeysForCluster("vllm-d")
	// "vllm-d-app-999.pem" is a PRIMARY key for a cluster literally named
	// "vllm-d-app-999", which happens to collide with the secondary naming.
	// It is included because the two are genuinely indistinguishable by name —
	// which is exactly why nothing in the delivery path resolves a cluster from
	// a filename. The assertion that matters is that OTHER clusters' keys and
	// non-canonical names never appear.
	for _, id := range got {
		if id == 5945 {
			t.Fatal("another cluster's secondary key was attributed to vllm-d")
		}
		if id == 4729416 {
			continue
		}
		if id != 999 {
			t.Fatalf("unexpected app id %d in the inventory (non-canonical filenames must be skipped)", id)
		}
	}
	found := false
	for _, id := range got {
		if id == 4729416 {
			found = true
		}
	}
	if !found {
		t.Fatal("positive control: the cluster's own secondary key is missing from the inventory")
	}
	if other := secondaryAppKeysForCluster("other"); len(other) != 1 || other[0] != 5945 {
		t.Fatalf("positive control: other cluster's inventory = %v, want [5945]", other)
	}
}

// --- helpers ---

// secondaryAppTestClusters is the fleet shape these tests reason about: a
// cluster with a primary App (vllm-d, app 5686 — the live GHE App ID), one with
// none, and a second cluster fronting the SAME App so cross-cluster attribution
// is exercised.
func secondaryAppTestClusters() map[string]ClusterConfig {
	return map[string]ClusterConfig{
		"vllm-d": {ID: "vllm-d", GitHubAppID: 5686},
		"no-app": {ID: "no-app"},
		"other":  {ID: "other", GitHubAppID: 5686},
	}
}

func newSecondaryAppTestServer(t *testing.T) *HubServer {
	t.Helper()
	return &HubServer{logger: appKeyTestLogger(), clusters: secondaryAppTestClusters()}
}

func putClusterAppKey(t *testing.T, s *HubServer, clusterID string, body clusterAppKeyRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut,
		"/api/saas/admin/cluster-app-keys/"+clusterID, strings.NewReader(string(raw)))
	req.SetPathValue("clusterID", clusterID)
	rec := httptest.NewRecorder()
	s.handlePutClusterAppKey(rec, req)
	return rec
}

func putSecondaryApp(t *testing.T, s *HubServer, user, hiveID string, appID int64) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(secondaryAppAssignmentRequest{AppID: appID})
	if err != nil {
		t.Fatal(err)
	}
	req := setPathValue(reqWithUser(http.MethodPut,
		"/api/saas/hives/"+hiveID+"/secondary-app", string(raw), user), "id", hiveID)
	rec := httptest.NewRecorder()
	s.handleSetHiveSecondaryApp(rec, req)
	return rec
}

// withTempHivesDir redirects the SaaS hive-meta store at a temp dir.
func withTempHivesDir(t *testing.T) string {
	t.Helper()
	orig := saasHivesDir
	dir := t.TempDir()
	saasHivesDir = dir
	t.Cleanup(func() { saasHivesDir = orig })
	return dir
}

func mustSaveHive(t *testing.T, h *SaaSHive) {
	t.Helper()
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("save hive %s: %v", h.ID, err)
	}
}

// keyBodyLines returns the base64 body lines of a PEM — the actual secret
// bytes, stripped of the armour that every PEM shares. Searching for these
// catches a leak that quoting only part of the key would hide.
func keyBodyLines(t *testing.T, pemData string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(pemData), "\n") {
		line = strings.TrimSpace(line)
		// Long enough to be unmistakably key material rather than a coincidence.
		const minSecretLineLen = 32
		if strings.HasPrefix(line, "-----") || len(line) < minSecretLineLen {
			continue
		}
		out = append(out, line)
	}
	return out
}

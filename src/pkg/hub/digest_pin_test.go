package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================
// digest_pin.go - rollback as a first-class run-state (#6290, #6267, #6268)
// ============================================================
//
// The Deployment is modelled by a scripted fake kubectl that records every
// invocation and persists the image its last `set image` wrote. Tests assert
// on THAT file - the image the Deployment holds - not on log lines, because
// the failure these tests guard is a rollback that silently reverts.

const (
	testPinDigestHex   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testPinDigest      = digestPrefix + testPinDigestHex
	testPinOtherDigest = digestPrefix + "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testPinHiveID      = "pinhive"
	testPinOwner       = "alice"
	testPinStranger    = "mallory"
)

// deploymentModel is the fake cluster: the image the Deployment holds and the
// log of every kubectl invocation the hub made.
type deploymentModel struct {
	imageFile string
	logFile   string
}

// installDigestPinKubectl puts a fake kubectl on PATH that appends its argv
// to logFile and, on `set image`, writes the "*=<image>" value to imageFile.
// The initial Deployment image is seeded so "unchanged" is a real assertion.
func installDigestPinKubectl(t *testing.T, initialImage string) deploymentModel {
	t.Helper()
	dir := t.TempDir()
	m := deploymentModel{
		imageFile: filepath.Join(dir, "deployment-image"),
		logFile:   filepath.Join(dir, "kubectl.log"),
	}
	if err := os.WriteFile(m.imageFile, []byte(initialImage), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$*" >> "$HIVE_TEST_KUBECTL_LOG"
case "$*" in
  *"set image"*)
    for a in "$@"; do
      case "$a" in
        \*=*) printf '%s' "${a#\*=}" > "$HIVE_TEST_DEPLOYMENT_IMAGE" ;;
      esac
    done
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TEST_KUBECTL_LOG", m.logFile)
	t.Setenv("HIVE_TEST_DEPLOYMENT_IMAGE", m.imageFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return m
}

func (m deploymentModel) image(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(m.imageFile)
	if err != nil {
		t.Fatalf("read deployment image: %v", err)
	}
	return string(data)
}

func (m deploymentModel) invocations(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(m.logFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read kubectl log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// newPinHub is a hub whose only cluster claims in-cluster reachability, so
// the pin handler takes the kubectl path (KubectlReachable) and the fake
// kubectl on PATH stands in for the cluster. It carries everything the
// heartbeat handler and triggerAutoUpgrades need too, so one hub can run the
// whole pin -> beat -> reconcile sequence.
func newPinHub() *HubServer {
	return &HubServer{
		logger:                  slog.Default(),
		hubSecret:               testHubSecret,
		keyGenerations:          legacyGenerationSet(testHubSecret),
		saveCh:                  make(chan struct{}, 1),
		hubGitHash:              "abc1234",
		hubGitBranch:            "v4",
		clusters:                map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", InCluster: true, Domain: "hive.example.test"}},
		heartbeatHealth:         make(map[string]*HeartbeatHealthEntry),
		heartbeatUpgrade:        make(map[string]string),
		heartbeatSwitchTag:      make(map[string]string),
		pendingWebhooks:         make(map[string]*pendingWebhookEntry),
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
		pendingGateways:         make(map[string]*HeartbeatGatewayConfig),
		hubBanners:              make(map[string]*HubBannerEntry),
	}
}

// stubSpokeImageExists makes every digest/tag "published" for the test.
func stubSpokeImageExists(t *testing.T) {
	t.Helper()
	old := spokeImageExists
	spokeImageExists = func(string, *slog.Logger) bool { return true }
	t.Cleanup(func() { spokeImageExists = old })
}

func postPin(t *testing.T, s *HubServer, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/pin", body, user), "id", testPinHiveID)
	s.handlePinDigest(rec, req)
	return rec
}

// postPinHeartbeat posts a beat for the pin hive with the per-hive bearer the
// handler requires whenever the hub carries a secret. newPinHub sets hubSecret
// (the owner-session cookie needs it), so unlike newHeartbeatHub's unauthenticated
// beats this fixture must present the credential a real spoke would.
func postPinHeartbeat(t *testing.T, s *HubServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", heartbeatBearer(testHubSecret, testPinHiveID))
	rec := httptest.NewRecorder()
	s.handleHeartbeat(rec, req)
	return rec
}

func postUnpin(t *testing.T, s *HubServer, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/unpin", body, user), "id", testPinHiveID)
	s.handleUnpinDigest(rec, req)
	return rec
}

// seedPinHive persists a stable-tracking hive owned by alice with a live
// registry entry on v4-latest, the state a real channel hive is in before an
// operator rolls it back.
func seedPinHive(t *testing.T, s *HubServer, autoUpgrade bool) {
	t.Helper()
	mkUser(t, testPinOwner)
	mkUser(t, testPinStranger)
	if err := saveSaaSHive(&SaaSHive{
		ID: testPinHiveID, Owner: testPinOwner, ClusterID: "hive-oke",
		TrackedChannel: ReleaseChannelStable, AutoUpgrade: autoUpgrade,
	}); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}
	s.registry.Hives = []RegistryEntry{{
		ID: testPinHiveID, GitBranch: "v4", GitHash: "1111111",
		ImageRef:      "ghcr.io/hivecommons/hive:stable",
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
	}}
}

// TestPinDigestSetsDeploymentImage: the pin is applied through the same
// `set image deployment/hive "*=..." -n hive-hosted-<id>` path the switch
// handler uses, and the run-state - who/when/digest/reason - is on the
// persisted record, readable back from disk (the restart-survival property).
func TestPinDigestSetsDeploymentImage(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	stubSpokeImageExists(t)
	model := installDigestPinKubectl(t, "ghcr.io/hivecommons/hive:stable")
	s := newPinHub()
	seedPinHive(t, s, false)

	rec := postPin(t, s, testPinOwner, `{"digest":"`+testPinDigest+`","reason":"rollback: 2222222 broke the proxy"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("pin status = %d body=%s", rec.Code, rec.Body.String())
	}
	wantImage := "ghcr.io/hivecommons/hive@" + testPinDigest
	if got := model.image(t); got != wantImage {
		t.Errorf("deployment image = %q, want %q", got, wantImage)
	}
	var sawSetImage bool
	for _, inv := range model.invocations(t) {
		if strings.Contains(inv, "set image deployment/hive *="+wantImage+" -n hive-hosted-"+testPinHiveID) {
			sawSetImage = true
		}
	}
	if !sawSetImage {
		t.Errorf("expected a set image on deployment/hive in hive-hosted-%s, got %q", testPinHiveID, model.invocations(t))
	}

	// Provenance, read back from disk - not from the in-memory record the
	// handler mutated.
	h := loadSaaSHive(testPinHiveID)
	if h == nil || !h.DigestPinned() {
		t.Fatalf("persisted record carries no pin: %+v", h)
	}
	if h.DigestPin.Digest != testPinDigest {
		t.Errorf("pin digest = %q, want %q", h.DigestPin.Digest, testPinDigest)
	}
	if h.DigestPin.By != testPinOwner {
		t.Errorf("pin by = %q, want %q", h.DigestPin.By, testPinOwner)
	}
	if h.DigestPin.Reason != "rollback: 2222222 broke the proxy" {
		t.Errorf("pin reason = %q", h.DigestPin.Reason)
	}
	if _, err := time.Parse(time.RFC3339, h.DigestPin.At); err != nil {
		t.Errorf("pin at = %q is not RFC3339: %v", h.DigestPin.At, err)
	}
	if h.DigestPin.PreviousImage != "ghcr.io/hivecommons/hive:stable" {
		t.Errorf("previous image = %q, want the reported stable tag", h.DigestPin.PreviousImage)
	}
	if h.TrackedChannel != ReleaseChannelStable {
		t.Errorf("tracked channel = %q, want %q left untouched by the pin", h.TrackedChannel, ReleaseChannelStable)
	}

	// Re-pinning the same digest is a no-op that keeps the original provenance.
	rec = postPin(t, s, testPinOwner, `{"digest":"`+testPinDigest+`","reason":"second attempt"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"changed":false`) {
		t.Errorf("re-pin: status=%d body=%s, want changed:false", rec.Code, rec.Body.String())
	}
	if again := loadSaaSHive(testPinHiveID); again.DigestPin.Reason != "rollback: 2222222 broke the proxy" {
		t.Errorf("re-pin restamped the reason: %q", again.DigestPin.Reason)
	}
}

// TestPinDigestBySHAResolvesDigest: an operator may pin by the short git SHA
// the dashboard shows; the hub resolves it to the manifest digest and records
// both.
func TestPinDigestBySHAResolvesDigest(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	stubSpokeImageExists(t)
	model := installDigestPinKubectl(t, "ghcr.io/hivecommons/hive:stable")
	oldResolve := resolveSpokeDigest
	resolveSpokeDigest = func(tag string, _ *slog.Logger) (string, error) {
		if tag != "3f2a1c9" {
			t.Errorf("resolve called with tag %q", tag)
		}
		return testPinDigest, nil
	}
	t.Cleanup(func() { resolveSpokeDigest = oldResolve })
	s := newPinHub()
	seedPinHive(t, s, false)

	rec := postPin(t, s, testPinOwner, `{"sha":"3f2a1c9"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("pin status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := model.image(t); got != "ghcr.io/hivecommons/hive@"+testPinDigest {
		t.Errorf("deployment image = %q", got)
	}
	h := loadSaaSHive(testPinHiveID)
	if h.DigestPin == nil || h.DigestPin.SourceSHA != "3f2a1c9" || h.DigestPin.Digest != testPinDigest {
		t.Errorf("pin = %+v, want source sha 3f2a1c9 resolved to the digest", h.DigestPin)
	}

	// Both or neither is refused: a request cannot name two targets.
	rec = postPin(t, s, testPinOwner, `{"sha":"3f2a1c9","digest":"`+testPinOtherDigest+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("sha+digest status = %d, want 400", rec.Code)
	}
	rec = postPin(t, s, testPinOwner, `{"digest":"target1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed digest status = %d, want 400", rec.Code)
	}
}

// TestPinDigestNonOwnerRefused: a non-owner gets 403, no kubectl runs, and
// no pin is recorded.
func TestPinDigestNonOwnerRefused(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	stubSpokeImageExists(t)
	model := installDigestPinKubectl(t, "ghcr.io/hivecommons/hive:stable")
	s := newPinHub()
	seedPinHive(t, s, false)

	rec := postPin(t, s, testPinStranger, `{"digest":"`+testPinDigest+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("stranger pin status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if got := model.image(t); got != "ghcr.io/hivecommons/hive:stable" {
		t.Errorf("deployment image changed to %q on a refused pin", got)
	}
	if inv := model.invocations(t); len(inv) != 0 {
		t.Errorf("kubectl ran on a refused pin: %q", inv)
	}
	if h := loadSaaSHive(testPinHiveID); h.DigestPinned() {
		t.Errorf("refused pin was recorded: %+v", h.DigestPin)
	}

	rec = postUnpin(t, s, testPinStranger, `{}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("stranger unpin status = %d, want 403", rec.Code)
	}
}

// TestPinDigestKubectlFailureRecordsNothing: when the patch fails the pin
// must not exist on the hub - a recorded pin the Deployment does not hold is
// the exact lie the feature exists to remove.
func TestPinDigestKubectlFailureRecordsNothing(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	stubSpokeImageExists(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\necho 'connection refused' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := newPinHub()
	seedPinHive(t, s, false)

	rec := postPin(t, s, testPinOwner, `{"digest":"`+testPinDigest+`"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("pin status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if h := loadSaaSHive(testPinHiveID); h.DigestPinned() {
		t.Errorf("pin recorded despite kubectl failure: %+v", h.DigestPin)
	}
}

// TestHeartbeatReArmLeavesPinnedHiveAlone is the #6267 test. A pinned,
// channel-tracking hive beats with the digest image (no tag, so it "disagrees"
// with the channel exactly as the re-arm sees drift). The hub must send no
// switch_to_tag and no upgrade_to, arm nothing in memory, and the
// Deployment image - the fake cluster's state file - must still be the
// digest after the beat AND after an auto-upgrade reconcile tick with a
// newer build available. The unpinned twin at the end is the positive
// control: the same beat on the same hive with the pin lifted DOES re-arm.
func TestHeartbeatReArmLeavesPinnedHiveAlone(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	stubSpokeImageExists(t)
	model := installDigestPinKubectl(t, "ghcr.io/hivecommons/hive:stable")
	s := newPinHub()
	seedPinHive(t, s, true)
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v4"] = branchSHAInfo{SHA: "9999999"}
	latestSHAMu.Unlock()

	// A switch armed BEFORE the pin must not survive it either.
	s.mu.Lock()
	s.heartbeatSwitchTag[testPinHiveID] = ReleaseChannelStable
	s.mu.Unlock()

	if rec := postPin(t, s, testPinOwner, `{"digest":"`+testPinDigest+`","reason":"rollback"}`); rec.Code != http.StatusOK {
		t.Fatalf("pin status = %d body=%s", rec.Code, rec.Body.String())
	}
	pinnedImage := "ghcr.io/hivecommons/hive@" + testPinDigest
	if got := model.image(t); got != pinnedImage {
		t.Fatalf("deployment image after pin = %q, want %q", got, pinnedImage)
	}
	if got := armedSwitchTag(s, testPinHiveID); got != "" {
		t.Fatalf("pin left an armed switch tag %q", got)
	}
	before := len(model.invocations(t))

	// The re-arm tick: the spoke reports the pinned digest, not the channel.
	beat := `{
		"hive_id":"` + testPinHiveID + `","primary_repo":"r",
		"image_ref":"` + pinnedImage + `",
		"git_branch":"v4","git_hash":"1111111","upgrading":false
	}`
	rec := postPinHeartbeat(t, s, beat)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "switch_to_tag") {
		t.Errorf("pinned hive was told to switch: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "upgrade_to") {
		t.Errorf("pinned hive was told to upgrade: %s", rec.Body.String())
	}
	if got := armedSwitchTag(s, testPinHiveID); got != "" {
		t.Errorf("re-arm armed %q on a pinned hive", got)
	}

	// The reconcile tick: auto-upgrade is ON and a newer build exists, which
	// would arm an upgrade on any unpinned hive.
	s.triggerAutoUpgrades()
	s.mu.RLock()
	armedUpgrade := s.heartbeatUpgrade[testPinHiveID]
	s.mu.RUnlock()
	if armedUpgrade != "" {
		t.Errorf("auto-upgrade armed %q on a pinned hive", armedUpgrade)
	}

	// THE assertion: the Deployment still holds the digest, and nothing
	// touched the cluster since the pin.
	if got := model.image(t); got != pinnedImage {
		t.Errorf("deployment image after re-arm tick = %q, want %q (rollback reverted)", got, pinnedImage)
	}
	if after := len(model.invocations(t)); after != before {
		t.Errorf("kubectl ran %d more time(s) after the pin: %q", after-before, model.invocations(t)[before:])
	}

	// Positive control: lift the pin by hand-editing the record (not via
	// unpin, which rewrites the image itself) and replay the identical beat.
	// The channel re-arm now fires, proving the guard above is what held it.
	h := loadSaaSHive(testPinHiveID)
	h.DigestPin = nil
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}
	rec = postPinHeartbeat(t, s, beat)
	if !strings.Contains(rec.Body.String(), `"switch_to_tag":"stable"`) {
		t.Errorf("positive control: unpinned hive was NOT re-armed, got %s", rec.Body.String())
	}
}

// TestUnpinResumesTracking: unpin clears the run-state, writes the channel
// tag back onto the Deployment through the same path, and the next beat still
// reporting the old digest is re-armed onto the channel - tracking resumed.
func TestUnpinResumesTracking(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	stubSpokeImageExists(t)
	model := installDigestPinKubectl(t, "ghcr.io/hivecommons/hive:stable")
	s := newPinHub()
	seedPinHive(t, s, false)

	if rec := postPin(t, s, testPinOwner, `{"digest":"`+testPinDigest+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("pin status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec := postUnpin(t, s, testPinOwner, `{"reason":"fix shipped"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unpin status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"restore_tag":"stable"`) || !strings.Contains(rec.Body.String(), `"via":"kubectl"`) {
		t.Errorf("unpin body = %s, want restore_tag stable via kubectl", rec.Body.String())
	}
	if got := model.image(t); got != "ghcr.io/hivecommons/hive:stable" {
		t.Errorf("deployment image after unpin = %q, want the channel tag", got)
	}
	if h := loadSaaSHive(testPinHiveID); h.DigestPinned() {
		t.Errorf("unpin left the pin in place: %+v", h.DigestPin)
	}

	// The spoke has not rolled yet and still reports the digest: with the pin
	// gone, the durable re-arm heals it onto the channel.
	rec = postPinHeartbeat(t, s, `{
		"hive_id":"`+testPinHiveID+`","primary_repo":"r",
		"image_ref":"ghcr.io/hivecommons/hive@`+testPinDigest+`",
		"git_branch":"v4","git_hash":"1111111","upgrading":false
	}`)
	if !strings.Contains(rec.Body.String(), `"switch_to_tag":"stable"`) {
		t.Errorf("tracking did not resume after unpin: %s", rec.Body.String())
	}

	// Unpinning an unpinned hive is a no-op, not an error.
	rec = postUnpin(t, s, testPinOwner, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"changed":false`) {
		t.Errorf("second unpin: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestUnpinRestoresPlainBranchTag: a hive on a plain branch (no channel)
// returns to the mutable tag it was running when pinned.
func TestUnpinRestoresPlainBranchTag(t *testing.T) {
	h := &SaaSHive{DigestPin: &DigestPin{Digest: testPinDigest, PreviousImage: "ghcr.io/hivecommons/hive:v4-latest"}}
	if got := unpinRestoreTag(h, "v4"); got != "v4-latest" {
		t.Errorf("plain-branch restore = %q, want v4-latest", got)
	}
	h.TrackedChannel = ReleaseChannelCandidate
	if got := unpinRestoreTag(h, "v4"); got != ReleaseChannelCandidate {
		t.Errorf("channel restore = %q, want the channel", got)
	}
	// A previous image that was itself immutable (a SHA tag) is not a
	// moving tag to return to; fall back to the branch's -latest.
	h = &SaaSHive{DigestPin: &DigestPin{Digest: testPinDigest, PreviousImage: "ghcr.io/hivecommons/hive:abc1234"}}
	if got := unpinRestoreTag(h, "feat/x"); got != "feat-x-latest" {
		t.Errorf("sha-previous restore = %q, want feat-x-latest", got)
	}
	if got := unpinRestoreTag(&SaaSHive{}, ""); got != "" {
		t.Errorf("nothing known restore = %q, want empty", got)
	}
}

// TestImageChangesRefusedWhilePinned: upgrade, switch-branch and the bulk
// equivalents refuse a pinned hive with 409 naming the pin, and the
// Deployment is untouched.
func TestImageChangesRefusedWhilePinned(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	stubSpokeImageExists(t)
	model := installDigestPinKubectl(t, "ghcr.io/hivecommons/hive:stable")
	s := newPinHub()
	seedPinHive(t, s, false)
	if rec := postPin(t, s, testPinOwner, `{"digest":"`+testPinDigest+`","reason":"rollback"}`); rec.Code != http.StatusOK {
		t.Fatalf("pin status = %d body=%s", rec.Code, rec.Body.String())
	}
	before := len(model.invocations(t))

	rec := httptest.NewRecorder()
	s.handleSwitchBranch(rec, setPathValue(reqWithUser(http.MethodPost, "/sw", `{"branch":"v4"}`, testPinOwner), "id", testPinHiveID))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), testPinDigest) || !strings.Contains(rec.Body.String(), testPinOwner) {
		t.Errorf("switch-branch on pinned hive: status=%d body=%s, want 409 naming the pin", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleUpgradeHive(rec, setPathValue(reqWithUser(http.MethodPost, "/up", "", testPinOwner), "id", testPinHiveID))
	if rec.Code != http.StatusConflict {
		t.Errorf("upgrade on pinned hive: status=%d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if res := s.applyBulkAction(bulkActionUpgrade, "", testPinHiveID, testPinOwner); res.Ok || !strings.Contains(res.Error, testPinDigest) {
		t.Errorf("bulk upgrade on pinned hive = %+v, want refusal naming the pin", res)
	}
	if res := s.applyBulkAction(bulkActionSwitchBranch, "v4", testPinHiveID, testPinOwner); res.Ok {
		t.Errorf("bulk switch-branch on pinned hive = %+v, want refusal", res)
	}

	if got := model.image(t); got != "ghcr.io/hivecommons/hive@"+testPinDigest {
		t.Errorf("deployment image = %q after refused changes, want the digest", got)
	}
	if after := len(model.invocations(t)); after != before {
		t.Errorf("kubectl ran on refused changes: %q", model.invocations(t)[before:])
	}
}

// TestUnpinRefusedWhileSpokeUpgradesPaused: lifting a pin returns the hive to
// the upgrade train, which the admin pause holds still - same 409 as a switch.
// Pinning itself is NOT gated on the pause: a rollback during an incident is
// the operator steering by hand while the train is stopped.
func TestUnpinRefusedWhileSpokeUpgradesPaused(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	stubSpokeImageExists(t)
	model := installDigestPinKubectl(t, "ghcr.io/hivecommons/hive:stable")
	s := newPinHub()
	seedPinHive(t, s, false)
	if _, err := s.setUpgradePause(upgradePauseTargetSpokes, true, "admin"); err != nil {
		t.Fatalf("setUpgradePause: %v", err)
	}

	if rec := postPin(t, s, testPinOwner, `{"digest":"`+testPinDigest+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("pin under pause status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if got := model.image(t); got != "ghcr.io/hivecommons/hive@"+testPinDigest {
		t.Errorf("deployment image = %q, pin under pause must still land", got)
	}
	rec := postUnpin(t, s, testPinOwner, `{}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("unpin under pause status = %d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if h := loadSaaSHive(testPinHiveID); !h.DigestPinned() {
		t.Errorf("refused unpin cleared the pin")
	}
}

// TestMyHivesCarriesDigestPin: the dashboard renders the PINNED pill from the
// My Hives payload, overlaid from the hub-owned record like trackedChannel.
func TestMyHivesCarriesDigestPin(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newPinHub()
	seedPinHive(t, s, false)
	h := loadSaaSHive(testPinHiveID)
	h.DigestPin = &DigestPin{Digest: testPinDigest, By: testPinOwner, At: "2026-09-08T00:00:00Z", Reason: "rollback"}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleMyHives(rec, reqWithUser(http.MethodGet, "/api/saas/my-hives", "", testPinOwner))
	if rec.Code != http.StatusOK {
		t.Fatalf("my-hives status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"digestPin":{`) || !strings.Contains(body, `"digest":"`+testPinDigest+`"`) || !strings.Contains(body, `"reason":"rollback"`) {
		t.Errorf("my-hives payload lacks the pin with provenance: %s", body)
	}
}

func TestNormalizeImageDigest(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{testPinDigest, testPinDigest, true},
		{testPinDigestHex, testPinDigest, true},
		{"SHA256:" + strings.ToUpper(testPinDigestHex), testPinDigest, true},
		{"  " + testPinDigest + " ", testPinDigest, true},
		{"", "", false},
		{"sha256:abc", "", false},
		{"abc1234", "", false},
		{"md5:" + testPinDigestHex, "", false},
	}
	for _, tc := range cases {
		got, err := normalizeImageDigest(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("normalizeImageDigest(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
	if got := imageDigestOf("ghcr.io/hivecommons/hive@" + testPinDigest); got != testPinDigest {
		t.Errorf("imageDigestOf = %q", got)
	}
	if got := imageDigestOf("ghcr.io/hivecommons/hive:stable"); got != "" {
		t.Errorf("imageDigestOf(tag) = %q, want empty", got)
	}
}

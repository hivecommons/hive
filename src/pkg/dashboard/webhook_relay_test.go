package dashboard

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// signWebhookRelayForTest is the hub's relay signature (pkg/hub
// webhookRelaySignature), computed independently here.
func signWebhookRelayForTest(key, ts, delivery string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(ts + "." + delivery + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// withWebhookRelayKey gives this spoke a hub relay key (as a heartbeat
// delivery would) in a temp store, and no GITHUB_WEBHOOK_SECRET.
func withWebhookRelayKey(t *testing.T, key string) {
	t.Helper()
	t.Setenv(githubWebhookSecretEnvVar, "")
	t.Cleanup(github.SetWebhookRelayKeyPathForTest(filepath.Join(t.TempDir(), "webhook-relay.key")))
	if key == "" {
		return
	}
	if changed, err := github.AdoptWebhookRelayKey(key); err != nil || !changed {
		t.Fatalf("adopt relay key: changed=%v err=%v", changed, err)
	}
}

func postRelayedWebhook(s *Server, event, delivery, ts, relaySig string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	// GitHub's own signature rides along unchanged; this spoke cannot verify it.
	req.Header.Set("X-Hub-Signature-256", signGitHubWebhookForTest("app-secret-the-spoke-never-has", body))
	if ts != "" {
		req.Header.Set(webhookRelayTimestampHeader, ts)
	}
	if relaySig != "" {
		req.Header.Set(webhookRelaySignatureHeader, relaySig)
	}
	rec := httptest.NewRecorder()
	s.handleGitHubWebhook(rec, req)
	return rec
}

func relayErr(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Error
}

// TestHubRelayedWebhookAcceptedWithoutSecret: a hosted spoke with no
// GITHUB_WEBHOOK_SECRET applies a delivery its hub relayed and signed with
// this hive's relay key (#17911).
func TestHubRelayedWebhookAcceptedWithoutSecret(t *testing.T) {
	const key = "relay-key-h1"
	withWebhookRelayKey(t, key)
	s := newGitHubWebhookTestServer(t)

	body := []byte(`{"number":5,"pull_request":{"number":5},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	rec := postRelayedWebhook(s, "pull_request", "relay-accept-1", ts, signWebhookRelayForTest(key, ts, "relay-accept-1", body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("relay-signed delivery: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Status      string `json:"status"`
		Invalidated []int  `json:"invalidated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || !reflect.DeepEqual(got.Invalidated, []int{5}) {
		t.Fatalf("relay-signed delivery = %+v, want ok/[5]", got)
	}
}

// TestHubRelayedWebhookRejections: tampered bodies, wrong keys, stale
// timestamps, missing or replayed delivery IDs are each rejected with their
// own reason.
func TestHubRelayedWebhookRejections(t *testing.T) {
	const key = "relay-key-h1"
	withWebhookRelayKey(t, key)
	s := newGitHubWebhookTestServer(t)

	body := []byte(`{"number":5,"pull_request":{"number":5},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	t.Run("tampered body", func(t *testing.T) {
		tampered := []byte(strings.Replace(string(body), `"number":5`, `"number":6`, 1))
		rec := postRelayedWebhook(s, "pull_request", "tamper-1", now, signWebhookRelayForTest(key, now, "tamper-1", body), tampered)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(relayErr(t, rec), github.WebhookReasonRelaySignatureFailed) {
			t.Fatalf("tampered body: %d %s, want 401 %s", rec.Code, rec.Body.String(), github.WebhookReasonRelaySignatureFailed)
		}
	})
	t.Run("another hive's key", func(t *testing.T) {
		rec := postRelayedWebhook(s, "pull_request", "other-1", now, signWebhookRelayForTest("relay-key-h2", now, "other-1", body), body)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(relayErr(t, rec), github.WebhookReasonRelaySignatureFailed) {
			t.Fatalf("other hive's key: %d %s, want 401", rec.Code, rec.Body.String())
		}
	})
	t.Run("delivery ID swapped after signing", func(t *testing.T) {
		rec := postRelayedWebhook(s, "pull_request", "swapped-2", now, signWebhookRelayForTest(key, now, "swapped-1", body), body)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(relayErr(t, rec), github.WebhookReasonRelaySignatureFailed) {
			t.Fatalf("swapped delivery: %d %s, want 401", rec.Code, rec.Body.String())
		}
	})
	t.Run("missing relay signature", func(t *testing.T) {
		rec := postRelayedWebhook(s, "pull_request", "unsigned-1", now, "", body)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unsigned relay: status = %d, want 401", rec.Code)
		}
	})
	t.Run("stale timestamp", func(t *testing.T) {
		old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
		rec := postRelayedWebhook(s, "pull_request", "stale-1", old, signWebhookRelayForTest(key, old, "stale-1", body), body)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(relayErr(t, rec), github.WebhookReasonRelayStale) {
			t.Fatalf("stale timestamp: %d %s, want 401 %s", rec.Code, rec.Body.String(), github.WebhookReasonRelayStale)
		}
		future := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
		rec = postRelayedWebhook(s, "pull_request", "stale-2", future, signWebhookRelayForTest(key, future, "stale-2", body), body)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(relayErr(t, rec), github.WebhookReasonRelayStale) {
			t.Fatalf("future timestamp: %d %s, want 401 %s", rec.Code, rec.Body.String(), github.WebhookReasonRelayStale)
		}
	})
	t.Run("missing delivery ID", func(t *testing.T) {
		rec := postRelayedWebhook(s, "pull_request", "", now, signWebhookRelayForTest(key, now, "", body), body)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(relayErr(t, rec), github.WebhookReasonRelayDeliveryMissing) {
			t.Fatalf("missing delivery: %d %s, want 401 %s", rec.Code, rec.Body.String(), github.WebhookReasonRelayDeliveryMissing)
		}
	})
	t.Run("replayed delivery ID", func(t *testing.T) {
		sig := signWebhookRelayForTest(key, now, "replay-1", body)
		if rec := postRelayedWebhook(s, "pull_request", "replay-1", now, sig, body); rec.Code != http.StatusOK {
			t.Fatalf("first delivery: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		rec := postRelayedWebhook(s, "pull_request", "replay-1", now, sig, body)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(relayErr(t, rec), github.WebhookReasonRelayReplayed) {
			t.Fatalf("replay: %d %s, want 401 %s", rec.Code, rec.Body.String(), github.WebhookReasonRelayReplayed)
		}
	})
}

// TestHubRelayedWebhookFailuresAreCounted: a rejected relay counts as a
// failed delivery, the same signal a bad GitHub signature feeds the cadence
// mode, so a burst of forged relays degrades to polling and is visible.
func TestHubRelayedWebhookFailuresAreCounted(t *testing.T) {
	const key = "relay-key-h1"
	withWebhookRelayKey(t, key)
	t.Cleanup(github.ResetWebhookTrackerForTest())
	s := newGitHubWebhookTestServer(t)
	body := []byte(`{"number":5,"pull_request":{"number":5},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	if rec := postRelayedWebhook(s, "pull_request", "count-1", now, signWebhookRelayForTest(key, now, "count-1", body), body); rec.Code != http.StatusOK {
		t.Fatalf("good relay: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, _, recovered := github.ObserveWebhookMode(); !recovered {
		t.Fatal("a verified relay should engage webhook mode")
	}
	if rec := postRelayedWebhook(s, "pull_request", "count-2", now, signWebhookRelayForTest("wrong", now, "count-2", body), body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged relay: status = %d, want 401", rec.Code)
	}
	if _, degraded, _ := github.ObserveWebhookMode(); !degraded {
		t.Fatal("a rejected relay after the last good one must be counted as a failed delivery")
	}
}

// TestGitHubSignedWebhookUnchangedWithRelayKey: a spoke with
// GITHUB_WEBHOOK_SECRET keeps accepting GitHub-signed deliveries exactly as
// before, relay key or not — no relay headers needed and no replay check.
func TestGitHubSignedWebhookUnchangedWithRelayKey(t *testing.T) {
	withWebhookRelayKey(t, "relay-key-h1")
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	s := newGitHubWebhookTestServer(t)

	body := []byte(`{"number":5,"pull_request":{"number":5},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", "gh-direct-1")
		req.Header.Set("X-Hub-Signature-256", signGitHubWebhookForTest(secret, body))
		rec := httptest.NewRecorder()
		s.handleGitHubWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GitHub-signed delivery %d: status = %d, want 200 (%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest("wrong", body), body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad GitHub signature: status = %d, want 401", rec.Code)
	}
}

// TestWebhooksConfiguredByRelayKey: the relay key alone turns webhooks on
// for event-driven review dispatch and /api/status.webhooks.verification.
func TestWebhooksConfiguredByRelayKey(t *testing.T) {
	withWebhookRelayKey(t, "")
	if githubWebhooksConfigured() {
		t.Fatal("no secret and no relay key must leave webhooks off")
	}
	s := newGitHubWebhookTestServer(t)
	body := []byte(`{}`)
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest("x", body), body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret, no relay key: status = %d, want 503", rec.Code)
	}

	if _, err := github.AdoptWebhookRelayKey("relay-key-h1"); err != nil {
		t.Fatal(err)
	}
	if !githubWebhooksConfigured() {
		t.Fatal("a hub relay key must turn webhooks on")
	}

	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "relay-test"}
	s.statusMu.Unlock()
	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status?fields=webhooks", nil))
	var status struct {
		Webhooks struct {
			Verification string `json:"verification"`
		} `json:"webhooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Webhooks.Verification != github.WebhookVerificationRelay {
		t.Fatalf("webhooks.verification = %q, want %q", status.Webhooks.Verification, github.WebhookVerificationRelay)
	}
}

func TestWebhookRelayReplayCacheIsBounded(t *testing.T) {
	var c webhookRelayReplayCache
	now := time.Now()
	for i := 0; i < webhookRelayReplayMax+10; i++ {
		if !c.firstSeen(strconv.Itoa(i), now.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("id %d reported as seen", i)
		}
	}
	if len(c.seen) > webhookRelayReplayMax {
		t.Fatalf("cache holds %d ids, want at most %d", len(c.seen), webhookRelayReplayMax)
	}
	later := now.Add(webhookRelayReplayTTL + time.Hour)
	if !c.firstSeen(strconv.Itoa(webhookRelayReplayMax+5), later) {
		t.Fatal("an id older than the replay TTL must be accepted again")
	}
}

// TestHealthDeepRelayKeyClearsNoSecret: a hosted spoke holding a hub relay
// key is not "no_secret" — deep health (and so health_deep_read) reports the
// relay mode, and a forged relay surfaces as its own relay reason (#17911).
func TestHealthDeepRelayKeyClearsNoSecret(t *testing.T) {
	t.Cleanup(github.ResetWebhookTrackerForTest())
	withWebhookRelayKey(t, "relay-key-h1")
	s := newGitHubWebhookTestServer(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "webhook-relay-health-test"}
	s.ready = true
	s.statusMu.Unlock()

	_, check := readDeepWebhookCheck(t, s)
	if check["mode_reason_code"] == github.WebhookReasonNoSecret || check["status"] != "pass" ||
		check["verification"] != github.WebhookVerificationRelay {
		t.Fatalf("webhooks check with a relay key = %v, want pass, verification %q and no no_secret", check, github.WebhookVerificationRelay)
	}

	body := []byte(`{"number":5,"pull_request":{"number":5},"repository":{"full_name":"webhookorg/webhookrepo"}}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)
	if rec := postRelayedWebhook(s, "pull_request", "forged-1", now, signWebhookRelayForTest("wrong", now, "forged-1", body), body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged relay: status = %d, want 401", rec.Code)
	}
	got := readStatusWebhooks(t, s)
	if got["mode_reason_code"] != github.WebhookReasonRelaySignatureFailed || got["rejected_1h"] != float64(1) {
		t.Fatalf("webhooks after a forged relay = %v, want mode_reason_code %q and rejected_1h 1", got, github.WebhookReasonRelaySignatureFailed)
	}
	if _, check := readDeepWebhookCheck(t, s); check["status"] != "warn" || check["mode_reason_code"] != github.WebhookReasonRelaySignatureFailed {
		t.Fatalf("webhooks check after a forged relay = %v, want a relay signature warning", check)
	}
}

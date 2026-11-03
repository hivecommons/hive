package spoke

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The 2026-10-26 vllm-d incident: a Kyverno validating webhook (failurePolicy
// Fail, controller restart-looping) timed out every Deployment PATCH from
// inside the pod. The spoke reported the upgrade FAILED, the hub showed
// "failed 3 attempts" with a floating-tag hint, and the upgrade only landed
// because the process exit made the kubelet re-pull the :edge tag. These
// tests pin the classification, the restart-fallback decision and the retry.

func TestClassifyPatchFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want PatchFailureKind
	}{
		{"nil", nil, PatchFailureNone},
		{
			"client-side deadline on the PATCH (stuck webhook, observed live)",
			errors.New(`k8s API PATCH /apis/apps/v1/namespaces/x/deployments/hive: Patch "https://kubernetes.default.svc/apis/apps/v1/namespaces/x/deployments/hive": context deadline exceeded`),
			PatchFailureTimeout,
		},
		{
			"apiserver names the webhook (kubectl from outside)",
			errors.New(`k8s API PATCH /x: HTTP 500: Internal error occurred: failed calling webhook "validate.kyverno.svc-fail": failed to call webhook: Post "https://kyverno-svc.kyverno.svc:443/validate/fail?timeout=10s": context deadline exceeded`),
			PatchFailureAdmission,
		},
		{
			"admission webhook denies",
			errors.New(`k8s API PATCH /x: HTTP 400: admission webhook "validate.kyverno.svc-fail" denied the request: policy require-labels fail`),
			PatchFailureAdmission,
		},
		{"forbidden", errors.New(`k8s API PATCH /x: HTTP 403: deployments.apps "hive" is forbidden`), PatchFailureForbidden},
		{"unauthorized", errors.New(`k8s API PATCH /x: HTTP 401: Unauthorized`), PatchFailureForbidden},
		{"plain 5xx", errors.New(`k8s API PATCH /x: HTTP 503: etcdserver: leader changed`), PatchFailureServer},
		{"client timeout wording", errors.New(`Patch "https://…": net/http: request canceled (Client.Timeout exceeded while awaiting headers)`), PatchFailureTimeout},
		{"missing token", errors.New("reading service account token: open /var/run/…/token: no such file"), PatchFailureOther},
		{"conflict", errors.New(`k8s API PATCH /x: HTTP 409: the object has been modified`), PatchFailureOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPatchFailure(tc.err); got != tc.want {
				t.Fatalf("ClassifyPatchFailure(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestPatchFailureHintNamesTheWebhookNotTheTag(t *testing.T) {
	for _, kind := range []PatchFailureKind{PatchFailureAdmission, PatchFailureTimeout} {
		hint := PatchFailureHint(kind)
		if !strings.Contains(hint, "admission webhook") || !strings.Contains(hint, "Kyverno") {
			t.Errorf("hint for %q = %q, want it to name the admission webhook", kind, hint)
		}
		if strings.Contains(strings.ToLower(hint), "floating tag") {
			t.Errorf("hint for %q = %q must not blame the image tag", kind, hint)
		}
		if !strings.Contains(hint, "falling back to a process restart") {
			t.Errorf("hint for %q = %q must say the spoke is falling back to a restart", kind, hint)
		}
	}
	if got := PatchFailureHint(PatchFailureForbidden); !strings.Contains(got, "hive-self-upgrade") {
		t.Errorf("forbidden hint = %q, want the RBAC Role named", got)
	}
	if got := PatchFailureHint(PatchFailureNone); got != "" {
		t.Errorf("hint for no error = %q, want empty", got)
	}
}

func TestRestartFallbackFor(t *testing.T) {
	webhookErr := errors.New(`k8s API PATCH /x: Patch "https://…": context deadline exceeded`)
	rbacErr := errors.New(`k8s API PATCH /x: HTTP 403: forbidden`)
	cases := []struct {
		name       string
		patchErr   error
		image      string
		pullPolicy string
		imageErr   error
		wantLands  bool
		wantKind   PatchFailureKind
	}{
		{"floating tag, pullPolicy Always (the observed hosted spoke)", webhookErr, "ghcr.io/hivecommons/hive:edge", "Always", nil, true, PatchFailureTimeout},
		{"floating -latest tag, pullPolicy unset", webhookErr, "ghcr.io/hivecommons/hive:v5-latest", "", nil, true, PatchFailureTimeout},
		{"floating tag but IfNotPresent: a restart reuses the cached image", webhookErr, "ghcr.io/hivecommons/hive:edge", "IfNotPresent", nil, false, PatchFailureTimeout},
		{"floating tag but Never", webhookErr, "ghcr.io/hivecommons/hive:edge", "Never", nil, false, PatchFailureTimeout},
		{"SHA-pinned tag: restart lands the same build", webhookErr, "ghcr.io/hivecommons/hive:fcf0e78", "Always", nil, false, PatchFailureTimeout},
		{"digest pin", webhookErr, "ghcr.io/hivecommons/hive@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "Always", nil, false, PatchFailureTimeout},
		{"cannot read the Deployment at all: restart is the only lever", webhookErr, "", "", errors.New("GET: context deadline exceeded"), true, PatchFailureTimeout},
		{"RBAC denial on a floating tag still restarts", rbacErr, "ghcr.io/hivecommons/hive:edge", "Always", nil, true, PatchFailureForbidden},
		{"untagged ref (implicit :latest) cannot be proven pinned: restart", webhookErr, "ghcr.io/hivecommons/hive", "", nil, true, PatchFailureTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := restartFallbackFor(tc.patchErr, tc.image, tc.pullPolicy, tc.imageErr)
			if fb.RestartLands != tc.wantLands {
				t.Fatalf("RestartLands = %v, want %v (%+v)", fb.RestartLands, tc.wantLands, fb)
			}
			if fb.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", fb.Kind, tc.wantKind)
			}
			if fb.Hint == "" {
				t.Fatal("Hint must never be empty for a failed patch")
			}
			if !tc.wantLands && !strings.Contains(fb.Hint, "a restart cannot land the target") {
				t.Errorf("Hint = %q, want it to say a restart cannot land the target", fb.Hint)
			}
			if tc.wantLands && strings.Contains(fb.Hint, "a restart cannot land") {
				t.Errorf("Hint = %q must not claim a restart cannot land when it can", fb.Hint)
			}
		})
	}
}

// TestPatchWithRetry drives the retry loop with an injected sleep so no test
// ever waits on the real backoff.
func TestPatchWithRetry(t *testing.T) {
	webhookErr := errors.New(`k8s API PATCH /x: HTTP 500: failed calling webhook "validate.kyverno.svc-fail": context deadline exceeded`)
	rbacErr := errors.New(`k8s API PATCH /x: HTTP 403: forbidden`)
	cases := []struct {
		name       string
		results    []error
		wantCalls  int
		wantSleeps int
		wantErr    error
	}{
		{"first try succeeds", []error{nil}, 1, 0, nil},
		{"transient webhook failure then success", []error{webhookErr, nil}, 2, 1, nil},
		{"webhook failure twice gives up after the single retry", []error{webhookErr, webhookErr}, 2, 1, webhookErr},
		{"RBAC denial is not retried", []error{rbacErr, nil}, 1, 0, rbacErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, sleeps := 0, 0
			var slept time.Duration
			err := patchWithRetry(selfUpgradePatchAttempts, selfUpgradePatchRetryBackoff,
				func(d time.Duration) { sleeps++; slept += d },
				func() error {
					if calls >= len(tc.results) {
						t.Fatalf("patch called %d times, only %d results scripted", calls+1, len(tc.results))
					}
					err := tc.results[calls]
					calls++
					return err
				})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Errorf("patch calls = %d, want %d", calls, tc.wantCalls)
			}
			if sleeps != tc.wantSleeps {
				t.Errorf("sleeps = %d, want %d", sleeps, tc.wantSleeps)
			}
			if sleeps > 0 && slept != selfUpgradePatchRetryBackoff*time.Duration(sleeps) {
				t.Errorf("slept %v, want %v per retry", slept, selfUpgradePatchRetryBackoff)
			}
		})
	}
}

func TestSelfUpgradePatchTimeoutOutlastsAdmissionWebhook(t *testing.T) {
	// Admission webhooks default to a 10s timeout and the apiserver does not
	// return the webhook error until after it; a client deadline at or below
	// that hides the real cause behind "context deadline exceeded".
	const webhookDefaultTimeout = 10 * time.Second
	if selfUpgradePatchTimeout <= 2*webhookDefaultTimeout {
		t.Fatalf("selfUpgradePatchTimeout = %v, want comfortably above the %v webhook default", selfUpgradePatchTimeout, webhookDefaultTimeout)
	}
	if selfUpgradePatchAttempts != 2 {
		t.Fatalf("selfUpgradePatchAttempts = %d, want exactly one retry", selfUpgradePatchAttempts)
	}
}

// TestK8sAPIPatchRetriesOnceOnWebhookFailure exercises the real k8sAPIPatch
// against a fake apiserver that fails the first PATCH the way a stuck Kyverno
// webhook does, with the retry sleep stubbed out.
func TestK8sAPIPatchRetriesOnceOnWebhookFailure(t *testing.T) {
	oldSleep := selfUpgradePatchSleep
	var slept atomic.Int64
	selfUpgradePatchSleep = func(d time.Duration) { slept.Add(int64(d)) }
	t.Cleanup(func() { selfUpgradePatchSleep = oldSleep })

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"Internal error occurred: failed calling webhook \"validate.kyverno.svc-fail\": context deadline exceeded"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	if err := k8sAPIPatch("/apis/apps/v1/namespaces/x/deployments/hive", []byte(`{}`)); err != nil {
		t.Fatalf("k8sAPIPatch after one webhook failure: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("PATCH calls = %d, want 2 (one retry)", got)
	}
	if got := time.Duration(slept.Load()); got != selfUpgradePatchRetryBackoff {
		t.Fatalf("slept %v between tries, want %v", got, selfUpgradePatchRetryBackoff)
	}
}

func TestK8sAPIPatchDoesNotRetryForbidden(t *testing.T) {
	oldSleep := selfUpgradePatchSleep
	selfUpgradePatchSleep = func(time.Duration) { t.Fatal("a 403 must not be retried") }
	t.Cleanup(func() { selfUpgradePatchSleep = oldSleep })

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	err := k8sAPIPatch("/x", []byte(`{}`))
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if got := ClassifyPatchFailure(err); got != PatchFailureForbidden {
		t.Fatalf("classified %q, want forbidden: %v", got, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("PATCH calls = %d, want 1", got)
	}
}

func TestSelfDeploymentImageInfoReadsPullPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive","image":"ghcr.io/hivecommons/hive:edge","imagePullPolicy":"Always"}]}}}}`))
	}))
	defer srv.Close()
	withFakeK8sAPI(t, srv)

	image, policy, err := selfDeploymentImageInfo()
	if err != nil {
		t.Fatal(err)
	}
	if image != "ghcr.io/hivecommons/hive:edge" || policy != "Always" {
		t.Fatalf("got image=%q policy=%q", image, policy)
	}
	fb := restartFallbackFor(errors.New("context deadline exceeded"), image, policy, nil)
	if !fb.RestartLands {
		t.Fatalf("a floating tag pulled Always must fall through to restart: %+v", fb)
	}
}

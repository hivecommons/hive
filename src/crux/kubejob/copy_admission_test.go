package kubejob

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCopyTransportRefusedWhenTheAdmissionPolicyIsNotEnforced (#19971): copy
// mode runs only when the dry-run probe Secret is denied by the copy admission
// policy. An admitted probe (no policy, binding or namespace label) or one
// refused by anything else fails the kick closed before any object exists.
func TestCopyTransportRefusedWhenTheAdmissionPolicyIsNotEnforced(t *testing.T) {
	tests := []struct {
		name    string
		tweak   func(*fakeAPI)
		wantErr string
	}{
		{"probe admitted", func(a *fakeAPI) { a.probeAdmitted = true }, "was admitted"},
		{"probe forbidden by RBAC", func(a *fakeAPI) {
			a.probeStatus, a.probeMessage = http.StatusForbidden, `secrets is forbidden: User "system:serviceaccount:hive:hive" cannot create resource "secrets"`
		}, "could not confirm"},
		{"probe denied by another policy", func(a *fakeAPI) {
			a.probeStatus, a.probeMessage = http.StatusUnprocessableEntity, `secrets "hive-copy-admission-probe" is forbidden: ValidatingAdmissionPolicy 'some-other-policy' with binding 'x' denied request: no`
		}, "could not confirm"},
		{"api server error", func(a *fakeAPI) {
			a.probeStatus, a.probeMessage = http.StatusInternalServerError, "etcdserver: request timed out"
		}, "could not confirm"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.selfPod = labelledSelfPod
			tc.tweak(api)
			srv := httptest.NewServer(api.handler(t))
			defer srv.Close()
			l := newLauncher(srv, copyOptions())
			_, err := l.Run(context.Background(), specFor(t.TempDir()))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), CopyAdmissionPolicyName) {
				t.Fatalf("Run err = %v, want a refusal naming %s and %q", err, CopyAdmissionPolicyName, tc.wantErr)
			}
			if api.probes != 1 {
				t.Errorf("probes = %d, want 1", api.probes)
			}
			if api.created != nil || api.secret != nil || api.policy != nil {
				t.Errorf("objects were created before the admission check passed: job=%v secret=%v policy=%v", api.created != nil, api.secret != nil, api.policy != nil)
			}
			// A failed check is not remembered: the next kick probes again,
			// so installing the policy needs no restart.
			_, _ = l.Run(context.Background(), specFor(t.TempDir()))
			if api.probes != 2 {
				t.Errorf("probes after a second kick = %d, want 2 (a refusal is not cached)", api.probes)
			}
		})
	}
}

// TestCopyAdmissionProbeIsADryRunThePolicyMustDeny: the probe is a dry run
// (nothing is persisted) of a Secret outside hive-kick-*, without hive's label
// or an owner, so the policy's first rule denies it; a pass is cached.
func TestCopyAdmissionProbeIsADryRunThePolicyMustDeny(t *testing.T) {
	api := newFakeAPI()
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, copyOptions())
	if err := l.VerifyCopyAdmission(context.Background()); err != nil {
		t.Fatalf("VerifyCopyAdmission with the policy denying the probe: %v", err)
	}
	if api.probes != 1 {
		t.Fatalf("probes = %d, want 1", api.probes)
	}
	if strings.HasPrefix(copyAdmissionProbeName, "hive-kick-") {
		t.Errorf("probe name %q must fall outside hive-kick-*", copyAdmissionProbeName)
	}
	for _, bad := range []string{managedByLabel, "ownerReferences", "hive-kick-"} {
		if strings.Contains(api.probeBody, bad) {
			t.Errorf("probe body %s carries %q, which the policy would admit", api.probeBody, bad)
		}
	}
	if api.secret != nil {
		t.Errorf("the probe was recorded as a real Secret create: %v", api.secret)
	}
	if err := l.VerifyCopyAdmission(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.probes != 1 {
		t.Errorf("probes after a cached pass = %d, want 1", api.probes)
	}
}

func TestDeniedByCopyAdmission(t *testing.T) {
	cases := map[string]bool{
		`secrets "hive-copy-admission-probe" is forbidden: ValidatingAdmissionPolicy 'hive-sandbox-copy-objects' with binding 'hive-sandbox-copy-objects' denied request: hive's service account may only touch objects named hive-kick-*`: true,
		`secrets is forbidden: User "system:serviceaccount:hive:hive" cannot create resource "secrets"`:                                                                                                                                    false,
		`ValidatingAdmissionPolicy 'hive-sandbox-copy-objects-old' with binding 'b' denied request: x`:                                                                                                                                     false,
		`ValidatingAdmissionPolicy 'hive-sandbox-copy-objects' with binding 'b' failed: expression error`:                                                                                                                                  false,
	}
	for msg, want := range cases {
		if got := deniedByCopyAdmission(msg); got != want {
			t.Errorf("deniedByCopyAdmission(%q) = %v, want %v", msg, got, want)
		}
	}
}

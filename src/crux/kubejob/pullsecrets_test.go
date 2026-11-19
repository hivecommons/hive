package kubejob

import (
	"strings"
	"testing"
)

const saWithSecrets = `{"imagePullSecrets":[{"name":"reg-a"},{"name":"ghcr-pull"}]}`

func TestPullSecretsUnionDedupedAndOrdered(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = vanillaSelfPod // ghcr-pull
	api.serviceAccounts = map[string]string{"job-sa": saWithSecrets}
	pod := runRelayJob(t, api, Options{ServiceAccount: "job-sa", ImagePullSecrets: []string{"reg-b", "reg-a"}})
	want := `[{"name":"ghcr-pull"},{"name":"reg-a"},{"name":"reg-b"}]`
	if got := asJSON(pod["imagePullSecrets"]); got != want {
		t.Errorf("imagePullSecrets = %s, want %s", got, want)
	}
}

func TestPullSecretsConfiguredOnlyWithoutHivePodSecrets(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = openshiftSelfPod // no pull secrets
	pod := runRelayJob(t, api, Options{ImagePullSecrets: []string{"reg-b"}})
	if got := asJSON(pod["imagePullSecrets"]); got != `[{"name":"reg-b"}]` {
		t.Errorf("imagePullSecrets = %s, want the configured name", got)
	}
}

func TestPullSecretsServiceAccountReadDeniedFallsBack(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = vanillaSelfPod
	api.saForbidden = true
	pod := runRelayJob(t, api, Options{ServiceAccount: "job-sa", ImagePullSecrets: []string{"reg-b"}})
	if got := asJSON(pod["imagePullSecrets"]); got != `[{"name":"ghcr-pull"},{"name":"reg-b"}]` {
		t.Errorf("imagePullSecrets = %s, want hive's plus configured, no error", got)
	}
}

func TestPullSecretsNoneLeavesFieldOff(t *testing.T) {
	api := newFakeAPI()
	api.selfPod = openshiftSelfPod
	pod := runRelayJob(t, api, Options{})
	if _, ok := pod["imagePullSecrets"]; ok {
		t.Errorf("imagePullSecrets = %s, want none", asJSON(pod["imagePullSecrets"]))
	}
}

func TestValidatePullSecretNames(t *testing.T) {
	if err := validatePullSecretNames([]string{"reg-a", "a.b-c"}); err != nil {
		t.Errorf("valid names refused: %v", err)
	}
	for _, bad := range []string{"KEY=value", "Upper", "has space", "-lead", ""} {
		if err := validatePullSecretNames([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := validatePullSecretNames(make([]string, maxConfiguredPullSecrets+1)); err == nil {
		t.Error("over-long list accepted")
	}
	if err := (Options{WorkspaceClaim: "c", ImagePullSecrets: []string{"a=b"}}).Validate(); err == nil || !strings.Contains(err.Error(), "names") {
		t.Errorf("Validate = %v, want a names-only refusal", err)
	}
}

func TestMergePullSecretNames(t *testing.T) {
	got := mergePullSecretNames([]string{"a", "b", ""}, []string{"b", "c"}, []string{"a", "d"})
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Errorf("merge = %v", got)
	}
}

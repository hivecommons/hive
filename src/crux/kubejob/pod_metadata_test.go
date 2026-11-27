package kubejob

import (
	"strings"
	"testing"
)

func TestPodMetadataPreservesJobSelectors(t *testing.T) {
	const name = "hive-kick-deps-1"
	l := &Launcher{Options: Options{
		WorkspaceClaim: "hive-data",
		PodLabels:      map[string]string{"example.com/internet-client": "true"},
		PodAnnotations: map[string]string{"example.com/policy": "sandbox"},
	}}
	m := l.manifest("hive", name, "sandbox/deps-1", specFor("/data/sandbox/deps-1"))
	jobLabels := m["metadata"].(map[string]any)["labels"].(map[string]string)
	pod := m["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
	podLabels := pod["labels"].(map[string]string)
	for _, labels := range []map[string]string{jobLabels, podLabels} {
		for k, want := range map[string]string{"example.com/internet-client": "true", managedByLabel: managedByValue, jobLabel: name, agentLabel: "hive-deps-sandbox"} {
			if labels[k] != want {
				t.Errorf("label %s = %q, want %q", k, labels[k], want)
			}
		}
	}
	if got := pod["annotations"].(map[string]string)["example.com/policy"]; got != "sandbox" {
		t.Errorf("pod annotation = %q, want sandbox", got)
	}
	policy := (&copyPlan{}).ingressOnlyPolicy("hive", name)
	selector := policy["spec"].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]string)
	for k, v := range selector {
		if podLabels[k] != v {
			t.Errorf("copy policy selector %s=%s does not match pod", k, v)
		}
	}
	for _, kv := range strings.Split(AgentLabelSelector("hive-deps-sandbox"), ",") {
		k, v, _ := strings.Cut(kv, "=")
		if podLabels[k] != v {
			t.Errorf("log selector %s does not match pod", kv)
		}
	}
}

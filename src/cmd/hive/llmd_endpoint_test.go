package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// k8sDeploymentPath is the self-hosted base Deployment, relative to this
// package.
const k8sDeploymentPath = "../../deploy/k8s/deployment.yaml"

// The base Deployment overrides HIVE_LLMD_ENDPOINT, so a stale value there
// wins over the code default. It pointed at the retired llm-d-epp Service
// while the code had moved to hive-llm-d-epp (#9726).
func TestK8sDeploymentLLMDEndpointMatchesDefault(t *testing.T) {
	raw, err := os.ReadFile(k8sDeploymentPath)
	if err != nil {
		t.Fatalf("read %s: %v", k8sDeploymentPath, err)
	}
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string `yaml:"name"`
						Env  []struct {
							Name  string `yaml:"name"`
							Value string `yaml:"value"`
						} `yaml:"env"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", k8sDeploymentPath, err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != "hive" {
			continue
		}
		for _, e := range c.Env {
			if e.Name != "HIVE_LLMD_ENDPOINT" {
				continue
			}
			if e.Value != defaultLLMDEndpoint {
				t.Errorf("%s sets HIVE_LLMD_ENDPOINT=%q, want %q (defaultLLMDEndpoint)", k8sDeploymentPath, e.Value, defaultLLMDEndpoint)
			}
			return
		}
		return
	}
	t.Fatalf("no container named hive in %s", k8sDeploymentPath)
}

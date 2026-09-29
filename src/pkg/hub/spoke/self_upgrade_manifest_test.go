package spoke

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// selfHostedDeploymentPath is the self-hosted base Deployment, relative to
// this package.
const selfHostedDeploymentPath = "../../../deploy/k8s/deployment.yaml"

// upgradeSelfMutableToSHA rolls the pod with an annotation and relies on
// imagePullPolicy: Always so the new pod re-pulls the floating tag. Without
// it Kubernetes defaults to IfNotPresent for any tag other than :latest and
// a node with the tag cached keeps the old build (#9332).
func TestSelfHostedDeploymentAlwaysPullsFloatingTag(t *testing.T) {
	raw, err := os.ReadFile(selfHostedDeploymentPath)
	if err != nil {
		t.Fatalf("read %s: %v", selfHostedDeploymentPath, err)
	}
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name            string `yaml:"name"`
						Image           string `yaml:"image"`
						ImagePullPolicy string `yaml:"imagePullPolicy"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", selfHostedDeploymentPath, err)
	}
	found := false
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != "hive" {
			continue
		}
		found = true
		if strings.Contains(c.Image, "@sha256:") {
			t.Skipf("hive image is pinned by digest (%s); pull policy does not matter", c.Image)
		}
		if c.ImagePullPolicy != "Always" {
			t.Errorf("hive container runs floating image %q with imagePullPolicy %q, want Always", c.Image, c.ImagePullPolicy)
		}
	}
	if !found {
		t.Fatalf("no container named hive in %s", selfHostedDeploymentPath)
	}
}

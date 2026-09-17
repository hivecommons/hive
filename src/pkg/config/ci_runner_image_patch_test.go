package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hivecommons/hive#7398.
//
// src/deploy/ci-runners/runner-image-patch.yaml is applied verbatim by an
// operator:
//
//	kubectl -n arc-systems patch runnerdeployment hivecommons-hive-runners \
//	  --type merge --patch-file src/deploy/ci-runners/runner-image-patch.yaml
//
// It shipped carrying `:REPLACE_ME`, so that documented command rolled every
// runner pod into ImagePullBackOff unless the operator remembered to hand-edit
// first. These tests pin the file to a real, immutable reference.

func ciRunnerImagePatch(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "ci-runners", "runner-image-patch.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// The image reference as the patch actually sets it — parsed, not grepped, so
// a tag mentioned only in the file's prose cannot satisfy these tests.
func ciRunnerPatchImage(t *testing.T) string {
	t.Helper()
	var patch struct {
		Spec struct {
			Template struct {
				Spec struct {
					Image string `yaml:"image"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(ciRunnerImagePatch(t)), &patch); err != nil {
		t.Fatalf("runner-image-patch.yaml must be valid YAML (kubectl --patch-file parses it): %v", err)
	}
	image := patch.Spec.Template.Spec.Image
	if image == "" {
		t.Fatal("runner-image-patch.yaml must set spec.template.spec.image — summerwind ARC takes the runner image there, and an empty patch silently leaves the pods on the stock image (#7398)")
	}
	return image
}

func TestCIRunnerImagePatchNamesAPublishedTag(t *testing.T) {
	image := ciRunnerPatchImage(t)

	const repo = "ghcr.io/hivecommons/hive-ci-runner"
	if !strings.HasPrefix(image, repo+":") && !strings.HasPrefix(image, repo+"@") {
		t.Fatalf("runner-image-patch.yaml image is %q, want a reference to %s — ci-runner-image.yml publishes there", image, repo)
	}

	// A placeholder is the #7398 bug: the documented kubectl patch command is
	// meant to be applied as written.
	for _, placeholder := range []string{"REPLACE_ME", "REPLACEME", "TODO", "CHANGEME"} {
		if strings.Contains(strings.ToUpper(image), placeholder) {
			t.Fatalf("runner-image-patch.yaml image is %q: applying this file as the README documents would put every runner pod into ImagePullBackOff (#7398)", image)
		}
	}

	// ARC does not re-pull a tag that has not changed, so a mutable tag makes
	// the deployed toolchain unknowable from the cluster.
	if strings.HasSuffix(image, ":latest") || strings.HasSuffix(image, ":main") {
		t.Fatalf("runner-image-patch.yaml image is %q: ARC does not re-pull an unchanged tag, so the reference must be immutable (#7398)", image)
	}

	// The published tag is `<runner version>-<tag_suffix>`, e.g.
	// v2.337.0-ubuntu-24.04-toolchain-1. Anything else is not something
	// ci-runner-image.yml can have pushed.
	if strings.Contains(image, ":") && !strings.Contains(image, "@sha256:") {
		tag := image[strings.LastIndex(image, ":")+1:]
		published := regexp.MustCompile(`^v\d+\.\d+\.\d+-ubuntu-\d+\.\d+-\S+-\d+$`)
		if !published.MatchString(tag) {
			t.Fatalf("runner-image-patch.yaml tag %q does not match the <runner version>-<tag_suffix> shape ci-runner-image.yml publishes (e.g. v2.337.0-ubuntu-24.04-toolchain-1) (#7398)", tag)
		}
	}
}

// The README's "Apply" step is what an operator follows. If it still tells
// them to edit a placeholder, or names a different tag than the patch sets,
// the hand-off that #7398 is about breaks again.
func TestCIRunnerREADMEMatchesTheImagePatch(t *testing.T) {
	image := ciRunnerPatchImage(t)
	path := filepath.Join("..", "..", "deploy", "ci-runners", "README.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	readme := string(body)

	if !strings.Contains(readme, image) {
		t.Fatalf("ci-runners/README.md does not mention %q, the image runner-image-patch.yaml sets — the two drift and the operator applies the wrong tag (#7398)", image)
	}
	if !strings.Contains(readme, "runner-image-patch.yaml") {
		t.Fatal("ci-runners/README.md must document applying runner-image-patch.yaml — it is the only place the cluster-side step is written down (#7398)")
	}
}

// ci-runner-image.yml's job summary prints the command an operator pastes to
// move the file onto a freshly published tag. It used to sed for the literal
// REPLACE_ME, which now matches nothing — a silent no-op that would leave the
// stale tag in place.
func TestCIRunnerImageWorkflowSummaryDoesNotSedThePlaceholder(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".github", "workflows", "ci-runner-image.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.Contains(string(body), "REPLACE_ME") {
		t.Fatal("ci-runner-image.yml still rewrites the REPLACE_ME placeholder, which runner-image-patch.yaml no longer carries — the sed would match nothing and the operator would patch the old tag (#7398)")
	}
}

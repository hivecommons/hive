package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// The Publish to repository block lives in the Knowledge pipeline renderer and
// saves through the existing pipeline endpoint; these tests pin its markup and
// the no-native-dialog rule.
func TestBeadSynthPublishSettingsUI(t *testing.T) {
	html := indexHTML(t)
	render := jsFunc(t, html, "renderKnowledgePipelineBody")
	save := jsFunc(t, html, "saveKnowledgePipeline")

	for _, want := range []string{
		"Publish to repository",
		"knowledge.bead_synthesizer.publish.enabled",
		"'pub-repo'", "'pub-path'", "'pub-base_branch'", "'pub-cadence'", "kp-pub-mode",
		"directCommitOK",
		"Pull request only",
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderKnowledgePipelineBody missing %q", want)
		}
	}
	if !strings.Contains(render, "dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')") {
		t.Error("publish block must be owner-gated like its sibling fields")
	}
	for _, want := range []string{"'pub-enabled'", "'pub-repo'", "'pub-path'", "'pub-base_branch'", "'pub-mode'", "'pub-cadence'", "/api/config/knowledge/pipeline", "showToast('Save failed: '"} {
		if !strings.Contains(save, want) {
			t.Errorf("saveKnowledgePipeline missing %q", want)
		}
	}
}

func TestBeadSynthPublishSettingsUINoNativeDialogs(t *testing.T) {
	html := indexHTML(t)
	native := regexp.MustCompile(`(^|[^.\w])(alert|confirm|prompt)\(`)
	for _, fn := range []string{"renderKnowledgePipelineBody", "saveKnowledgePipeline", "kpToggle"} {
		if native.MatchString(jsFunc(t, html, fn)) {
			t.Errorf("%s uses a native browser dialog; use showToast/hiveConfirm", fn)
		}
	}
}

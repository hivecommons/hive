package dashboard

import (
	"strings"
	"testing"
)

// hivecommons/hive#7667: the prompt editor's rendered preview is how an owner
// confirms project.writing_guide took. It substitutes the config-only subset
// of kick variables, and the guide is config-only, so it must render there —
// where the kick will place it — and render nothing when unset.

func TestSubstituteTemplateVars_WritingGuideRendersInThePreview(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.WritingGuide = "Short sentences. Evidence under a <details> block."

	tmpl := "before\n\n${WRITING_GUIDE}\n\n```bash\ngh issue create --body \"## Finding\"\n```"
	out := s.substituteTemplateVars(tmpl, "scanner")
	if strings.Contains(out, "${WRITING_GUIDE}") {
		t.Fatalf("preview left ${WRITING_GUIDE} unrendered — an owner cannot see the setting took:\n%s", out)
	}
	if !strings.Contains(out, "WRITING GUIDE") || !strings.Contains(out, "Short sentences. Evidence under a <details> block.") {
		t.Fatalf("preview does not show the guide where the kick places it:\n%s", out)
	}
	if strings.Index(out, "Short sentences.") > strings.Index(out, "gh issue create") {
		t.Fatalf("guide must render ahead of the body template it governs:\n%s", out)
	}
}

func TestSubstituteTemplateVars_WritingGuideUnsetRendersNothing(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.WritingGuide = ""

	out := s.substituteTemplateVars("a\n\n${WRITING_GUIDE}\n\nb", "scanner")
	if out != "a\n\n\n\nb" {
		t.Fatalf("an unset guide must render as empty, not as a literal or a header; got %q", out)
	}
}

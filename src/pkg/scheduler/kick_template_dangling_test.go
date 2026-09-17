package scheduler

// hivecommons/hive#7390: a kick_template that resolves nowhere failed
// silently — the success path logged "using config kick_template", the miss
// path logged nothing and fell through to the pack/convention template. These
// tests pin the provenance the fix adds: the miss is logged with the paths
// tried and the fallback, ResolveTemplate reports it for the prompt editor,
// and WarnDanglingKickTemplates catches it at boot.

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// danglingScheduler builds a scheduler whose "review" agent points at a
// template that exists nowhere and whose "scanner" points at a shipped one,
// with the policy dirs redirected to empty temp dirs and the log captured.
func danglingScheduler(t *testing.T) (*Scheduler, *bytes.Buffer) {
	t.Helper()
	prevUser, prevCloned := userSavedPolicyDir, clonedPoliciesDir
	userSavedPolicyDir, clonedPoliciesDir = t.TempDir(), t.TempDir()
	t.Cleanup(func() { userSavedPolicyDir, clonedPoliciesDir = prevUser, prevCloned })

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "projectbluefin", Repos: []string{"projectbluefin/testsuite"}},
		Agents: map[string]config.AgentConfig{
			"review":  {Backend: "claude", Mode: "ISSUES_AND_PRS", KickTemplate: "review.md"},
			"scanner": {Backend: "claude", Mode: "ISSUES_AND_PRS", KickTemplate: "scanner-holdgated.md"},
			"quality": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
		},
	}
	return New(cfg, logger), &buf
}

func TestResolveNamedTemplate_Provenance(t *testing.T) {
	s, _ := danglingScheduler(t)

	content, source, tried := s.resolveNamedTemplate("scanner-holdgated.md")
	if content == "" || source != TemplateSourceEmbedded {
		t.Fatalf("shipped template: content=%d bytes source=%q, want embedded default", len(content), source)
	}
	if len(tried) == 0 || !strings.Contains(tried[len(tried)-1], "pkg/policies/defaults/scanner-holdgated.md") {
		t.Errorf("tried paths must end with the embedded location: %v", tried)
	}

	content, source, tried = s.resolveNamedTemplate("review.md")
	if content != "" || source != "" {
		t.Fatalf("dangling template resolved to something: source=%q", source)
	}
	if len(tried) < 3 || !strings.HasPrefix(tried[0], userSavedPolicyDir) {
		t.Errorf("tried paths must start with the user override dir and list every location: %v", tried)
	}

	// A user override wins and is reported as the source.
	if err := os.WriteFile(userSavedPolicyDir+"/review.md", []byte("# override"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, source, _ = s.resolveNamedTemplate("review.md")
	if content != "# override" || source != userSavedPolicyDir+"/review.md" {
		t.Errorf("override not reported: content=%q source=%q", content, source)
	}
}

func TestResolveTemplate_ReportsDanglingAndFallback(t *testing.T) {
	s, _ := danglingScheduler(t)

	res := s.ResolveTemplate("review")
	if res.KickTemplate != "review.md" || res.Resolved || res.Source != "" || res.EmbeddedDefaultExists {
		t.Errorf("dangling resolution = %+v, want unresolved with no source and no embedded default", res)
	}
	if len(res.PathsTried) == 0 {
		t.Error("PathsTried must list where the template was expected")
	}
	if res.Fallback != "hardcoded kick for review" {
		t.Errorf("Fallback = %q, want the hardcoded kick (no pack, no review.md convention template)", res.Fallback)
	}

	res = s.ResolveTemplate("scanner")
	if !res.Resolved || res.Source != TemplateSourceEmbedded || !res.EmbeddedDefaultExists {
		t.Errorf("shipped template resolution = %+v, want resolved from the embedded default", res)
	}
	if res.Fallback != "convention template scanner.md" {
		t.Errorf("scanner fallback = %q, want the shipped scanner.md convention template", res.Fallback)
	}

	// No kick_template at all: nothing configured, fallback still described.
	res = s.ResolveTemplate("quality")
	if res.KickTemplate != "" || res.Resolved || len(res.PathsTried) != 0 || res.Fallback == "" {
		t.Errorf("unset resolution = %+v", res)
	}

	// Nil-safety for a bare scheduler.
	if r := (*Scheduler)(nil).ResolveTemplate("x"); r.Agent != "x" || r.Resolved {
		t.Errorf("nil scheduler resolution = %+v", r)
	}
	if _, ok := s.TemplateExists("review.md"); ok {
		t.Error("TemplateExists must be false for a dangling name")
	}
	if src, ok := s.TemplateExists("scanner-holdgated.md"); !ok || src != TemplateSourceEmbedded {
		t.Errorf("TemplateExists(shipped) = (%q, %v)", src, ok)
	}
}

// The miss is logged at WARN with agent, template, fallback and paths — the
// same weight the success path already had. On the parent commit this kick
// built without a single line about the missing template.
func TestBuildAgentMessage_WarnsOnDanglingKickTemplate(t *testing.T) {
	s, buf := danglingScheduler(t)
	msg := s.BuildAgentMessage("review", nil, &github.ActionableResult{})
	if msg == "" {
		t.Fatal("the kick must still be built from the fallback")
	}
	log := buf.String()
	for _, want := range []string{
		"level=WARN", "config kick_template not found; falling back",
		"agent=review", "template=review.md", "fallback=\"hardcoded kick for review\"", "paths_tried=",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}

	buf.Reset()
	s.BuildAgentMessage("scanner", nil, &github.ActionableResult{})
	if log := buf.String(); strings.Contains(log, "level=WARN") || !strings.Contains(log, "using config kick_template") {
		t.Errorf("a resolving kick_template must log the success line and no warning:\n%s", log)
	}
}

func TestWarnDanglingKickTemplates_AtBoot(t *testing.T) {
	s, buf := danglingScheduler(t)
	got := s.WarnDanglingKickTemplates()
	if len(got) != 1 || got["review"] != "review.md" {
		t.Fatalf("dangling = %v, want only review→review.md", got)
	}
	log := buf.String()
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "kick_template does not resolve") || !strings.Contains(log, "agent=review") {
		t.Errorf("boot warning missing:\n%s", log)
	}
	if strings.Contains(log, "agent=scanner") {
		t.Errorf("a resolving template must not be warned about:\n%s", log)
	}
	if (*Scheduler)(nil).WarnDanglingKickTemplates() != nil {
		t.Error("nil scheduler must be a no-op")
	}
}

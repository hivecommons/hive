package spektacular

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFakeSpektacular(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary is POSIX-only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "spektacular")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbe_ReportsPresentBinaryAndVersion(t *testing.T) {
	bin := writeFakeSpektacular(t, `echo "spektacular 0.22.0"`)
	res, err := Probe(context.Background(), " "+bin+" ")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Present || res.Version != "spektacular 0.22.0" || res.Binary != bin {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestProbe_MissingBinaryIsNotPresent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	res, err := Probe(context.Background(), missing)
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if res.Present || res.Version != "" || res.Binary != missing {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(err.Error(), "spektacular --version") {
		t.Fatalf("error should name the probe command: %v", err)
	}
}

func TestProbe_NonZeroExitSurfacesStderr(t *testing.T) {
	bin := writeFakeSpektacular(t, `echo "boom: no license" >&2; exit 3`)
	_, err := Probe(context.Background(), bin)
	if err == nil || !strings.Contains(err.Error(), "boom: no license") {
		t.Fatalf("expected stderr in error, got %v", err)
	}
}

func TestProbe_EmptyBinaryDefaultsToPATHName(t *testing.T) {
	// Point PATH at a dir that only has our fake so the default name resolves.
	bin := writeFakeSpektacular(t, `echo v1`)
	t.Setenv("PATH", filepath.Dir(bin))
	res, err := Probe(context.Background(), "")
	if err != nil {
		t.Fatalf("Probe with default name: %v", err)
	}
	if res.Binary != "spektacular" || res.Version != "v1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestWorkDirError_MessageIncludesAvailableFields(t *testing.T) {
	cases := []struct {
		err  WorkDirError
		want string
	}{
		{WorkDirError{}, "spektacular: no repo workdir resolved"},
		{WorkDirError{RunKey: "r1"}, "spektacular: no repo workdir resolved run=r1"},
		{WorkDirError{RunKey: "r1", Stage: KindPlan, Repo: "o/r"}, "spektacular: no repo workdir resolved run=r1 stage=plan repo=o/r"},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
	var target *WorkDirError
	wrapped := errors.Join(errors.New("outer"), &WorkDirError{RunKey: "x"})
	if !errors.As(wrapped, &target) || target.RunKey != "x" {
		t.Fatal("WorkDirError should be matchable via errors.As")
	}
}

func TestExportPlanWithFallback_PublicWrapperFallsBackToTasksJSON(t *testing.T) {
	ex := &scriptedExec{
		exportErr:  errors.New("exit status 1"),
		exportJSON: `{"error":true,"code":"unknown_subcommand","message":"unknown subcommand \"export\" for \"spektacular plan\""}`,
		files: map[string]string{
			"feature-x/tasks.json": `{"tasks":[{"id":"T1","title":"Do it","repo":"myorg/repo1"}]}`,
		},
	}
	r := &Runner{Exec: ex.exec}
	plan, err := r.ExportPlanWithFallback(context.Background(), "feature-x")
	if err != nil {
		t.Fatalf("ExportPlanWithFallback: %v", err)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].ID != "T1" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	for _, d := range ex.dirs {
		if d != "" {
			t.Fatalf("public wrapper must not pin a dir, got %q", d)
		}
	}
}

func TestExportPlanFallback_PublicWrapperReadsPlanMarkdown(t *testing.T) {
	ex := &scriptedExec{
		files: map[string]string{
			"feature-y/plan.md": "# plan\n- [T1] Title (repo: myorg/repo1)\n",
		},
	}
	r := &Runner{Exec: ex.exec}
	plan, err := r.ExportPlanFallback(context.Background(), "feature-y")
	if err != nil {
		t.Fatalf("ExportPlanFallback: %v", err)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].ID != "T1" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if _, err := r.ExportPlanFallback(context.Background(), "   "); err == nil {
		t.Fatal("empty artifact name must be rejected")
	}
}

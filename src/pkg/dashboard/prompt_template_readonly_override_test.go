package dashboard

// hivecommons/hive#13005: on a hosted hive the shipped/seeded template in
// /data/policies can be read-only to the hive process, and saving from the
// prompt editor failed with "permission denied". The save must land in the
// writable override dir, the editor must reopen on the saved text, and the
// scheduler must resolve the override ahead of the shipped default.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/scheduler"
)

// pinPromptTemplateDirs points the dashboard's save/override dirs and the
// scheduler's matching roots at the given directories for one test.
func pinPromptTemplateDirs(t *testing.T, saveDir, overrideDir string) {
	t.Helper()
	prevSave, prevOverride := promptTemplateSaveDir, promptTemplateOverrideDir
	promptTemplateSaveDir, promptTemplateOverrideDir = saveDir, overrideDir
	t.Cleanup(func() { promptTemplateSaveDir, promptTemplateOverrideDir = prevSave, prevOverride })
	t.Cleanup(scheduler.SetPolicyDirsForTest(saveDir, t.TempDir()))
	t.Cleanup(scheduler.SetPolicyOverrideDirForTest(overrideDir))
}

func TestPromptTemplateSave_ReadOnlyDefaultUsesOverride(t *testing.T) {
	const tplName = "scanner-holdgated.md"
	const shipped = "# shipped default\n${GH_AUTH}\n"
	const edited = "# edited\n${GH_AUTH}\n${ISSUE_LIST}\n${PR_LIST}\n${KNOWLEDGE}\n"

	cases := []struct {
		name         string
		readOnly     bool
		failOverride bool
		// setup seeds the default dir and makes it (or its file) unwritable.
		setup func(t *testing.T, saveDir string)
	}{
		{
			name: "writable default is saved in place",
			setup: func(t *testing.T, saveDir string) {
				writePromptFixture(t, filepath.Join(saveDir, tplName), shipped, 0o644)
			},
		},
		{
			name:     "read-only shipped file",
			readOnly: true,
			setup: func(t *testing.T, saveDir string) {
				writePromptFixture(t, filepath.Join(saveDir, tplName), shipped, 0o444)
			},
		},
		{
			name:     "read-only policies dir",
			readOnly: true,
			setup: func(t *testing.T, saveDir string) {
				writePromptFixture(t, filepath.Join(saveDir, tplName), shipped, 0o444)
				chmodPromptDir(t, saveDir, 0o555)
			},
		},
		{
			name:         "override dir also unwritable reports both paths",
			readOnly:     true,
			failOverride: true,
			setup: func(t *testing.T, saveDir string) {
				writePromptFixture(t, filepath.Join(saveDir, tplName), shipped, 0o444)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.readOnly && os.Geteuid() == 0 {
				t.Skip("root bypasses file permissions; the read-only path is unreachable")
			}
			s, deps := promptServer(t)
			saveDir := t.TempDir()
			overrideDir := filepath.Join(t.TempDir(), "policy-overrides")
			if tc.failOverride {
				parent := t.TempDir()
				overrideDir = filepath.Join(parent, "policy-overrides")
				chmodPromptDir(t, parent, 0o555)
			}
			pinPromptTemplateDirs(t, saveDir, overrideDir)
			tc.setup(t, saveDir)

			defaultPath := filepath.Join(saveDir, tplName)
			overridePath := filepath.Join(overrideDir, tplName)

			// The editor is told up front whether the shipped copy is read-only.
			before := decodeJSON(t, doGet(s, "/api/config/agent/scanner/prompt?raw=1"))
			tf, _ := before["templateFile"].(map[string]any)
			if tf == nil || tf["readOnly"] != tc.readOnly || tf["defaultPath"] != defaultPath {
				t.Fatalf("templateFile before save = %v, want readOnly=%v defaultPath=%s", tf, tc.readOnly, defaultPath)
			}
			wantSave := defaultPath
			if tc.readOnly {
				wantSave = overridePath
			}
			if tf["savePath"] != wantSave {
				t.Errorf("templateFile.savePath = %v, want %s", tf["savePath"], wantSave)
			}

			rec := doPut(s, "/api/config/agent/scanner/prompt", map[string]any{"template": edited})
			if tc.failOverride {
				if rec.Code != http.StatusInternalServerError {
					t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
				}
				msg, _ := decodeJSON(t, rec)["error"].(string)
				if !strings.Contains(msg, defaultPath) || !strings.Contains(msg, overridePath) {
					t.Errorf("error must name the read-only default and the override path, got %q", msg)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("save status = %d: %s", rec.Code, rec.Body.String())
			}
			resp := decodeJSON(t, rec)
			if resp["path"] != wantSave || resp["override"] != tc.readOnly {
				t.Errorf("save response = %v, want path=%s override=%v", resp, wantSave, tc.readOnly)
			}

			if tc.readOnly {
				if got := readPromptFixture(t, defaultPath); got != shipped {
					t.Errorf("shipped default was modified: %q", got)
				}
			}
			if got := readPromptFixture(t, wantSave); got != edited {
				t.Errorf("saved file = %q, want %q", got, edited)
			}

			// Reopening the editor shows the saved text with placeholders intact.
			after := decodeJSON(t, doGet(s, "/api/config/agent/scanner/prompt?raw=1"))
			if after["prompt"] != edited {
				t.Errorf("reopened prompt = %q, want %q", after["prompt"], edited)
			}
			tf, _ = after["templateFile"].(map[string]any)
			if tf == nil || tf["path"] != wantSave || tf["override"] != tc.readOnly {
				t.Errorf("templateFile after save = %v, want path=%s override=%v", tf, wantSave, tc.readOnly)
			}

			// The next kick resolves the saved template, override ahead of default.
			res := deps.Scheduler.ResolveTemplate("scanner")
			if !res.Resolved || res.Source != wantSave {
				t.Errorf("scheduler resolution = %+v, want source %s", res, wantSave)
			}
			if len(res.PathsTried) == 0 || res.PathsTried[0] != overridePath {
				t.Errorf("override dir must be probed first, tried %v", res.PathsTried)
			}

			// A second save keeps updating the override that shadows the default.
			const again = "# edited again ${GH_AUTH}\n"
			if rec := doPut(s, "/api/config/agent/scanner/prompt", map[string]any{"template": again}); rec.Code != http.StatusOK {
				t.Fatalf("second save status = %d: %s", rec.Code, rec.Body.String())
			}
			if got := decodeJSON(t, doGet(s, "/api/config/agent/scanner/prompt?raw=1"))["prompt"]; got != again {
				t.Errorf("reopened prompt after second save = %q, want %q", got, again)
			}
		})
	}
}

func writePromptFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func readPromptFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// chmodPromptDir sets dir's mode and restores 0755 at cleanup so t.TempDir can
// remove it.
func chmodPromptDir(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

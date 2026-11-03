package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifySourceEditRejectsExecutableWorkspaceConfig(t *testing.T) {
	for _, key := range []string{
		"core.fsmonitor", "core.hooksPath", "core.askPass", "core.sshCommand",
		"diff.external", "diff.fixture.textconv", "filter.fixture.clean",
		"filter.fixture.process", "credential.helper", "core.attributesFile",
		"include.path", "remote.origin.uploadpack", "core.worktree",
	} {
		t.Run(key, func(t *testing.T) {
			dir, file, _, start, now := codeWorkTestWorktree(t)
			marker := filepath.Join(dir, "helper-ran")
			script := filepath.Join(dir, "helper")
			if err := os.WriteFile(script, []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			value := script
			switch key {
			case "core.hooksPath":
				value = filepath.Join(dir, "hooks")
				if err := os.Mkdir(value, 0700); err != nil {
					t.Fatal(err)
				}
				for _, hook := range []string{"pre-commit", "post-commit", "post-index-change"} {
					if err := os.WriteFile(filepath.Join(value, hook), []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			case "include.path":
				value = filepath.Join(dir, "included-config")
				if err := os.WriteFile(value, []byte("[core]\nfsmonitor = "+script+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "credential.helper":
				value = "!" + script
			case "core.worktree":
				value = t.TempDir()
			}
			if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("source.go filter=fixture diff=fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			codeWorkTestGit(t, dir, "config", key, value)
			if got := verifySourceEdit(sourceEdit{path: file, started: start, at: now}, now); got != nil {
				t.Fatalf("unsafe workspace invented source work: %+v", got)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("workspace helper ran or marker unreadable: %v", err)
			}
		})
	}
}

func TestVerifySourceEditIgnoresHostileGitEnvironment(t *testing.T) {
	dir, file, head, start, now := codeWorkTestWorktree(t)
	decoy, _, _, _, _ := codeWorkTestWorktree(t)
	codeWorkTestGit(t, decoy, "remote", "set-url", "origin", "https://github.com/wrong/repo.git")
	codeWorkTestGit(t, decoy, "add", "source.go")
	codeWorkTestGit(t, decoy, "commit", "--quiet", "-s", "-m", "different decoy head")
	marker := filepath.Join(dir, "inherited-helper-ran")
	script := filepath.Join(dir, "inherited-helper")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "inherited-config")
	if err := os.WriteFile(config, []byte("[core]\nfsmonitor = "+script+"\n[diff]\nexternal = "+script+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"GIT_DIR":                          filepath.Join(decoy, ".git"),
		"GIT_COMMON_DIR":                   filepath.Join(decoy, ".git"),
		"GIT_WORK_TREE":                    decoy,
		"GIT_INDEX_FILE":                   filepath.Join(decoy, ".git", "index"),
		"GIT_OBJECT_DIRECTORY":             filepath.Join(decoy, ".git", "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(decoy, ".git", "objects"),
		"GIT_CONFIG":                       config,
		"GIT_CONFIG_SYSTEM":                config,
		"GIT_CONFIG_GLOBAL":                config,
		"GIT_CONFIG_COUNT":                 "1",
		"GIT_CONFIG_KEY_0":                 "core.fsmonitor",
		"GIT_CONFIG_VALUE_0":               script,
		"GIT_CONFIG_PARAMETERS":            "'core.fsmonitor=" + script + "'",
		"GIT_TRACE":                        marker,
		"GIT_TRACE_SETUP":                  marker,
		"GIT_EXTERNAL_DIFF":                script,
		"GIT_SSH_COMMAND":                  script,
		"GIT_ASKPASS":                      script,
		"GIT_PROXY_COMMAND":                script,
		"GIT_EXEC_PATH":                    decoy,
		"GIT_NAMESPACE":                    "decoy",
		"GIT_REPLACE_REF_BASE":             "refs/decoy/",
		"GIT_NO_REPLACE_OBJECTS":           "0",
		"GIT_OPTIONAL_LOCKS":               "1",
		"GIT_ATTR_SOURCE":                  "refs/heads/decoy",
	} {
		t.Setenv(key, value)
	}
	got := verifySourceEdit(sourceEdit{path: file, started: start, at: now}, now)
	if got == nil || got.Repo != "org/repo" || got.Head != head || got.Path != "source.go" || !got.At.Equal(now) || !got.ObservedAt.Equal(now) {
		t.Fatalf("inherited Git state retargeted actual repository/head/path: %+v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("inherited Git helper/trace executed or marker unreadable: %v", err)
	}
	// Keep the receipt and file timestamp valid: only actual content changes
	// distinguish this negative, even while the decoy worktree is dirty.
	if err := os.WriteFile(file, []byte("package before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, start, start); err != nil {
		t.Fatal(err)
	}
	if got := verifySourceEdit(sourceEdit{path: file, started: start, at: now}, now); got != nil {
		t.Fatalf("unchanged real file borrowed decoy source work: %+v", got)
	}
}

func TestVerifySourceEditNormalizationRemainsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configKey string
		value     string
		attribute string
	}{
		{name: "autocrlf", configKey: "core.autocrlf", value: "true"},
		{name: "eol", configKey: "core.eol", value: "crlf"},
		{name: "text", attribute: "text=auto"},
		{name: "eol attribute", attribute: "eol=lf"},
		{name: "ident", attribute: "ident"},
		{name: "encoding", attribute: "working-tree-encoding=UTF-16"},
		{name: "legacy crlf", attribute: "crlf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, file, _, start, now := codeWorkTestWorktree(t)
			if tc.configKey != "" {
				codeWorkTestGit(t, dir, "config", tc.configKey, tc.value)
			}
			if tc.attribute != "" {
				if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("source.go "+tc.attribute+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := verifySourceEdit(sourceEdit{path: file, started: start, at: now}, now); got != nil {
				t.Fatalf("unsupported normalization invented a verified Git change: %+v", got)
			}
		})
	}
}

func TestVerifySourceEditUntrackedIgnoredAndSSHOrigin(t *testing.T) {
	dir, file, head, start, now := codeWorkTestWorktree(t)
	codeWorkTestGit(t, dir, "remote", "set-url", "origin", "git@github.com:org/repo.git")
	got := verifySourceEdit(sourceEdit{path: file, started: start, at: now}, now)
	if got == nil || got.Repo != "org/repo" || got.Head != head {
		t.Fatalf("actual SCP origin lost provenance: %+v", got)
	}
	added := filepath.Join(dir, "added.go")
	if err := os.WriteFile(added, []byte("package added\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(added, start, start); err != nil {
		t.Fatal(err)
	}
	if got := verifySourceEdit(sourceEdit{path: added, started: start, at: now}, now); got == nil || got.Path != "added.go" || got.Head != head {
		t.Fatalf("nonignored source creation lost actual head/path: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("added.go\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := verifySourceEdit(sourceEdit{path: added, started: start, at: now}, now); got != nil {
		t.Fatalf("ignored file invented source work: %+v", got)
	}
}

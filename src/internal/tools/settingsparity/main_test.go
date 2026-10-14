package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fixtureConfig = `package config

type Config struct {
	Name  string ` + "`yaml:\"name\"`" + `
	Hub   Hub    ` + "`yaml:\"hub\"`" + `
	Pipes string ` + "`yaml:\"pipes\"`" + `
}

type Hub struct {
	URL   string ` + "`yaml:\"url\"`" + `
	Token string ` + "`yaml:\"token\"`" + `
}
`

const fixtureExample = `# hive.yaml example
name: hive  # inline comment
pipes: "a|b"
hub:
  url: https://hub.example
  token:
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureRepo builds a minimal repo and changes into its src/ directory,
// which is where the tool is run from.
func fixtureRepo(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "src", "pkg", "config", "config.go"), fixtureConfig)
	writeFile(t, filepath.Join(root, "src", "pkg", "dashboard", "theme", "theme.go"), "package theme\n")
	writeFile(t, filepath.Join(root, "src", "hive.yaml.example"), fixtureExample)
	writeFile(t, filepath.Join(root, "src", "pkg", "dashboard", "static", "index.html"), `<input data-key="hub.url">`)
	writeFile(t, filepath.Join(root, "src", "pkg", "dashboard", "settings.go"), "package dashboard\n\nconst k = \"name\"\n")
	writeFile(t, filepath.Join(root, "src", "pkg", "dashboard", "notes.txt"), "pipes")
	t.Chdir(filepath.Join(root, "src"))
}

func TestRunMarkdown(t *testing.T) {
	fixtureRepo(t)
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr: %s", code, stderr.String())
	}
	want := strings.Join([]string{
		"| key | type | default | exposed-in-UI | where | notes |",
		"| --- | --- | --- | --- | --- | --- |",
		"| `agents.github_api_hourly_cap` | `int` |  | none |  | may require restart or reconnect |",
		"| `hub.token` | `string` |  | none |  | secret/credential — expose as masked or write-only |",
		"| `hub.url` | `string` | https://hub.example | read/write | Settings dialog reference | may require restart or reconnect |",
		"| `name` | `string` | hive | read/write | Settings dialog reference |  |",
		"| `pipes` | `string` | \"a\\|b\" | none |  |  |",
		"",
	}, "\n")
	if got := stdout.String(); got != want {
		t.Fatalf("markdown output mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunJSON(t *testing.T) {
	fixtureRepo(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-format", "json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr: %s", code, stderr.String())
	}
	var rows []row
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, stdout.String())
	}
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5: %+v", len(rows), rows)
	}
	want := row{Key: "hub.url", Type: "string", Default: "https://hub.example", UI: "read/write", Where: "Settings dialog reference", Notes: "may require restart or reconnect"}
	if rows[2] != want {
		t.Fatalf("rows[2] = %+v, want %+v", rows[2], want)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRunErrors(t *testing.T) {
	t.Run("bad flag", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-nope"}, &stdout, &stderr); code != 2 {
			t.Fatalf("run = %d, want 2", code)
		}
		if !strings.Contains(stderr.String(), "nope") {
			t.Fatalf("stderr should name the bad flag: %q", stderr.String())
		}
	})

	t.Run("not inside a repo", func(t *testing.T) {
		t.Chdir(t.TempDir())
		var stdout, stderr bytes.Buffer
		if code := run(nil, &stdout, &stderr); code != 1 {
			t.Fatalf("run = %d, want 1", code)
		}
		if stderr.Len() == 0 {
			t.Fatal("the failure must be reported on stderr")
		}
	})

	t.Run("json write failure", func(t *testing.T) {
		fixtureRepo(t)
		var stderr bytes.Buffer
		if code := run([]string{"-format=json"}, failingWriter{}, &stderr); code != 1 {
			t.Fatalf("run = %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "write failed") {
			t.Fatalf("stderr = %q, want the write error", stderr.String())
		}
	})
}

func TestReferenced(t *testing.T) {
	corpus := "settings: hub.url labels.<key>"
	for path, want := range map[string]bool{
		"hub.url":         true,
		"labels.<key>":    true,
		"agents.<key>.x":  false,
		"hooks[].url":     false,
		"hub.unmentioned": false,
	} {
		if got := referenced(corpus, path); got != want {
			t.Errorf("referenced(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestExampleDefaults(t *testing.T) {
	if got := exampleDefaults(filepath.Join(t.TempDir(), "missing.yaml")); got != nil {
		t.Fatalf("missing file: got %v, want nil", got)
	}
	path := filepath.Join(t.TempDir(), "hive.yaml.example")
	writeFile(t, path, strings.Join([]string{
		"# leading comment",
		"",
		"name: hive  # inline",
		"plain line without a colon",
		"agents:",
		"  model: \"sonnet\"",
		"  list:",
		"    - item: x",
		"hub:",
		"      deep:",
		"'quoted': yes   ",
		"other:",
		"  child: 3",
	}, "\n"))
	want := map[string]string{
		"name":         "hive",
		"agents.model": `"sonnet"`,
		"quoted":       "yes",
		"other.child":  "3",
	}
	if got := exampleDefaults(path); !reflect.DeepEqual(got, want) {
		t.Fatalf("exampleDefaults = %v, want %v", got, want)
	}
}

func TestNotesFor(t *testing.T) {
	const secret = "secret/credential — expose as masked or write-only"
	const restart = "may require restart or reconnect"
	for path, want := range map[string]string{
		"hub.token":          secret,
		"auth.client_secret": secret,
		"api_key":            secret,
		"tls.pem":            secret,
		"db.password":        secret,
		"agents.launch_cmd":  restart,
		"agents.backend":     restart,
		"service.url":        restart,
		"hub.name":           "hub-managed hives may render read-only",
		"name":               "",
	} {
		if got := notesFor(path); got != want {
			t.Errorf("notesFor(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestCellAndEsc(t *testing.T) {
	if got := cell(""); got != "" {
		t.Fatalf("cell(\"\") = %q, want empty", got)
	}
	if got := cell("a|b\nc"); got != `a\|b c` {
		t.Fatalf("cell = %q, want %q", got, `a\|b c`)
	}
	if got := esc("x|y"); got != `x\|y` {
		t.Fatalf("esc = %q, want %q", got, `x\|y`)
	}
}

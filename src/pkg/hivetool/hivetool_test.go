package hivetool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/codeindex"
	"github.com/hivecommons/hive/pkg/turn"
)

type fakeReader struct {
	files  map[string]string
	issues map[int]Issue
	reads  int
}

func (f *fakeReader) Issue(_ context.Context, _ string, n int) (Issue, error) {
	is, ok := f.issues[n]
	if !ok {
		return Issue{}, errors.New("not found")
	}
	return is, nil
}

func (f *fakeReader) List(_ context.Context, _, _, dir string) ([]Entry, error) {
	var out []Entry
	seen := map[string]bool{}
	for p := range f.files {
		rest := p
		if dir != "" {
			if !strings.HasPrefix(p, dir+"/") {
				continue
			}
			rest = strings.TrimPrefix(p, dir+"/")
		}
		name, _, isDir := strings.Cut(rest, "/")
		full := strings.TrimPrefix(dir+"/"+name, "/")
		if seen[full] {
			continue
		}
		seen[full] = true
		typ := "file"
		if isDir {
			typ = "dir"
		}
		out = append(out, Entry{Path: full, Type: typ})
	}
	return out, nil
}

func (f *fakeReader) File(_ context.Context, _, _, file string) ([]byte, error) {
	f.reads++
	s, ok := f.files[file]
	if !ok {
		return nil, fmt.Errorf("not found: %s", file)
	}
	return []byte(s), nil
}

func (f *fakeReader) Checks(context.Context, string, string) ([]CheckRun, error) {
	return []CheckRun{{Name: "unit", Status: "completed", Conclusion: "failure", Summary: "2 tests failed"}}, nil
}

type fakeSearcher struct{ hits []Hit }

func (s fakeSearcher) Search(_ context.Context, _, _, q, _ string, limit int) ([]Hit, error) {
	var out []Hit
	for _, h := range s.hits {
		if strings.Contains(h.Text, q) {
			out = append(out, h)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func newExec() (*Executor, *fakeReader) {
	lines := make([]string, 450)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	r := &fakeReader{
		files: map[string]string{
			"src/pkg/a.go":    "package a\n\nfunc Rebase() {}\n",
			"src/pkg/big.go":  strings.Join(lines, "\n") + "\n",
			"README.md":       "# hive\n",
			"src/cmd/main.go": "package main\n",
		},
		issues: map[int]Issue{7: {Number: 7, Title: "stuck PR", Body: "details", Labels: []string{"kind/bug"}, State: "open"}},
	}
	return &Executor{Repo: "o/r", Ref: "v6", Reader: r, Searcher: fakeSearcher{hits: []Hit{{Path: "src/pkg/a.go", Line: 3, Text: "func Rebase() {}"}}}}, r
}

func call(id, name, args string) turn.ToolCall {
	return turn.ToolCall{ID: id, Name: name, Arguments: args}
}

func run(t *testing.T, x *Executor, c turn.ToolCall) (string, error) {
	t.Helper()
	res, err := x.Execute(context.Background(), c)
	return res.Output, err
}

func TestReadTools(t *testing.T) {
	x, _ := newExec()
	out, err := run(t, x, call("1", ToolIssueRead, `{"number":"7"}`))
	if err != nil || !strings.Contains(out, "#7 [open] stuck PR") || !strings.Contains(out, "kind/bug") {
		t.Fatalf("issue.read = %q, %v", out, err)
	}
	out, err = run(t, x, call("2", ToolRepoList, `{"path":"src"}`))
	if err != nil || out != "dir src/cmd\ndir src/pkg\n" {
		t.Fatalf("list = %q, %v", out, err)
	}
	out, err = run(t, x, call("3", ToolRepoRead, `{"path":"/src/pkg/a.go","line_start":3,"line_end":3}`))
	if err != nil || !strings.Contains(out, "lines 3-3 of 3 (ref v6)") || !strings.Contains(out, "3\tfunc Rebase() {}") {
		t.Fatalf("read = %q, %v", out, err)
	}
	out, err = run(t, x, call("4", ToolRepoSearch, `{"query":"Rebase"}`))
	if err != nil || out != "src/pkg/a.go:3: func Rebase() {}\n" {
		t.Fatalf("search = %q, %v", out, err)
	}
	out, err = run(t, x, call("5", ToolRepoSearch, `{"query":"Nope","path":"src"}`))
	if err != nil || !strings.Contains(out, `no matches for "Nope" under src`) {
		t.Fatalf("empty search = %q, %v", out, err)
	}
	out, err = run(t, x, call("6", ToolChecksRead, `{}`))
	if err != nil || out != "unit: completed/failure — 2 tests failed\n" {
		t.Fatalf("checks = %q, %v", out, err)
	}
}

func TestReadFileIsBoundedAndPaged(t *testing.T) {
	x, _ := newExec()
	out, err := run(t, x, call("1", ToolRepoRead, `{"path":"src/pkg/big.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "lines 1-200 of 450") || !strings.Contains(out, "continue with start_line=201") || strings.Contains(out, "line 201\n") {
		t.Fatalf("paging header/hint wrong: %.200q", out)
	}
	if _, err := run(t, x, call("2", ToolRepoRead, `{"path":"src/pkg/big.go","start_line":999}`)); err == nil {
		t.Fatal("start past EOF must error")
	}
}

func TestPathConfinement(t *testing.T) {
	x, r := newExec()
	for _, p := range []string{"../etc/passwd", "src/../../x", "..\\\\x"} {
		if _, err := run(t, x, call(p, ToolRepoRead, fmt.Sprintf(`{"path":%q}`, p))); err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Errorf("%s: err = %v", p, err)
		}
	}
	if r.reads != 0 {
		t.Fatal("escaping paths must not reach the reader")
	}
}

func TestDuplicateCallsAreRefusedButFailuresRetryable(t *testing.T) {
	x, r := newExec()
	if _, err := run(t, x, call("a", ToolRepoRead, `{"path":"README.md","start_line":1}`)); err != nil {
		t.Fatal(err)
	}
	// Same arguments, different key order and id: still a duplicate.
	_, err := run(t, x, call("b", ToolRepoRead, `{"start_line":1,"path":"README.md"}`))
	if err == nil || !strings.Contains(err.Error(), "duplicate call") || !strings.Contains(err.Error(), "(call a)") {
		t.Fatalf("dup err = %v", err)
	}
	if r.reads != 1 {
		t.Fatalf("reader called %d times", r.reads)
	}
	// A failed call is forgotten so a retry runs.
	if _, err := run(t, x, call("c", ToolRepoRead, `{"path":"missing.go"}`)); err == nil {
		t.Fatal("want not found")
	}
	r.files["missing.go"] = "x\n"
	if _, err := run(t, x, call("d", ToolRepoRead, `{"path":"missing.go"}`)); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
}

func TestTerminalOutcome(t *testing.T) {
	x, _ := newExec()
	if _, ok := x.Outcome(); ok {
		t.Fatal("no outcome yet")
	}
	if _, err := run(t, x, call("1", ToolTaskComplete, `{}`)); err == nil {
		t.Fatal("summary required")
	}
	if _, err := run(t, x, call("2", ToolTaskComplete, `{"summary":"root cause at a.go:3","pr_number":12}`)); err != nil {
		t.Fatal(err)
	}
	o, ok := x.Outcome()
	if !ok || o.Tool != ToolTaskComplete || o.Summary != "root cause at a.go:3" || o.PRNumber != 12 {
		t.Fatalf("outcome = %+v", o)
	}
	if _, err := run(t, x, call("3", ToolTaskAbort, `{"reason":"x"}`)); err == nil {
		t.Fatal("a second terminal call must be refused")
	}
}

func TestUnknownAndWriteToolsAreRefused(t *testing.T) {
	x, _ := newExec()
	for _, name := range []string{"pr.create", "repo.patch_files", "issue.comment", "search", "Bash"} {
		_, err := run(t, x, call(name, name, `{}`))
		if !errors.Is(err, ErrUnknownTool) || !strings.Contains(err.Error(), ToolRepoRead) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := run(t, x, call("j", ToolRepoRead, `not json`)); err == nil {
		t.Error("non-object arguments must error")
	}
}

func TestBound(t *testing.T) {
	s := strings.Repeat("é", MaxOutputChars) // 2 bytes per rune
	out := Bound(s)
	if !strings.Contains(out, "truncated") || len(out) > MaxOutputChars+120 {
		t.Fatalf("len=%d", len(out))
	}
	if strings.ContainsRune(out[:MaxOutputChars-2], '�') {
		t.Fatal("cut inside a rune")
	}
	if Bound("short") != "short" {
		t.Fatal("short output changed")
	}
}

func TestDefinitionsMatchToolapproveClasses(t *testing.T) {
	defs := Definitions(agentmode.ModeAdvisory)
	if len(defs) != len(ReadToolNames()) {
		t.Fatalf("defs=%d names=%d", len(defs), len(ReadToolNames()))
	}
	for _, d := range defs {
		if !toolapprove.IsReadOnly(d.Tool) {
			t.Errorf("%s is offered to ADVISORY agents but toolapprove does not classify it read-only", d.Tool)
		}
		if d.Content == "" || d.Arguments["type"] != "object" {
			t.Errorf("%s: incomplete definition %+v", d.Tool, d)
		}
	}
	fo := FinishOnly()
	if len(fo) != 2 || fo[0].Tool != ToolTaskComplete || fo[1].Tool != ToolTaskAbort {
		t.Fatalf("FinishOnly = %+v", fo)
	}
}

func TestParseFenced(t *testing.T) {
	msg := "I'll read it.\n```hive-tool\n{\"protocol\":\"hive-tool/v1\",\"kick_id\":\"k\",\"call_id\":\"c1\",\"seq\":1,\"tool\":\"repo.read_file\",\"args\":{\"path\":\"a.go\"}}\n```\n" +
		"```hive-tool\n{\"protocol\":\"hive-tool/v0\",\"tool\":\"x\"}\n```\n" +
		"```hive-tool\n{\"protocol\":\"hive-tool/v1\",\"tool\":\"task.complete\",\"args\":{\"summary\":\"done\"},\"extra\":1}\n```\n" +
		"```hive-tool\n{\"protocol\":\"hive-tool/v1\",\"tool\":\"issue.read\",\"args\":[1]}\n```\n" +
		"```hive-tool\n{\"protocol\":\"hive-tool/v1\",\"tool\":\"task.abort\"}\n```"
	calls, errs := ParseFenced(msg)
	if len(calls) != 2 || calls[0].ID != "c1" || calls[0].Name != ToolRepoRead || calls[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[1].ID != "fenced_4" || calls[1].Name != ToolTaskAbort || calls[1].Arguments != "{}" {
		t.Fatalf("second call = %+v", calls[1])
	}
	if len(errs) != 3 {
		t.Fatalf("errs = %v", errs)
	}
	if c, e := ParseFenced("no blocks here"); len(c) != 0 || len(e) != 0 {
		t.Fatal("plain prose must yield nothing")
	}
}

func TestRESTReader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/repos/o/r/issues/7":
			io.WriteString(w, `{"number":7,"title":"t","body":"b","state":"open","labels":[{"name":"kind/bug"}]}`)
		case r.URL.Path == "/repos/o/r/contents/src/pkg" && r.URL.Query().Get("ref") == "v6":
			io.WriteString(w, `[{"path":"src/pkg/a.go","type":"file"}]`)
		case r.URL.Path == "/repos/o/r/contents/src/pkg/a.go" && r.Header.Get("Accept") == "application/vnd.github.raw":
			io.WriteString(w, "package a\n")
		case r.URL.Path == "/repos/o/r/commits/v6/check-runs":
			io.WriteString(w, `{"check_runs":[{"name":"unit","status":"completed","conclusion":"success","output":{"title":"ok"}}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	r := &RESTReader{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}
	ctx := context.Background()
	if is, err := r.Issue(ctx, "o/r", 7); err != nil || is.Title != "t" || is.Labels[0] != "kind/bug" {
		t.Fatalf("issue = %+v, %v", is, err)
	}
	if es, err := r.List(ctx, "o/r", "v6", "src/pkg"); err != nil || len(es) != 1 || es[0].Path != "src/pkg/a.go" {
		t.Fatalf("list = %+v, %v", es, err)
	}
	if b, err := r.File(ctx, "o/r", "v6", "src/pkg/a.go"); err != nil || string(b) != "package a\n" {
		t.Fatalf("file = %q, %v", b, err)
	}
	if cs, err := r.Checks(ctx, "o/r", "v6"); err != nil || cs[0].Conclusion != "success" {
		t.Fatalf("checks = %+v, %v", cs, err)
	}
	if _, err := r.File(ctx, "o/r", "v6", "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing = %v", err)
	}
}

func TestCodeIndexSearcher(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init")
	for p, s := range map[string]string{"src/pkg/rebase.go": "package pkg\n\nfunc DependabotRecreate() {}\n", "docs/x.md": "DependabotRecreate docs\n"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "f")
	idx, errs := codeindex.Build(context.Background(), []codeindex.Repo{{Name: "o/r", Root: root}})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	s := CodeIndexSearcher{Index: func() *codeindex.Index { return idx }}
	hits, err := s.Search(context.Background(), "o/r", "", "DependabotRecreate", "src", 10)
	if err != nil || len(hits) != 1 || hits[0].Path != "src/pkg/rebase.go" || hits[0].Line != 3 {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	if hits, _ := s.Search(context.Background(), "other/repo", "", "DependabotRecreate", "", 10); len(hits) != 0 {
		t.Fatal("other repos must be filtered out")
	}
	if hits, err := (CodeIndexSearcher{}).Search(context.Background(), "o/r", "", "x", "", 10); hits != nil || err != nil {
		t.Fatal("nil index must be empty, not an error")
	}
}

func TestEvidenceGate(t *testing.T) {
	x, _ := newExec()
	x.RequireEvidence = true
	// Paraphrasing the issue without reading code is refused.
	_, err := run(t, x, call("1", ToolTaskComplete, `{"summary":"The App posts @dependabot rebase which is ignored; see src/pkg/a.go:3"}`))
	if err == nil || !strings.Contains(err.Error(), "cites no path:line") {
		t.Fatalf("ungrounded completion: %v", err)
	}
	if _, ok := x.Outcome(); ok {
		t.Fatal("refused completion must not record an outcome")
	}
	// A search hit counts as evidence; so does a read range (with slack).
	if _, err := run(t, x, call("2", ToolRepoSearch, `{"query":"Rebase"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, x, call("3", ToolRepoRead, `{"path":"src/pkg/big.go","start_line":149,"end_line":168}`)); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, x, call("4", ToolTaskComplete, `{"summary":"Root cause src/pkg/a.go:3 and src/pkg/big.go:147‑170; also README.md:1192 (not read)"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 of 3 citations verified") {
		t.Fatalf("out = %q", out)
	}
	o, _ := x.Outcome()
	if len(o.Citations) != 3 || !o.Citations[0].Verified || !o.Citations[1].Verified || o.Citations[2].Verified || o.Citations[2].Path != "README.md" {
		t.Fatalf("citations = %+v", o.Citations)
	}
	if o.Citations[1].EndLine != 170 {
		t.Fatalf("U+2011 range not parsed: %+v", o.Citations[1])
	}
}

func TestEvidenceOptional(t *testing.T) {
	x, _ := newExec()
	out, err := run(t, x, call("1", ToolTaskComplete, `{"summary":"no citations"}`))
	if err != nil || !strings.Contains(out, "0 of 0") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got := x.Citations("x ../etc/passwd:1 and src/a.go:0"); len(got) != 0 {
		t.Fatalf("escaping or zero-line citations must be ignored: %+v", got)
	}
}

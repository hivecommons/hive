package hivetool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/turn"
)

type errReader struct{ err error }

func (r errReader) Issue(context.Context, string, int) (Issue, error) { return Issue{}, r.err }
func (r errReader) List(context.Context, string, string, string) ([]Entry, error) {
	return nil, r.err
}
func (r errReader) File(context.Context, string, string, string) ([]byte, error) { return nil, r.err }
func (r errReader) Checks(context.Context, string, string) ([]CheckRun, error)   { return nil, r.err }

type errSearcher struct{}

func (errSearcher) Search(context.Context, string, string, string, string, int) ([]Hit, error) {
	return nil, errors.New("index down")
}

func TestReaderErrorsAndMissingArgs(t *testing.T) {
	x := &Executor{Repo: "o/r", Ref: "v6", Reader: errReader{errors.New("boom")}, Searcher: errSearcher{}}
	for _, c := range []turn.ToolCall{
		call("1", ToolIssueRead, `{"number":3}`),
		call("2", ToolIssueRead, `{}`),
		call("3", ToolRepoList, `{"path":"src"}`),
		call("4", ToolRepoList, `{"path":"../x"}`),
		call("5", ToolRepoRead, `{"path":"a.go"}`),
		call("6", ToolRepoRead, `{}`),
		call("7", ToolRepoSearch, `{"query":"x"}`),
		call("8", ToolRepoSearch, `{}`),
		call("9", ToolRepoSearch, `{"query":"x","path":"../y"}`),
		call("10", ToolChecksRead, `{"ref":"main"}`),
		call("11", ToolTaskAbort, `{}`),
	} {
		if _, err := run(t, x, c); err == nil {
			t.Errorf("%s %s: want error", c.Name, c.Arguments)
		}
	}
	if _, err := run(t, &Executor{Repo: "o/r", Reader: errReader{}}, call("s", ToolRepoSearch, `{"query":"x"}`)); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("nil searcher: %v", err)
	}
}

func TestListBoundsChecksAndAbort(t *testing.T) {
	x, r := newExec()
	for i := 0; i < MaxListEntries+5; i++ {
		r.files["many/f"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+"/"+string(rune('a'+i/26))] = ""
	}
	out, err := run(t, x, call("1", ToolRepoList, `{"path":"many"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, x, call("2", ToolRepoList, `{"path":"empty"}`)); err != nil {
		t.Fatal(err)
	}
	_ = out
	x.Reader = noChecks{r}
	if out, _ := run(t, x, call("3", ToolChecksRead, `{}`)); !strings.Contains(out, "no check runs") {
		t.Fatalf("checks = %q", out)
	}
	if out, err := run(t, x, call("4", ToolTaskAbort, `{"reason":"blocked on access"}`)); err != nil || !strings.Contains(out, "recorded") {
		t.Fatalf("abort = %q %v", out, err)
	}
	if o, ok := x.Outcome(); !ok || o.Tool != ToolTaskAbort || o.Reason != "blocked on access" {
		t.Fatalf("outcome = %+v", o)
	}
}

type noChecks struct{ *fakeReader }

func (noChecks) Checks(context.Context, string, string) ([]CheckRun, error) { return nil, nil }

func TestSearchTruncatesAndListTruncates(t *testing.T) {
	var hits []Hit
	for i := 0; i < MaxSearchResults+5; i++ {
		hits = append(hits, Hit{Path: "a.go", Line: i + 1, Text: "needle"})
	}
	x, r := newExec()
	x.Searcher = fakeSearcher{hits: hits}
	out, err := run(t, x, call("1", ToolRepoSearch, `{"query":"needle","path":"src"}`))
	if err != nil || !strings.Contains(out, "more matches") {
		t.Fatalf("search = %q, %v", out, err)
	}
	for i := 0; i < MaxListEntries+3; i++ {
		r.files["big/"+strings.Repeat("d", 1)+itoa(i)] = "x"
	}
	if out, _ := run(t, x, call("2", ToolRepoList, `{"path":"big"}`)); !strings.Contains(out, "more entries") {
		t.Fatalf("list not truncated: %d bytes", len(out))
	}
}

func itoa(i int) string {
	const d = "0123456789"
	if i < 10 {
		return string(d[i])
	}
	return itoa(i/10) + string(d[i%10])
}

func TestArgHelpers(t *testing.T) {
	a := map[string]any{"n": float64(4), "s": " 5 ", "bad": "x", "str": 3.0}
	if argInt(a, "missing", "n") != 4 || argInt(a, "s") != 5 || argInt(a, "bad") != 0 {
		t.Fatal("argInt")
	}
	if argString(a, "str") != "3" || argString(a, "missing") != "" {
		t.Fatal("argString")
	}
	if p, err := cleanPath("./src//pkg/"); err != nil || p != "src/pkg" {
		t.Fatalf("cleanPath = %q %v", p, err)
	}
	if p, _ := cleanPath("."); p != "" {
		t.Fatal("dot")
	}
	if inDir("") != "" || inDir("src") != " under src" {
		t.Fatal("inDir")
	}
}

func TestFencedLimits(t *testing.T) {
	var b strings.Builder
	for i := 0; i < MaxFencedCalls+2; i++ {
		b.WriteString("```hive-tool\n{\"protocol\":\"hive-tool/v1\",\"tool\":\"issue.read\",\"args\":{\"number\":1}}\n```\n")
	}
	b.WriteString("```hive-tool\n{\"protocol\":\"hive-tool/v1\"}\n```\n")
	calls, errs := ParseFenced(b.String())
	if len(calls) != MaxFencedCalls || len(errs) == 0 {
		t.Fatalf("calls=%d errs=%v", len(calls), errs)
	}
	if c, e := ParseFenced("```hive-tool\n{\"protocol\":\"hive-tool/v1\"}\n```"); len(c) != 0 || len(e) != 1 {
		t.Fatal("tool-less block must error")
	}
}

func TestRESTReaderErrorPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/1":
			io.WriteString(w, `{"number":1,"title":"t","state":"open","pull_request":{}}`)
		case "/repos/o/r/contents/file.go":
			if r.Header.Get("Accept") == "application/vnd.github.raw" {
				io.WriteString(w, "package x\n")
				return
			}
			io.WriteString(w, `{"type":"file"}`) // object, not a listing
		case "/repos/o/r/contents/dir":
			io.WriteString(w, `[{"path":"dir/a","type":"file"}]`)
		case "/repos/o/r/commits/main/check-runs":
			w.WriteHeader(500)
			io.WriteString(w, "oops\nmore")
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	r := &RESTReader{BaseURL: srv.URL}
	if is, err := r.Issue(ctx, "o/r", 1); err != nil || !strings.Contains(is.State, "pull request") {
		t.Fatalf("issue = %+v %v", is, err)
	}
	if _, err := r.List(ctx, "o/r", "main", "file.go"); err == nil || !strings.Contains(err.Error(), "is a file") {
		t.Fatalf("list of file: %v", err)
	}
	if _, err := r.File(ctx, "o/r", "main", "dir"); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("file of dir: %v", err)
	}
	if _, err := r.Checks(ctx, "o/r", "main"); err == nil || !strings.Contains(err.Error(), "500: oops") {
		t.Fatalf("checks: %v", err)
	}
	bad := &RESTReader{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "", errors.New("no token") }}
	if _, err := bad.Issue(ctx, "o/r", 1); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("token error: %v", err)
	}
	if _, err := (&RESTReader{BaseURL: "http://127.0.0.1:1"}).Issue(ctx, "o/r", 1); err == nil {
		t.Fatal("transport error expected")
	}
	if firstLine(strings.Repeat("x", 400)) != strings.Repeat("x", 300) {
		t.Fatal("firstLine")
	}
}

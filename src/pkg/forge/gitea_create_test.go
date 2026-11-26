package forge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// fakeForgejoWrites is a fake Forgejo API for the create paths. It records
// every request body by "METHOD path".
type fakeForgejoWrites struct {
	mu         sync.Mutex
	bodies     map[string][]map[string]any
	openPulls  string
	labelsFail bool
	createFail bool
}

func newFakeForgejoWrites() *fakeForgejoWrites {
	return &fakeForgejoWrites{bodies: map[string][]map[string]any{}, openPulls: `[]`}
}

func (f *fakeForgejoWrites) record(r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.bodies[key] = append(f.bodies[key], m)
}

// lastBody re-encodes the body record stored for r, which the handler has
// already consumed.
func (f *fakeForgejoWrites) lastBody(r *http.Request) []byte {
	calls := f.calls(r.Method + " " + r.URL.Path)
	if len(calls) == 0 {
		return nil
	}
	b, _ := json.Marshal(calls[len(calls)-1])
	return b
}

func (f *fakeForgejoWrites) calls(key string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func (f *fakeForgejoWrites) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token fj" {
			t.Errorf("%s %s: Authorization = %q", r.Method, r.URL.Path, got)
		}
		f.record(r)
		const repo = "/api/v1/repos/acme/widgets"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == repo:
			_, _ = w.Write([]byte(`{"full_name": "acme/widgets", "default_branch": "trunk"}`))
		case r.Method == http.MethodGet && r.URL.Path == repo+"/pulls":
			_, _ = w.Write([]byte(f.openPulls))
		case r.Method == http.MethodGet && r.URL.Path == repo+"/issues":
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(`[
					{"number": 4, "body": "no marker here", "html_url": "u4"},
					{"number": 5, "body": "x <!-- m:abc --> y", "html_url": "u5", "pull_request": {"merged": false}},
					{"number": 6, "body": "x <!-- m:abc --> y", "html_url": "u6"}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && (r.URL.Path == repo+"/issues" || r.URL.Path == repo+"/pulls"):
			if f.createFail {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"bad"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number": 42, "html_url": "https://fj.example/acme/widgets/x/42"}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, repo+"/issues/42/labels"):
			if f.labelsFail {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"label not found"}`))
				return
			}
			_, _ = w.Write(echoAppliedLabels(f.lastBody(r)))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestForgejo(t *testing.T, srv *httptest.Server) Forge {
	t.Helper()
	f, err := NewForge(KindForgejo, "fj", Options{BaseURL: srv.URL, Org: "acme"})
	if err != nil {
		t.Fatalf("NewForge: %v", err)
	}
	return f
}

func TestGiteaFamilyImplementsCreateSeams(t *testing.T) {
	for _, kind := range []Kind{KindGitea, KindForgejo} {
		f, err := NewForge(kind, "t", Options{BaseURL: "https://git.example"})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if _, ok := f.(IssueSeam); !ok {
			t.Errorf("%s adapter does not implement IssueSeam", kind)
		}
		if _, ok := f.(ChangeRequestCreator); !ok {
			t.Errorf("%s adapter does not implement ChangeRequestCreator", kind)
		}
	}
	// The GitHub adapter keeps opening pull requests through *github.Client.
	if _, ok := any(newGitHubForge("t", Options{})).(ChangeRequestCreator); ok {
		t.Error("GitHub adapter implements ChangeRequestCreator; the GitHub PR path must stay on *github.Client")
	}
}

func TestForgejoCreateIssueAddsLabelsByName(t *testing.T) {
	fake := newFakeForgejoWrites()
	f := newTestForgejo(t, fake.server(t))

	ref, err := f.(IssueCreator).CreateIssue(context.Background(), "widgets", "Title", "Body", []string{"kind/bug"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if ref.Number != 42 || ref.URL != "https://fj.example/acme/widgets/x/42" {
		t.Errorf("ref = %+v", ref)
	}
	creates := fake.calls("POST /api/v1/repos/acme/widgets/issues")
	if len(creates) != 1 || creates[0]["title"] != "Title" || creates[0]["body"] != "Body" {
		t.Fatalf("create bodies = %v", creates)
	}
	if _, hasLabels := creates[0]["labels"]; hasLabels {
		t.Error("create sent labels; the Forgejo create option takes label ids, not names")
	}
	labels := fake.calls("POST /api/v1/repos/acme/widgets/issues/42/labels")
	if len(labels) != 1 {
		t.Fatalf("label calls = %v", labels)
	}
	want := []any{"kind/bug", config.OriginLabel(config.OriginSourceForge)}
	if got := labels[0]["labels"]; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

func TestForgejoCreateIssueLabelFailureKeepsRef(t *testing.T) {
	fake := newFakeForgejoWrites()
	fake.labelsFail = true
	f := newTestForgejo(t, fake.server(t))

	ref, err := f.(IssueCreator).CreateIssue(context.Background(), "widgets", "Title", "", nil)
	if !errors.Is(err, ErrLabelsNotApplied) {
		t.Fatalf("err = %v, want ErrLabelsNotApplied", err)
	}
	if ref.Number != 42 {
		t.Errorf("ref = %+v, want the created issue so the caller does not file twice", ref)
	}
}

func TestForgejoCreateIssueReportsDroppedLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte(`[{"name":"bug"}]`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number": 42, "html_url": "u"}`))
		}
	}))
	t.Cleanup(srv.Close)
	f := newTestForgejo(t, srv)

	ref, err := f.(IssueCreator).CreateIssue(context.Background(), "widgets", "T", "", []string{"bug", "ready-for-review"})
	if !errors.Is(err, ErrLabelsNotApplied) {
		t.Fatalf("err = %v, want ErrLabelsNotApplied", err)
	}
	if !strings.Contains(err.Error(), "ready-for-review") || strings.Contains(err.Error(), "bug,") {
		t.Errorf("err = %v, want it to name only the dropped label", err)
	}
	if ref.Number != 42 {
		t.Errorf("ref = %+v", ref)
	}
}

func TestForgejoCreateIssueRefusals(t *testing.T) {
	fake := newFakeForgejoWrites()
	fake.createFail = true
	f := newTestForgejo(t, fake.server(t))
	c := f.(IssueCreator)
	if _, err := c.CreateIssue(context.Background(), "widgets", "  ", "", nil); err == nil {
		t.Error("empty title accepted")
	}
	if _, err := c.CreateIssue(context.Background(), "widgets", "T", "", nil); err == nil || !strings.Contains(err.Error(), "status 422") {
		t.Errorf("err = %v, want the forge's 422", err)
	}
}

func TestForgejoFindIssueByMarker(t *testing.T) {
	fake := newFakeForgejoWrites()
	f := newTestForgejo(t, fake.server(t)).(IssueFinder)

	ref, ok, err := f.FindIssueByMarker(context.Background(), "widgets", "<!-- m:abc -->")
	if err != nil || !ok || ref.Number != 6 || ref.URL != "u6" {
		t.Fatalf("got %+v, %v, %v; want #6 (the PR stub #5 skipped)", ref, ok, err)
	}
	if _, ok, err := f.FindIssueByMarker(context.Background(), "widgets", "<!-- none -->"); ok || err != nil {
		t.Errorf("missing marker: ok=%v err=%v", ok, err)
	}
	if _, _, err := f.FindIssueByMarker(context.Background(), "widgets", ""); err == nil {
		t.Error("empty marker accepted")
	}
}

func TestForgejoCreateChangeRequest(t *testing.T) {
	fake := newFakeForgejoWrites()
	f := newTestForgejo(t, fake.server(t)).(ChangeRequestCreator)

	ref, err := f.CreateChangeRequest(context.Background(), "widgets", ChangeRequestSpec{
		Head: "fix-1", Title: "fix: one", Body: "Fixes #1", Labels: []string{"hold"},
	})
	if err != nil {
		t.Fatalf("CreateChangeRequest: %v", err)
	}
	if ref.Number != 42 || ref.AlreadyExisted {
		t.Errorf("ref = %+v", ref)
	}
	creates := fake.calls("POST /api/v1/repos/acme/widgets/pulls")
	if len(creates) != 1 || creates[0]["head"] != "fix-1" || creates[0]["base"] != "trunk" || creates[0]["body"] != "Fixes #1" {
		t.Fatalf("create bodies = %v, want base resolved to the default branch", creates)
	}
	if labels := fake.calls("POST /api/v1/repos/acme/widgets/issues/42/labels"); len(labels) != 1 {
		t.Errorf("label calls = %v, want one", labels)
	}
}

func TestForgejoCreateChangeRequestReusesOpenHead(t *testing.T) {
	fake := newFakeForgejoWrites()
	fake.openPulls = `[{"number": 9, "html_url": "u9", "head": {"ref": "fix-1"}, "base": {"ref": "trunk"}}]`
	f := newTestForgejo(t, fake.server(t)).(ChangeRequestCreator)

	ref, err := f.CreateChangeRequest(context.Background(), "widgets", ChangeRequestSpec{Head: "fix-1", Title: "t"})
	if err != nil {
		t.Fatalf("CreateChangeRequest: %v", err)
	}
	if ref.Number != 9 || !ref.AlreadyExisted {
		t.Errorf("ref = %+v, want the open #9", ref)
	}
	if creates := fake.calls("POST /api/v1/repos/acme/widgets/pulls"); len(creates) != 0 {
		t.Errorf("opened a second pull request: %v", creates)
	}
}

func TestForgejoCreateChangeRequestRefusals(t *testing.T) {
	fake := newFakeForgejoWrites()
	fake.createFail = true
	f := newTestForgejo(t, fake.server(t)).(ChangeRequestCreator)
	ctx := context.Background()
	if _, err := f.CreateChangeRequest(ctx, "widgets", ChangeRequestSpec{Title: "t"}); err == nil {
		t.Error("missing head accepted")
	}
	if _, err := f.CreateChangeRequest(ctx, "widgets", ChangeRequestSpec{Head: "h", Base: "trunk", Title: "t"}); err == nil || !strings.Contains(err.Error(), "status 422") {
		t.Errorf("err = %v, want the forge's 422", err)
	}
}

package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
	"golang.org/x/net/html"
)

const webTreeRepo = "github/github.com/org/repo"

func webTreeFixture(t *testing.T) (*httptest.Server, *hivectl.ProfileStore, *hivectl.StagedEdits, *hivectl.ProfileStore) {
	t.Helper()
	set := twoHives()
	set.Profiles[0].Hub += "?token=endpoint-secret"
	for i := range set.Profiles {
		set.Profiles[i].Selection = &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected, Repositories: []string{webTreeRepo}, FollowNewRepositories: true}
	}
	set.Hubs = []hivectl.HubEntry{{Name: "one", URL: "https://one.example", Follow: hivectl.HubFollowCurrent}, {Name: "two", URL: "https://two.example"}}
	hubs := []hivectl.TreeHubSnapshot{
		{Name: "one", URL: "https://one.example", Discovery: hivectl.TreeComplete, HiveOrigins: []string{"https://acme.example", "https://other.example"}},
		{Name: "two", URL: "https://two.example", Discovery: hivectl.TreeComplete, HiveOrigins: []string{"https://acme.example"}},
	}
	repos := map[string]hivectl.TreeRepositorySnapshot{
		"acme":  {Discovery: hivectl.TreeComplete, Repositories: []string{webTreeRepo}},
		"other": {Discovery: hivectl.TreeComplete, Repositories: []string{webTreeRepo}},
	}
	store := hivectl.NewProfileStore(t.TempDir())
	expectedStore := hivectl.NewProfileStore(t.TempDir())
	for _, target := range []*hivectl.ProfileStore{store, expectedStore} {
		if err := target.Save(set); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := hivectl.NewStagedEdits(set, hubs, repos)
	if err != nil {
		t.Fatal(err)
	}
	handler := newCommonsWebServer(&hivesDeps{store: store}, "secret").(*commonsWebServer)
	handler.discover = func(context.Context, *hivectl.ProfileSet) ([]hivectl.TreeHubSnapshot, map[string]hivectl.TreeRepositorySnapshot) {
		return hubs, repos
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, store, expected, expectedStore
}

func treeCall(t *testing.T, server *httptest.Server, path string, previous *commonsTreeResponse, edit commonsTreeEdit, want int) commonsTreeResponse {
	t.Helper()
	method := http.MethodGet
	var body any
	if previous != nil {
		method = http.MethodPost
		edit.Session, edit.Version = previous.Session, previous.Version
		body = edit
	}
	req := commonsReq(t, server, method, path, body)
	if previous != nil {
		req.Header.Set("If-Match", previous.Revision)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s status=%d want=%d body=%s", path, resp.StatusCode, want, raw)
	}
	for _, secret := range []string{"tok-acme", "tok-other", "endpoint-secret", "provider-secret", "ghp-secret"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("%s exposed credential %q", path, secret)
		}
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("missing hardening headers: %v", resp.Header)
	}
	var result commonsTreeResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode %s: %v (%s)", path, err, raw)
	}
	return result
}

func treeFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCommonsTreeSharedDiffAndReceipt(t *testing.T) {
	server, store, expected, expectedStore := webTreeFixture(t)
	before := treeFile(t, store.Path())
	state := treeCall(t, server, "/api/tree?discover=1", nil, commonsTreeEdit{}, http.StatusOK)
	if state.Revision == "" || state.Session == "" {
		t.Fatal("missing reviewed revision/session")
	}
	actions := []struct {
		edit   commonsTreeEdit
		shared func() error
	}{
		{commonsTreeEdit{Operation: "current", Target: "acme", Value: false}, func() error { return expected.SelectCurrent("acme", false, false) }},
		{commonsTreeEdit{Operation: "current", Target: "acme", Value: true}, func() error { return expected.SelectCurrent("acme", false, true) }},
		{commonsTreeEdit{Operation: "exclude", Target: "acme", Repository: webTreeRepo, Value: true}, func() error { return expected.SetExcluded("acme", webTreeRepo, true) }},
		{commonsTreeEdit{Operation: "follow", Target: "acme", Value: false}, func() error { return expected.SetFollow("acme", false) }},
		{commonsTreeEdit{Operation: "enabled", Target: "https://one.example", Hub: true, Value: false}, func() error { return expected.SetHubEnabled("https://one.example", false) }},
		{commonsTreeEdit{Operation: "repository", Target: "other", Repository: "github/github.com/org/new", Value: true}, func() error { return expected.SelectRepository("other", "github/github.com/org/new", true) }},
	}
	for _, action := range actions {
		if err := action.shared(); err != nil {
			t.Fatal(err)
		}
		state = treeCall(t, server, "/api/tree/diff", &state, action.edit, http.StatusOK)
		actualJSON, _ := json.Marshal(state.Diff)
		expectedJSON, _ := json.Marshal(expected.Diff())
		if !bytes.Equal(actualJSON, expectedJSON) {
			t.Fatalf("Web differs from shared diff:\n%+v\n%+v", state.Diff, expected.Diff())
		}
		tree, err := expected.Tree()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(state.Tree, tree) {
			t.Fatal("Web tree differs from shared tree")
		}
		if !bytes.Equal(before, treeFile(t, store.Path())) {
			t.Fatal("staging wrote profiles")
		}
		if _, err := os.Stat(store.EnvPath()); !os.IsNotExist(err) {
			t.Fatal("staging wrote projection")
		}
	}
	if len(state.Diff.SharedBranches) == 0 || len(state.Diff.Exclusions) == 0 || len(state.Diff.HubFollow) == 0 {
		t.Fatal("shared side effects missing from review")
	}
	receipt, err := expected.Apply(context.Background(), expectedStore)
	if err != nil {
		t.Fatal(err)
	}
	applied := treeCall(t, server, "/api/tree/apply", &state, commonsTreeEdit{}, http.StatusOK)
	if applied.Receipt == nil || *applied.Receipt != receipt || !applied.Complete {
		t.Fatalf("receipt=%+v want=%+v", applied.Receipt, receipt)
	}
	for _, paths := range [][2]string{{store.Path(), expectedStore.Path()}, {store.EnvPath(), expectedStore.EnvPath()}} {
		if !bytes.Equal(treeFile(t, paths[0]), treeFile(t, paths[1])) {
			t.Fatalf("Web and shared Apply differ for %s", paths[0])
		}
	}
}

func TestCommonsTreeDiscardAndIndependentTabs(t *testing.T) {
	server, store, _, _ := webTreeFixture(t)
	before := treeFile(t, store.Path())
	first := treeCall(t, server, "/api/tree", nil, commonsTreeEdit{}, 200)
	second := treeCall(t, server, "/api/tree", nil, commonsTreeEdit{}, 200)
	first = treeCall(t, server, "/api/tree/diff", &first, commonsTreeEdit{Operation: "enabled", Target: "acme", Value: false}, 200)
	second = treeCall(t, server, "/api/tree?session="+second.Session, nil, commonsTreeEdit{}, 200)
	if !second.Diff.Empty() {
		t.Fatal("another tab acquired staged edits")
	}
	refreshed := treeCall(t, server, "/api/tree?session="+first.Session, nil, commonsTreeEdit{}, 200)
	if !reflect.DeepEqual(first.Diff, refreshed.Diff) {
		t.Fatal("refresh lost edits")
	}
	treeCall(t, server, "/api/tree/discard", &first, commonsTreeEdit{}, 200)
	if !bytes.Equal(before, treeFile(t, store.Path())) {
		t.Fatal("Discard wrote consent")
	}
	applied := treeCall(t, server, "/api/tree/apply", &second, commonsTreeEdit{}, 200)
	if applied.Receipt.Saved != hivectl.StageUnchanged || !bytes.Equal(before, treeFile(t, store.Path())) {
		t.Fatal("empty Apply wrote consent")
	}
	if _, err := os.Stat(store.EnvPath()); !os.IsNotExist(err) {
		t.Fatal("empty Apply wrote projection")
	}
}

func TestCommonsTreeStaleRevisionRequiresRenewedReview(t *testing.T) {
	for _, endpoint := range []string{"/api/tree/diff", "/api/tree/apply"} {
		t.Run(endpoint, func(t *testing.T) {
			server, store, _, _ := webTreeFixture(t)
			state := treeCall(t, server, "/api/tree", nil, commonsTreeEdit{}, 200)
			state = treeCall(t, server, "/api/tree/diff", &state, commonsTreeEdit{Operation: "enabled", Target: "acme", Value: false}, 200)
			changed, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if err := changed.SetCommonsStrategy(hivectl.CommonsStrategySpread); err != nil {
				t.Fatal(err)
			}
			if err := store.CommitExpecting(changed, changed.Revision()); err != nil {
				t.Fatal(err)
			}
			before := treeFile(t, store.Path())
			stale := treeCall(t, server, endpoint, &state, commonsTreeEdit{Operation: "follow", Target: "acme", Value: false}, 409)
			if !stale.Diff.Stale || !strings.Contains(stale.Error, "review again") {
				t.Fatalf("missing stale review: %+v", stale)
			}
			refreshed := treeCall(t, server, "/api/tree?session="+state.Session, nil, commonsTreeEdit{}, 200)
			if !refreshed.Diff.Stale {
				t.Fatal("refresh made stale review fresh")
			}
			if !bytes.Equal(before, treeFile(t, store.Path())) {
				t.Fatal("stale review overwrote consent")
			}
		})
	}
}

func TestCommonsTreeRejectsUnreviewedRequestsAndBrowserOrigins(t *testing.T) {
	server, store, _, _ := webTreeFixture(t)
	state := treeCall(t, server, "/api/tree", nil, commonsTreeEdit{}, 200)
	before := treeFile(t, store.Path())
	for _, path := range []string{"/api/tree", "/api/tree/diff", "/api/tree/apply", "/api/tree/discard"} {
		for _, attack := range []string{"host", "port", "origin", "fetch-site", "token"} {
			method := http.MethodPost
			if path == "/api/tree" {
				method = http.MethodGet
			}
			req := commonsReq(t, server, method, path, commonsTreeEdit{Session: state.Session, Version: state.Version, Operation: "enabled", Target: "acme", Value: false})
			req.Header.Set("If-Match", state.Revision)
			want := http.StatusForbidden
			switch attack {
			case "host":
				req.Host = "evil.example:80"
			case "port":
				req.Host = "localhost:1"
			case "origin":
				req.Header.Set("Origin", "https://evil.example")
			case "fetch-site":
				req.Header.Set("Sec-Fetch-Site", "cross-site")
			case "token":
				req.Header.Del(commonsWebAuthHeader)
				want = http.StatusUnauthorized
			}
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != want {
				t.Fatalf("%s/%s status=%d", path, attack, response.StatusCode)
			}
		}
	}
	for _, missing := range []bool{true, false} {
		req := commonsReq(t, server, http.MethodPost, "/api/tree/diff", commonsTreeEdit{Session: state.Session, Version: state.Version + 1, Operation: "enabled", Target: "acme"})
		if !missing {
			req.Header.Set("If-Match", state.Revision)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("unreviewed request status=%d", resp.StatusCode)
		}
	}
	if !bytes.Equal(before, treeFile(t, store.Path())) {
		t.Fatal("rejected request wrote consent")
	}
}

func TestCommonsTreeLegacyAndIncompleteDiscovery(t *testing.T) {
	server, store, _, _ := webTreeFixture(t)
	set, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	set.Profiles[0].Selection = nil
	if err := store.Save(set); err != nil {
		t.Fatal(err)
	}
	before := treeFile(t, store.Path())
	state := treeCall(t, server, "/api/tree", nil, commonsTreeEdit{}, 200)
	if !state.Tree.Subscriptions[0].Selection.Legacy {
		t.Fatal("legacy warning missing")
	}
	for _, edit := range []commonsTreeEdit{{Operation: "current", Target: "acme", Value: true}, {Operation: "restrict", Target: "acme"}, {Operation: "current", Target: "other", Value: true}} {
		failed := treeCall(t, server, "/api/tree/diff", &state, edit, 400)
		if !failed.Diff.Empty() {
			t.Fatal("incomplete bulk inclusion staged consent")
		}
	}
	state = treeCall(t, server, "/api/tree/diff", &state, commonsTreeEdit{Operation: "enabled", Target: "acme", Value: false}, 200)
	if !state.Tree.Subscriptions[0].Selection.Legacy {
		t.Fatal("disable converted legacy")
	}
	if !bytes.Equal(before, treeFile(t, store.Path())) {
		t.Fatal("legacy review mutated profiles")
	}
	treeCall(t, server, "/api/tree/discard", &state, commonsTreeEdit{}, 200)
	state = treeCall(t, server, "/api/tree?discover=1", nil, commonsTreeEdit{}, 200)
	state = treeCall(t, server, "/api/tree/diff", &state, commonsTreeEdit{Operation: "restrict", Target: "acme"}, 200)
	if len(state.Diff.LegacyConversions) != 1 || state.Tree.Subscriptions[0].Selection.Legacy {
		t.Fatal("explicit restriction missing")
	}
}

func TestCommonsTreeProjectionFailureRetryAndReceipt(t *testing.T) {
	server, store, _, _ := webTreeFixture(t)
	state := treeCall(t, server, "/api/tree", nil, commonsTreeEdit{}, 200)
	state = treeCall(t, server, "/api/tree/diff", &state, commonsTreeEdit{Operation: "enabled", Target: "acme", Value: false}, 200)
	if err := os.Mkdir(store.EnvPath(), 0700); err != nil {
		t.Fatal(err)
	}
	failed := treeCall(t, server, "/api/tree/apply", &state, commonsTreeEdit{}, 400)
	if failed.Receipt.Saved != hivectl.StageProfilesSaved || failed.Complete || !failed.ProjectionPending || failed.Receipt.Acknowledged != "unknown" {
		t.Fatalf("false success: %+v", failed)
	}
	treeCall(t, server, "/api/tree/discard", &failed, commonsTreeEdit{}, 400)
	if err := os.Remove(store.EnvPath()); err != nil {
		t.Fatal(err)
	}
	// A malformed relay record proves a reload failure is distinct from a save,
	// while its credential-like contents must not appear in the receipt/error.
	if err := os.WriteFile(store.RelayPIDPath(), []byte("provider-secret ghp-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	applied := treeCall(t, server, "/api/tree/apply", &failed, commonsTreeEdit{}, 400)
	if applied.Receipt.Saved != hivectl.StageSaved || applied.Receipt.RelayReload != hivectl.StageReloadFailed || applied.Receipt.Acknowledged != "pending" || !applied.Complete {
		t.Fatalf("untruthful retry receipt: %+v", applied)
	}
}

func TestCommonsTreeOpenDoesNotMigrateLegacyEnv(t *testing.T) {
	server, store, _ := newCommonsWebHarness(t, nil)
	defer server.Close()
	env := []byte("HIVE_HUB=wss://legacy.example/contribute\nHIVE_REGISTRATION_TOKEN=tok-acme\nGH_TOKEN=ghp-secret\nPROVIDER_API_KEY=provider-secret\n")
	if err := os.WriteFile(store.EnvPath(), env, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/hives", "/api/tree"} {
		resp, err := server.Client().Do(commonsReq(t, server, http.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if path == "/api/tree" && !bytes.Contains(raw, []byte(`"legacy_unrestricted":true`)) {
			t.Fatal("legacy subscription not shown")
		}
		if resp.StatusCode != 200 || bytes.Contains(raw, []byte("tok-acme")) || bytes.Contains(raw, []byte("ghp-secret")) || bytes.Contains(raw, []byte("provider-secret")) {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, raw)
		}
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatal("opening UI migrated legacy env")
	}
	if !bytes.Equal(env, treeFile(t, store.EnvPath())) {
		t.Fatal("opening UI rewrote legacy env")
	}
}

func TestCommonsTreeMalformedDataDoesNotEchoSecrets(t *testing.T) {
	server, store, _, _ := webTreeFixture(t)
	state := treeCall(t, server, "/api/tree", nil, commonsTreeEdit{}, 200)
	req := commonsReq(t, server, http.MethodPost, "/api/tree/diff", map[string]string{"provider-secret": "ghp-secret"})
	req.Header.Set("If-Match", state.Revision)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 400 || bytes.Contains(raw, []byte("secret")) {
		t.Fatalf("unsafe decode response: %s", raw)
	}
	if err := os.WriteFile(store.Path(), []byte("version: [provider-secret ghp-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/tree", "/api/hives"} {
		response, err = server.Client().Do(commonsReq(t, server, http.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 400 || bytes.Contains(raw, []byte("secret")) {
			t.Fatalf("unsafe profile response: %s", raw)
		}
	}
}

func TestCommonsTreePageAccessibilityAndLocalAssets(t *testing.T) {
	var page bytes.Buffer
	if err := commonsWebTemplate.Execute(&page, map[string]string{"Token": "local-token"}); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	for _, want := range []string{`role="tree"`, `aria-live="polite"`, `aria-atomic="true"`, `'treeitem'`, `'aria-expanded'`, `'aria-checked'`, `'mixed'`, `'ArrowLeft'`, `'ArrowRight'`, `'ArrowDown'`, `'ArrowUp'`, `'Tab'`, `dialog.showModal()`, `previous.focus()`, `'If-Match':review.revision`, `data.receipt.saved`, `data.receipt.relay_reload`, `data.receipt.acknowledged`} {
		if !strings.Contains(html, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, bad := range []string{"window.confirm(", "window.alert(", "window.prompt(", "addEventListener('beforeunload'", "<script src=", "<link ", ".innerHTML ="} {
		// The older profile table uses static, non-metadata HTML; the tree never does.
		source := html
		if bad == ".innerHTML =" {
			source = commonsWebTreeJS
		}
		if strings.Contains(source, bad) {
			t.Errorf("page contains forbidden %q", bad)
		}
	}
}

// The page script sends this literal as the X-Hive-Commons-Token header. The
// template is html/template, whose JS context already quotes a string; piping
// the token through printf "%q" first double-quotes it, the header then
// carries literal quote characters, and every API call is rejected with 401.
func TestCommonsPageEmbedsTokenAsSingleJSStringLiteral(t *testing.T) {
	for _, token := range []string{"local-token", `a"b</script>`} {
		var page bytes.Buffer
		if err := commonsWebTemplate.Execute(&page, map[string]string{"Token": token}); err != nil {
			t.Fatal(err)
		}
		line := ""
		for _, l := range strings.Split(page.String(), "\n") {
			if strings.HasPrefix(l, "const token = ") {
				line = l
			}
		}
		if line == "" {
			t.Fatal("page has no `const token = ` line")
		}
		literal := strings.TrimSuffix(strings.TrimPrefix(line, "const token = "), ";")
		var got string
		if err := json.Unmarshal([]byte(literal), &got); err != nil || got != token {
			t.Errorf("token literal %s decodes to %q (err %v), want exactly %q", literal, got, err, token)
		}
	}
}

// "Manage Hives" is a <details>. Its <summary> is natively focusable, so a
// negative tabindex takes it out of the Tab order and leaves keyboard users
// with only the skip link. Parse the rendered page and check the element.
func TestCommonsPageManageDisclosureIsReachableByTab(t *testing.T) {
	var page bytes.Buffer
	if err := commonsWebTemplate.Execute(&page, map[string]string{"Token": "local-token"}); err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(&page)
	if err != nil {
		t.Fatal(err)
	}
	var find func(*html.Node) *html.Node
	find = func(n *html.Node) *html.Node {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "id" && a.Val == "profiles-title" {
					return n
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if f := find(c); f != nil {
				return f
			}
		}
		return nil
	}
	node := find(doc)
	if node == nil || node.Data != "summary" || node.Parent == nil || node.Parent.Data != "details" {
		t.Fatalf("#profiles-title must be the <summary> of a <details>, got %+v", node)
	}
	for _, a := range node.Attr {
		if a.Key != "tabindex" {
			continue
		}
		if n, err := strconv.Atoi(a.Val); err != nil || n < 0 {
			t.Errorf("summary has tabindex=%q, which removes it from the Tab order", a.Val)
		}
	}
}

func TestCommonsTreePublicDiscoveryAndOutage(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "outage"}[unavailable], func(t *testing.T) {
			var remote *httptest.Server
			remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get(commonsWebAuthHeader) != "" || r.URL.RawQuery != "" {
					t.Error("discovery sent credentials")
				}
				if unavailable {
					http.Error(w, "provider-secret", http.StatusServiceUnavailable)
					return
				}
				switch r.URL.Path {
				case "/api/hives":
					writeCommonsJSON(w, map[string]any{"hives": []map[string]string{{"id": "publisher-id", "hub_url": remote.URL + "/contribute", "project_name": "<img src=x onerror=alert(1)>"}}})
				case "/api/contribute/repositories":
					writeCommonsJSON(w, map[string]any{"repositories": []map[string]string{{"id": webTreeRepo}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer remote.Close()
			set := twoHives()
			set.Profiles = set.Profiles[:1]
			set.Profiles[0].Hub = remote.URL + "/contribute?token=endpoint-secret"
			set.Profiles[0].Selection = &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected, Repositories: []string{webTreeRepo}}
			origin, err := hivectl.NormalizeHiveID(remote.URL)
			if err != nil {
				t.Fatal(err)
			}
			set.Hubs = []hivectl.HubEntry{{Name: "publisher", URL: remote.URL, Reviewed: []string{origin}}}
			hubs, repos := discoverCommonsTree(context.Background(), set)
			want := hivectl.TreeComplete
			if unavailable {
				want = hivectl.TreeUnavailable
			}
			if len(hubs) != 1 || hubs[0].Discovery != want || len(hubs[0].HiveOrigins) != 1 || repos["acme"].Discovery != want {
				t.Fatalf("discovery: %+v %+v", hubs, repos)
			}
			tree, err := hivectl.BuildContributionTree(set, hubs, repos)
			if err != nil {
				t.Fatal(err)
			}
			if tree.Subscriptions[0].Selection.Selected != 1 {
				t.Fatal("discovery lost saved consent")
			}
			raw, _ := json.Marshal(tree)
			if bytes.Contains(raw, []byte("endpoint-secret")) || bytes.Contains(raw, []byte("<img")) {
				t.Fatal("publisher metadata/credentials escaped the read model")
			}
		})
	}
}

// Opening the page (discover=1) stages nothing; only the explicit accept=1
// request stages the new repositories of followed Hives, and neither saves.
func TestCommonsTreeAcceptStagesNewRepositoriesOnlyOnRequest(t *testing.T) {
	const added = "github/github.com/org/added"
	set := twoHives()
	for i := range set.Profiles {
		set.Profiles[i].Selection = &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected, Repositories: []string{webTreeRepo}, FollowNewRepositories: i == 0}
	}
	repos := map[string]hivectl.TreeRepositorySnapshot{
		"acme":  {Discovery: hivectl.TreeComplete, Repositories: []string{webTreeRepo, added}},
		"other": {Discovery: hivectl.TreeComplete, Repositories: []string{webTreeRepo, added}},
	}
	store := hivectl.NewProfileStore(t.TempDir())
	if err := store.Save(set); err != nil {
		t.Fatal(err)
	}
	handler := newCommonsWebServer(&hivesDeps{store: store}, "secret").(*commonsWebServer)
	handler.discover = func(context.Context, *hivectl.ProfileSet) ([]hivectl.TreeHubSnapshot, map[string]hivectl.TreeRepositorySnapshot) {
		return nil, repos
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	before := treeFile(t, store.Path())

	opened := treeCall(t, server, "/api/tree?discover=1", nil, commonsTreeEdit{}, http.StatusOK)
	if len(opened.Diff.Auto) != 0 || !opened.Diff.Empty() {
		t.Fatalf("opening the page staged %+v", opened.Diff)
	}
	accepted := treeCall(t, server, "/api/tree?discover=1&accept=1", nil, commonsTreeEdit{}, http.StatusOK)
	if len(accepted.Diff.Auto) != 1 || accepted.Diff.Auto[0].Subscription != "acme" || accepted.Diff.Auto[0].Repository != added {
		t.Fatalf("accept=1 staged %+v, want only %s on the followed Hive acme", accepted.Diff.Auto, added)
	}
	if !bytes.Equal(treeFile(t, store.Path()), before) {
		t.Fatal("staging new repositories wrote to the store")
	}
}

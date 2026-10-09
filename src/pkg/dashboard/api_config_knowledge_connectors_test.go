package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

type fakeConnectorRuntime struct {
	mu       sync.Mutex
	statuses []connector.Status
	synced   chan string
}

func (f *fakeConnectorRuntime) Statuses() []connector.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]connector.Status(nil), f.statuses...)
}

func (f *fakeConnectorRuntime) SyncNow(_ context.Context, name string) (connector.Status, error) {
	f.synced <- name
	return connector.Status{Name: name}, nil
}

const kcGoodPut = `{"connectors":[{"name":"docs","type":"document","enabled":true,"layer":"project","scope":{"url":"https://example.org/guide.html"},"auth":{"env":"DOCS_TOKEN"}}]}`

func TestKnowledgeConnectors_PutThenGet(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutRaw(s, "/api/config/knowledge/connectors", kcGoodPut); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%q", rec.Code, rec.Body.String())
	}
	rec := doOwnerGet(s, "/api/config/knowledge/connectors")
	var got struct {
		Connectors []knowledgeConnectorJSON `json:"connectors"`
		Types      []string                 `json:"types"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Connectors) != 1 || got.Connectors[0].Name != "docs" || got.Connectors[0].Auth.Env != "DOCS_TOKEN" || !got.Connectors[0].Enabled {
		t.Fatalf("connectors = %+v", got.Connectors)
	}
	if len(got.Types) == 0 {
		t.Fatal("types must list the registered connector types")
	}
}

func TestKnowledgeConnectors_PutRequiresOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutNoRole(s, "/api/config/knowledge/connectors", kcGoodPut); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT = %d, want 403", rec.Code)
	}
	if rec := doGetNoRole(s, "/api/config/knowledge/connectors"); rec.Code != http.StatusOK {
		t.Fatalf("non-owner GET = %d, want 200 (read-only table)", rec.Code)
	}
}

func TestKnowledgeConnectors_PutRejectsInvalid(t *testing.T) {
	s := covApiServer(t)
	cases := map[string]string{
		"inline secret":   `{"connectors":[{"name":"docs","type":"document","layer":"project","scope":{"url":"https://example.org/a"},"auth":{"token":"hunter2"}}]}`,
		"secret in scope": `{"connectors":[{"name":"docs","type":"document","layer":"project","scope":{"api_key":"x","url":"https://example.org/a"}}]}`,
		"bad layer":       `{"connectors":[{"name":"docs","type":"document","layer":"nope","scope":{"url":"https://example.org/a"}}]}`,
		"unknown type":    `{"connectors":[{"name":"docs","type":"wat","layer":"project"}]}`,
		"duplicate":       `{"connectors":[{"name":"a","type":"document","layer":"project","scope":{"url":"https://example.org/a"}},{"name":"a","type":"document","layer":"project","scope":{"url":"https://example.org/b"}}]}`,
		"env and file":    `{"connectors":[{"name":"docs","type":"document","layer":"project","scope":{"url":"https://example.org/a"},"auth":{"env":"X","file":"/run/x"}}]}`,
		"missing list":    `{}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			rec := doPutRaw(s, "/api/config/knowledge/connectors", raw)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d body=%q, want 400", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Fatal("error must not echo a pasted secret")
			}
		})
	}
	if n := len(s.deps.Config.Knowledge.Connectors); n != 0 {
		t.Fatalf("rejected PUTs must not mutate config, have %d connectors", n)
	}
}

func TestKnowledgeConnectors_Validate(t *testing.T) {
	s := covApiServer(t)
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := `{"name":"docs","type":"document","enabled":true,"layer":"project","scope":{"url":"https://example.org/a"},"auth":{"file":"` + secret + `"}}`
	rec := doPostRaw(s, "/api/config/knowledge/connectors/validate", ok)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("validate good = %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatal("validate must not echo the credential")
	}

	t.Setenv("KC_UNSET_TOKEN", "")
	bad := `{"name":"docs","type":"document","enabled":true,"layer":"project","scope":{"url":"https://example.org/a"},"auth":{"env":"KC_UNSET_TOKEN"}}`
	rec = doPostRaw(s, "/api/config/knowledge/connectors/validate", bad)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("validate unset env = %d %q, want ok:false", rec.Code, rec.Body.String())
	}
}

func TestKnowledgeConnectors_StatusAndSync(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutRaw(s, "/api/config/knowledge/connectors", `{"connectors":[
		{"name":"docs","type":"document","enabled":true,"layer":"project","scope":{"url":"https://example.org/a"}},
		{"name":"off","type":"document","enabled":false,"layer":"org","scope":{"url":"https://example.org/b"}}]}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %q", rec.Code, rec.Body.String())
	}

	statusOf := func() map[string]knowledgeConnectorStatusRow {
		rec := doGetNoRole(s, "/api/config/knowledge/connectors/status")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var resp struct {
			Connectors []knowledgeConnectorStatusRow `json:"connectors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		out := map[string]knowledgeConnectorStatusRow{}
		for _, r := range resp.Connectors {
			out[r.Name] = r
		}
		return out
	}

	rows := statusOf()
	if rows["docs"].Status != "pending" || rows["off"].Status != "disabled" {
		t.Fatalf("no runtime: %+v", rows)
	}
	if rec := doPostRaw(s, "/api/config/knowledge/connectors/docs/sync", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("sync without runtime = %d, want 503", rec.Code)
	}

	rt := &fakeConnectorRuntime{synced: make(chan string, 1), statuses: []connector.Status{
		{Name: "docs", LastSync: time.Now(), Pages: 3, Facts: 7},
		{Name: "off", LastError: "boom"},
	}}
	s.deps.KnowledgeConnectors = rt
	rows = statusOf()
	if rows["docs"].Status != "ok" || rows["docs"].Pages != 3 || rows["docs"].Facts != 7 || rows["docs"].LastSync == "" {
		t.Fatalf("docs row = %+v", rows["docs"])
	}
	if rows["off"].Status != "disabled" {
		t.Fatalf("disabled must win over last error: %+v", rows["off"])
	}

	if rec := doPostRaw(s, "/api/config/knowledge/connectors/nope/sync", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown connector = %d, want 404", rec.Code)
	}
	if rec := doPostRaw(s, "/api/config/knowledge/connectors/docs/sync", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("sync = %d %q, want 202", rec.Code, rec.Body.String())
	}
	select {
	case name := <-rt.synced:
		if name != "docs" {
			t.Fatalf("synced %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SyncNow was not invoked")
	}

	rt.mu.Lock()
	rt.statuses[0].Running = true
	rt.mu.Unlock()
	if rec := doPostRaw(s, "/api/config/knowledge/connectors/docs/sync", ""); rec.Code != http.StatusConflict {
		t.Fatalf("sync while running = %d, want 409", rec.Code)
	}
	if got := statusOf()["docs"].Status; got != "syncing" {
		t.Fatalf("running pill = %q", got)
	}
}

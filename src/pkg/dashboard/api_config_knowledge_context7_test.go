package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

const kcContext7Put = `{"connectors":[{"name":"ctx7","type":"context7","enabled":true,"layer":"community","scope":{"library_ids":"/vercel/next.js"},"auth":{"env":"HIVE_TEST_C7_DASH_KEY"}}],"context7_auto_import":[{"id":"/vercel/next.js","query":"routing"}]}`

func TestKnowledgeConnectors_Context7PutThenGet(t *testing.T) {
	t.Setenv("HIVE_TEST_C7_DASH_KEY", "super-secret-value")
	s := covApiServer(t)
	if rec := doPutRaw(s, "/api/config/knowledge/connectors", kcContext7Put); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%q", rec.Code, rec.Body.String())
	}
	rec := doOwnerGet(s, "/api/config/knowledge/connectors")
	if strings.Contains(rec.Body.String(), "super-secret-value") {
		t.Fatal("GET must never return the credential value")
	}
	var got struct {
		Connectors []knowledgeConnectorJSON `json:"connectors"`
		AutoImport []context7AutoImportJSON `json:"context7_auto_import"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Connectors) != 1 || got.Connectors[0].CredentialSet == nil || !*got.Connectors[0].CredentialSet {
		t.Fatalf("connectors = %+v, want credential_set true", got.Connectors)
	}
	if len(got.AutoImport) != 1 || got.AutoImport[0].ID != "/vercel/next.js" || got.AutoImport[0].Query != "routing" {
		t.Fatalf("auto_import = %+v", got.AutoImport)
	}
	if ai := s.deps.Config.Knowledge.Context7.AutoImport; len(ai) != 1 || ai[0].Query != "routing" {
		t.Fatalf("config auto_import = %+v", ai)
	}

	t.Setenv("HIVE_TEST_C7_DASH_KEY", "")
	rec = doOwnerGet(s, "/api/config/knowledge/connectors")
	if !strings.Contains(rec.Body.String(), `"credential_set":false`) {
		t.Fatalf("unset env must report credential_set false: %s", rec.Body.String())
	}

	// Omitting context7_auto_import leaves the legacy list untouched.
	if rec := doPutRaw(s, "/api/config/knowledge/connectors", `{"connectors":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d", rec.Code)
	}
	if n := len(s.deps.Config.Knowledge.Context7.AutoImport); n != 1 {
		t.Fatalf("auto_import changed by a PUT that omitted it: %d", n)
	}
}

func TestKnowledgeConnectors_Context7PutRejectsBadAutoImport(t *testing.T) {
	s := covApiServer(t)
	for name, raw := range map[string]string{
		"no slash":  `{"connectors":[],"context7_auto_import":[{"id":"@evil.com/x"}]}`,
		"duplicate": `{"connectors":[],"context7_auto_import":[{"id":"/a/b"},{"id":"/a/b"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := doPutRaw(s, "/api/config/knowledge/connectors", raw); rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d body=%q, want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestKnowledgeConnectors_Context7ValidateHitsSearch(t *testing.T) {
	s := covApiServer(t)
	t.Setenv("HIVE_TEST_C7_DASH_KEY", "super-secret-value")
	orig := context7ValidateSearch
	t.Cleanup(func() { context7ValidateSearch = orig })
	var gotName, gotKey string
	var searchErr error
	context7ValidateSearch = func(_ context.Context, name, key string) ([]knowledge.Context7SearchResult, error) {
		gotName, gotKey = name, key
		return nil, searchErr
	}
	body := `{"name":"ctx7","type":"context7","enabled":true,"layer":"community","scope":{"library_ids":"/vercel/next.js"},"auth":{"env":"HIVE_TEST_C7_DASH_KEY"}}`
	rec := doPostRaw(s, "/api/config/knowledge/connectors/validate", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("validate = %d %q", rec.Code, rec.Body.String())
	}
	if gotName != "next.js" || gotKey != "super-secret-value" {
		t.Fatalf("search called with %q / key match %v", gotName, gotKey == "super-secret-value")
	}

	searchErr = errors.New("context7 search: HTTP 401 from Context7")
	rec = doPostRaw(s, "/api/config/knowledge/connectors/validate", body)
	if !strings.Contains(rec.Body.String(), `"ok":false`) || !strings.Contains(rec.Body.String(), "HTTP 401") {
		t.Fatalf("validate search failure = %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "super-secret-value") {
		t.Fatal("validate must not echo the credential")
	}
}

package dashboard

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKnowledgeListPaginationParameters(t *testing.T) {
	for _, tc := range []struct {
		query         string
		limit, offset int
	}{
		{"", 50, 0}, {"?limit=0&offset=-1", 50, 0}, {"?limit=bad&offset=bad", 50, 0},
		{"?limit=-5", 50, 0}, {"?limit=999&offset=10", 200, 10}, {"?limit=2&offset=3", 2, 3},
	} {
		t.Run(tc.query, func(t *testing.T) {
			limit, offset := knowledgeListPagination(httptest.NewRequest("GET", "/api/knowledge"+tc.query, nil))
			if limit != tc.limit || offset != tc.offset {
				t.Fatalf("got %d, %d; want %d, %d", limit, offset, tc.limit, tc.offset)
			}
		})
	}
}

func TestKnowledgeListPages(t *testing.T) {
	s := knowledgeTOCServer(t)
	vault := t.TempDir()
	for i := 0; i < 60; i++ {
		name := fmt.Sprintf("page-%03d.md", i)
		body := fmt.Sprintf("---\ntitle: Page %03d\ntype: gotcha\nlayer: project\n---\nA fact for pagination.\n", i)
		if err := os.WriteFile(filepath.Join(vault, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.deps.Knowledge.ConnectVault(vault, "pages"); err != nil {
		t.Fatal(err)
	}
	_, first := tocGet(t, s, "/api/knowledge?include_states=all")
	facts := first["facts"].([]interface{})
	if len(facts) != 50 || first["total"] != float64(65) || first["has_more"] != true {
		t.Fatalf("first page: %v", first)
	}
	_, last := tocGet(t, s, "/api/knowledge?include_states=all&offset=50")
	rest := last["facts"].([]interface{})
	if len(rest) != 15 || last["has_more"] != false {
		t.Fatalf("last page: %v", last)
	}
	seen := map[string]bool{}
	previous := ""
	for _, raw := range append(facts, rest...) {
		f := raw.(map[string]interface{})
		key := fmt.Sprint(f["layer"]) + "/" + fmt.Sprint(f["slug"])
		if seen[key] || strings.Compare(previous, key) > 0 {
			t.Fatalf("duplicate or unordered fact %s after %s", key, previous)
		}
		seen[key] = true
		previous = key
	}
	_, empty := tocGet(t, s, "/api/knowledge?offset=999999")
	if len(empty["facts"].([]interface{})) != 0 || empty["has_more"] != false {
		t.Fatalf("past end: %v", empty)
	}
	_, none := tocGet(t, s, "/api/knowledge?type=does-not-exist")
	if len(none["facts"].([]interface{})) != 0 || none["total"] != float64(0) {
		t.Fatalf("empty filter: %v", none)
	}
}

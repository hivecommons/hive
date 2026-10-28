package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestReadSupersedeAdoptionLatestTrustedOwnership(t *testing.T) {
	for _, reverted := range []bool{false, true} {
		t.Run(map[bool]string{false: "latest adoption", true: "reverted"}[reverted], func(t *testing.T) {
			comments := []map[string]any{}
			add := func(author, body string) {
				comments = append(comments, map[string]any{"user": map[string]string{"login": author}, "body": body})
			}
			add("hive[bot]", "<!-- hive:adopted mode=supersede superseded_by=90 head=aaa -->")
			add("hive[bot]", "<!-- hive:adopted mode=supersede superseded_by=91 head=aaa -->")
			add("attacker", "<!-- hive:adopted mode=supersede superseded_by=999 head=evil -->")
			if reverted {
				add("hive[bot]", "<!-- hive:adoption-reverted -->\nThe earlier copy closed unmerged.")
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(comments) }))
			defer srv.Close()
			client := gh.NewClient(srv.Client())
			client.BaseURL, _ = url.Parse(srv.URL + "/")
			marker, err := ReadSupersedeAdoption(t.Context(), client, "o", "r", 7, func(login string) bool { return login == "hive[bot]" })
			if err != nil {
				t.Fatal(err)
			}
			if reverted && marker.By != 0 {
				t.Fatalf("reverted adoption restored: %+v", marker)
			}
			if !reverted && (marker.By != 91 || marker.Head != "aaa") {
				t.Fatalf("wrong ownership: %+v", marker)
			}
		})
	}
}

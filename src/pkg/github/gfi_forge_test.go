package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/gfi"
)

func TestHardSuppressIssueLabelsIsCopyWithoutGFI(t *testing.T) {
	got := HardSuppressIssueLabels()
	if len(got) != len(hardSuppressIssueLabels) || got[0] != issueNeedsHumanLabel {
		t.Fatalf("got %v", got)
	}
	for _, l := range got {
		if strings.EqualFold(l, gfi.Label) || strings.EqualFold(l, gfi.HelpWantedLabel) {
			t.Fatalf("%q must not hard-suppress", l)
		}
	}
	got[0] = "mutated"
	if hardSuppressIssueLabels[0] == "mutated" {
		t.Fatal("returned slice aliases the package list")
	}
}

func TestUpsertIssueMarkerComment(t *testing.T) {
	var mu sync.Mutex
	comments := []map[string]any{
		{"id": 1, "body": "unrelated"},
	}
	var edited, created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			_ = json.NewEncoder(w).Encode(comments)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			b, _ := io.ReadAll(r.Body)
			var in struct{ Body string }
			_ = json.Unmarshal(b, &in)
			created = append(created, in.Body)
			comments = append(comments, map[string]any{"id": 2, "body": in.Body})
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2, "body": in.Body})
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/issues/comments/2"):
			b, _ := io.ReadAll(r.Body)
			var in struct{ Body string }
			_ = json.Unmarshal(b, &in)
			edited = append(edited, in.Body)
			comments[1]["body"] = in.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2, "body": in.Body})
		default:
			http.Error(w, r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	ctx := context.Background()

	// Pure lookup: nothing found, nothing created.
	res, err := c.UpsertIssueMarkerComment(ctx, "widgets", 7, gfi.Marker, "", false)
	if err != nil || res.Found || res.Created || len(created) != 0 {
		t.Fatalf("lookup: %+v %v created=%v", res, err, created)
	}
	// create=false with a body and no match: still nothing.
	if res, err = c.UpsertIssueMarkerComment(ctx, "widgets", 7, gfi.Marker, gfi.Marker+"\nhi", false); err != nil || res.Found || len(created) != 0 {
		t.Fatalf("no-create: %+v %v", res, err)
	}
	// Create.
	body := gfi.Marker + "\nhello"
	if res, err = c.UpsertIssueMarkerComment(ctx, "acme/widgets", 7, gfi.Marker, body, true); err != nil || !res.Created || res.Found || len(created) != 1 {
		t.Fatalf("create: %+v %v", res, err)
	}
	// Same body: found, no edit.
	if res, err = c.UpsertIssueMarkerComment(ctx, "widgets", 7, gfi.Marker, body, true); err != nil || !res.Found || res.Previous != body || len(edited) != 0 {
		t.Fatalf("idempotent: %+v %v edited=%v", res, err, edited)
	}
	// Different body: edited in place, Previous reports the old one.
	if res, err = c.UpsertIssueMarkerComment(ctx, "widgets", 7, gfi.Marker, gfi.SupersededMarker+"\nbye", false); err != nil || !res.Found || res.Previous != body || len(edited) != 1 || len(created) != 1 {
		t.Fatalf("edit: %+v %v edited=%v", res, err, edited)
	}
	// The superseded marker is now findable and the live one is not.
	if res, _ = c.UpsertIssueMarkerComment(ctx, "widgets", 7, gfi.SupersededMarker, "", false); !res.Found {
		t.Fatal("superseded marker not found")
	}
	if res, _ = c.UpsertIssueMarkerComment(ctx, "widgets", 7, gfi.Marker, "", false); res.Found {
		t.Fatal("live marker still matched after supersede")
	}
	var nilClient *Client
	if _, err := nilClient.UpsertIssueMarkerComment(ctx, "widgets", 7, gfi.Marker, "", false); err == nil {
		t.Fatal("nil client should error")
	}
}

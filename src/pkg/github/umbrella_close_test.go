package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/gfi"
)

// hivecommons/hive#19078: an umbrella is withheld from contributors but must
// stay closable by every close path, unlike a hive/tracking archive.
func TestUmbrellaStaysClosableAndTrackingDoesNot(t *testing.T) {
	cases := []struct {
		name         string
		number       int
		labels       []string
		body         string
		title        string
		wantTracking bool
	}{
		{"umbrella label", 101, []string{"umbrella"}, "children carry the work", "plan", false},
		{"tracker label", 102, []string{"tracker"}, "", "plan", false},
		{"epic label", 103, []string{"epic"}, "", "plan", false},
		{"umbrella marker", 104, nil, "plan\n" + gfi.UmbrellaMarker + "\n", "plan", false},
		{"epic title prefix", 105, nil, "- [ ] #1\n- [ ] #2\n", "[Epic] plan", false},
		{"owner-only marker", 106, nil, gfi.HumanOnlyMarker, "plan", false},
		{"hive/tracking label", 107, []string{gfi.TrackingLabel}, "", "weekly archive", true},
		{"hive tracking marker", 108, nil, gfi.TrackingMarker, "weekly archive", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var patched bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/"+strconv.Itoa(tc.number)) {
					is := trackingTestIssue(tc.number, tc.labels, tc.body)
					is.Title = gh.Ptr(tc.title)
					_ = json.NewEncoder(w).Encode(is)
					return
				}
				if r.Method == http.MethodPatch {
					patched = true
					_ = json.NewEncoder(w).Encode(map[string]any{"number": tc.number, "state": "closed"})
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()
			c := testClient(t, srv.URL)

			if _, tracking := isTrackingIssue(umbrellaTestIssue(tc.number, tc.labels, tc.body, tc.title)); tracking != tc.wantTracking {
				t.Fatalf("isTrackingIssue = %v, want %v", tracking, tc.wantTracking)
			}
			err := c.CloseIssue(context.Background(), "o/r", tc.number, IssueCloseOptions{StateReason: IssueStateReasonCompleted})
			if tc.wantTracking {
				if !errors.Is(err, ErrTrackingIssue) || patched {
					t.Fatalf("hive/tracking close: err=%v patched=%v, want ErrTrackingIssue and no write", err, patched)
				}
				return
			}
			if err != nil || !patched {
				t.Fatalf("umbrella close: err=%v patched=%v, want a close", err, patched)
			}
		})
	}
}

// The per-repo parent_autoclose policy treats umbrella, tracker and
// owner-only markers as ordinary parents; the epic label keeps its existing
// rule (closable only under epics_too).
func TestParentAutoCloseDoesNotExemptUmbrellaMarkers(t *testing.T) {
	cases := []struct {
		policy string
		labels []string
		want   string
	}{
		{config.ParentAutoCloseOn, []string{"umbrella"}, ""},
		{config.ParentAutoCloseOn, []string{"tracker"}, ""},
		{config.ParentAutoCloseOn, []string{"human-only"}, ""},
		{config.ParentAutoCloseOn, []string{"epic"}, "labelled epic"},
		{config.ParentAutoCloseEpicsToo, []string{"epic", "umbrella"}, ""},
		{config.ParentAutoCloseOff, []string{"umbrella"}, "repo parent_autoclose is false"},
	}
	for _, tc := range cases {
		if got := parentAutoCloseExempt(tc.policy, tc.labels); got != tc.want {
			t.Errorf("parentAutoCloseExempt(%q, %v) = %q, want %q", tc.policy, tc.labels, got, tc.want)
		}
	}
}

func umbrellaTestIssue(number int, labels []string, body, title string) *gh.Issue {
	i := trackingTestIssue(number, labels, body)
	i.Title = gh.Ptr(title)
	return i
}

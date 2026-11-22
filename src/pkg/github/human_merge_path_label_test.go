package github

// The human-merge-path rule applies needs-owner-merge, never `hold`
// (hivecommons/hive#18468): the label says a person merges the PR, and it is
// never read as a hold, so the PR stays actionable for its fixer.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestHumanMergePathRuleAppliesNeedsOwnerMergeNotHold(t *testing.T) {
	f := &humanPathMergeFixture{files: []string{".claude/settings.json"}}
	_, resp := runHumanPathMergeRequest(t, f, map[string][]string{"o/r": {".claude/**"}})
	if resp.OK || f.merges != 0 {
		t.Fatalf("result=%+v merges=%d; a human-merge path must never be merged by the relay", resp, f.merges)
	}
	if len(f.labels) != 1 || f.labels[0] != "needs-owner-merge" {
		t.Fatalf("labels = %v, want exactly [needs-owner-merge]", f.labels)
	}
	for _, l := range f.labels {
		if strings.EqualFold(l, "hold") {
			t.Fatalf("labels = %v: the rule must never apply hold", f.labels)
		}
	}
	if len(f.comments) != 1 || IsLegacyHumanMergePathHoldNotice(f.comments[0]) || !strings.Contains(f.comments[0], "It is not a hold") {
		t.Fatalf("comments = %q, want one new-style notice that says the label is not a hold", f.comments)
	}
}

func TestHumanMergePathLabelIsNeverAHold(t *testing.T) {
	if HumanMergePathLabel != "needs-owner-merge" {
		t.Fatalf("HumanMergePathLabel = %q, want needs-owner-merge (the default human_merge_labels label)", HumanMergePathLabel)
	}
	for _, extra := range [][]string{nil, {"merge"}, {"owner"}, {"owner-merge"}, {"hive-pause/x"}} {
		if HasHoldLabelWith([]string{"needs-owner-merge"}, extra) {
			t.Fatalf("HasHoldLabelWith(needs-owner-merge, extra=%v) = true; a substring pattern must never make the label a hold", extra)
		}
		if HasHoldLabelWith([]string{"Needs-Owner-Merge "}, extra) {
			t.Fatalf("HasHoldLabelWith is case/space sensitive for the label (extra=%v)", extra)
		}
	}
	// An operator who lists the label itself as a hold label gets one.
	if !HasHoldLabelWith([]string{"needs-owner-merge"}, []string{" Needs-Owner-Merge"}) {
		t.Fatal("an explicitly listed needs-owner-merge must hold")
	}
	if !reservedLabelForAgents("needs-owner-merge", AutoMergeQueuedLabel) {
		t.Fatal("agents must not add or remove needs-owner-merge through the label relay")
	}
	if !HasHoldLabelWith([]string{"needs-owner-merge", "hold"}, nil) {
		t.Fatal("a person's hold beside the label must still hold the PR")
	}
	c := NewClientForTest("http://127.0.0.1:0", "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetHoldLabels([]string{"owner"})
	if c.isHeld([]string{"needs-owner-merge"}) {
		t.Fatal("Client.isHeld read needs-owner-merge as a hold through an extra hold label")
	}
}

func TestLegacyHumanMergePathHoldNotice(t *testing.T) {
	legacy := HumanMergePathMarker + "\nHive will not merge this PR.\n\nHive added the `hold` label. A maintainer should review and merge this PR by hand.\n"
	if !IsLegacyHumanMergePathHoldNotice(legacy) {
		t.Fatal("the notice the rule posted while it applied hold must read as legacy")
	}
	if IsLegacyHumanMergePathHoldNotice(HumanMergePathComment([]string{"a.yml"})) {
		t.Fatal("the new notice must not read as a legacy hold notice")
	}
	if IsLegacyHumanMergePathHoldNotice("Hive added the `hold` label.") {
		t.Fatal("without the marker nothing is a legacy notice")
	}
}

// labelledPRServer serves one PR (#7) at head with labels, records label
// adds and comments, and lists the comments it has.
type labelledPRServer struct {
	mu       sync.Mutex
	head     string
	labels   []string
	added    []string
	comments []string
}

func (s *labelledPRServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
		var labels []map[string]any
		for _, l := range s.labels {
			labels = append(labels, map[string]any{"name": l})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "head": map[string]any{"sha": s.head}, "labels": labels})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/labels":
		var labels []string
		_ = json.NewDecoder(r.Body).Decode(&labels)
		s.added = append(s.added, labels...)
		_ = json.NewEncoder(w).Encode([]map[string]string{})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
		var out []map[string]any
		for _, b := range s.comments {
			out = append(out, map[string]any{"body": b})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.comments = append(s.comments, body.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": len(s.comments)})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestLabelForHumanMergePathsStampsHeadAndSkipsAnExistingLabel(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name       string
		labels     []string
		wantAdd    bool
		wantMarker bool
	}{
		{name: "unlabelled PR gets the label and the head stamp", wantAdd: true, wantMarker: true},
		{name: "a person's hold does not stop the label", labels: []string{"hold"}, wantAdd: true, wantMarker: true},
		{name: "an existing label is not attributed or re-added", labels: []string{"needs-owner-merge"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &labelledPRServer{head: head, labels: tc.labels}
			srv := httptest.NewServer(http.HandlerFunc(s.handler))
			defer srv.Close()
			c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err := LabelForHumanMergePaths(context.Background(), c.client, "o", "r", 7, []string{".claude/settings.json"}, head); err != nil {
				t.Fatal(err)
			}
			if got := len(s.added) == 1 && s.added[0] == HumanMergePathLabel; got != tc.wantAdd {
				t.Fatalf("added = %v, want label added = %v", s.added, tc.wantAdd)
			}
			for _, l := range s.added {
				if strings.EqualFold(l, "hold") {
					t.Fatalf("added %v: never hold", s.added)
				}
			}
			if len(s.comments) != 1 {
				t.Fatalf("comments = %q, want one notice", s.comments)
			}
			if got := strings.Contains(s.comments[0], HumanMergeHeadMarker(head)); got != tc.wantMarker {
				t.Fatalf("head marker present = %v, want %v: %q", got, tc.wantMarker, s.comments[0])
			}
		})
	}
}

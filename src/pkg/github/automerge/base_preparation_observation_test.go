package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestActiveBasePreparationCurrentHeadLifecycleAndReadOnlyRestart(t *testing.T) {
	now := time.Date(2026, 10, 30, 20, 40, 0, 0, time.UTC)
	for _, mode := range []string{"admitted", "refreshed", "author-push", "amended-merge", "wrong-base-parent", "held", "closed", "retargeted", "expired", "released", "policy-off", "released-during-read", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "preparation")
			store := NewBasePreparationStore(dir)
			key := preparationKey("acme", "widget", "main")
			if err := store.change(key, func(st *basePreparation) error {
				st.Owner = preparationCandidate{Number: 7, Head: "old", Lane: stageLaneTrusted, Since: now.Add(-10 * time.Minute)}
				st.Attempts = map[int]preparationAttempt{7: {Head: "old", Base: "base1", At: now.Add(-9 * time.Minute)}}
				st.Waiting = []preparationCandidate{{Number: 8, Since: now.Add(-5 * time.Minute)}}
				if mode == "expired" {
					st.Owner.Since = now.Add(-24 * time.Hour)
				}
				if mode == "released" {
					st.Owner = preparationCandidate{}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			path := store.statePath(key) + ".json"
			if mode == "unreadable" {
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			pr := preparationPR(7, "main", "refreshed")
			if mode == "admitted" {
				pr.Head.SHA = gh.Ptr("old")
			}
			if mode == "held" {
				pr.Labels = []*gh.Label{{Name: gh.Ptr("hold")}}
			}
			if mode == "closed" {
				pr.State = gh.Ptr("closed")
			}
			if mode == "retargeted" {
				pr.Base.Ref = gh.Ptr("other")
			}
			reads := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/7"):
					if mode == "released-during-read" {
						if err := store.change(key, func(st *basePreparation) error {
							st.Owner = preparationCandidate{}
							return nil
						}); err != nil {
							t.Error(err)
						}
					}
					_ = json.NewEncoder(w).Encode(pr)
				case strings.HasSuffix(r.URL.Path, "/commits/refreshed"):
					parents := []map[string]string{{"sha": "old"}, {"sha": "base1"}}
					if mode == "author-push" {
						parents = parents[:1]
					}
					if mode == "wrong-base-parent" {
						parents[1]["sha"] = "other-base"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"sha": "refreshed", "parents": parents})
				case strings.Contains(r.URL.Path, "/compare/"):
					patch := "+unchanged PR content"
					if mode == "amended-merge" && strings.HasSuffix(r.URL.Path, "...refreshed") {
						patch = "+author changed content in amended merge"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]string{{
						"filename": "widget.go", "status": "modified", "patch": patch,
					}}})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := preparationEngine(srv.URL, NewBasePreparationStore(dir))
			c.now = func() time.Time { return now }
			if mode == "policy-off" {
				c.requireCurrentBase = nil
			}
			observation, active, err := c.ActiveBasePreparation(context.Background(), "acme", "widget", "main")
			if (err != nil) != (mode == "unreadable") {
				t.Fatalf("error=%v", err)
			}
			want := mode == "admitted" || mode == "refreshed"
			if active != want {
				t.Fatalf("active=%v want=%v observation=%+v", active, want, observation)
			}
			if active && (observation.Number != 7 || observation.HeadSHA != pr.GetHead().GetSHA() || observation.Waiting != 1 || observation.Reason != basePreparationReady) {
				t.Fatalf("observation=%+v", observation)
			}
			if (mode == "released" || mode == "expired" || mode == "policy-off" || mode == "unreadable") && reads != 0 {
				t.Fatalf("inactive owner made %d remote reads", reads)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "released-during-read" && string(before) != string(after) {
				t.Fatal("read-only observation rewrote durable state")
			}
		})
	}
}

func TestPreparationOwnerSnapshotAbsentDoesNotCreateState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	for _, store := range []*BasePreparationStore{NewBasePreparationStore(dir), NewBasePreparationStore("")} {
		got, err := store.ownerSnapshot(preparationKey("acme", "widget", "main"))
		if err != nil || got.Owner.Number != 0 {
			t.Fatalf("snapshot=%+v error=%v", got, err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("read-only snapshot created state: %v", err)
	}
}

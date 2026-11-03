package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/github"
)

func TestClaimHooksRemoveLabelAfterSecondClaimExpires(t *testing.T) {
	var mu sync.Mutex
	// A small label store: the hooks also write the hive/claimed-by-<who>
	// modifier (the generic `human` for a human principal, never the login)
	// and swap stage labels through the stage engine, so only the
	// stage/020-claimed add/remove is counted here.
	labels := map[string]bool{}
	adds, removes := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			out := []map[string]string{}
			for name := range labels {
				out = append(out, map[string]string{"name": name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 63, "labels": out})
		case http.MethodPost:
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, name := range body {
				if name == claims.LabelClaimed {
					adds++
				}
				labels[name] = true
			}
			_, _ = w.Write([]byte(`[]`))
		case http.MethodDelete:
			_, name, _ := strings.Cut(r.URL.Path, "/labels/")
			name, _ = url.PathUnescape(name)
			if name == claims.LabelClaimed {
				removes++
			}
			delete(labels, name)
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := github.NewClientForTest(srv.URL, "o", nil, logger)
	dir := t.TempDir()
	ml, err := mutation.OpenLedger(filepath.Join(dir, "mutations.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := mutation.OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetMutationBoundary(&mutation.Boundary{Executor: mutation.Executor{Ledger: ml, Journal: journal, Mode: "enforce"}, Holder: "test"})
	cfg := &config.Config{}
	no := false
	cfg.Governor.Claims.Comment = &no
	queue := &claimEffectQueue{}
	hooks := githubClaimHooksOn(queue, context.Background(), cfg, func() *github.Client { return c }, logger)
	l, err := claims.New(filepath.Join(dir, "claims.json"), claims.DefaultPolicy(), hooks)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	l.SetNow(func() time.Time { return now })
	wait := func(wantAdds, wantRemoves int) {
		t.Helper()
		key := mutation.TaskClaim("o/r", "o/r#label_mutation/63").Key()
		testutil.Eventually(t, 5*time.Second, func() bool {
			mu.Lock()
			a, r := adds, removes
			claimedBy := labels[claims.ClaimedByHumanLabel]
			mu.Unlock()
			if a != wantAdds || r != wantRemoves {
				return false
			}
			// The hooks also write (and later clear) the claimed-by modifier
			// through the same mutation key; wait for that write too.
			if claimedBy != (wantAdds > wantRemoves) {
				return false
			}
			// Wait until the boundary has durably acknowledged the HTTP response
			// and released the writer before starting the next hook.
			entry, ok := ml.Get(key)
			return ok && entry.State == mutation.StateReleased
		}, "hooks did not complete: want adds=%d removes=%d (adds=%d removes=%d labels=%v)", wantAdds, wantRemoves, adds, removes, labels)
	}
	for cycle := 1; cycle <= 2; cycle++ {
		if _, err := l.Claim(claims.Request{Repo: "o/r", Issue: 63, Holder: "alice", Kind: claims.KindHuman, TTL: time.Minute}); err != nil {
			t.Fatal(err)
		}
		wait(cycle, cycle-1)
		now = now.Add(2 * time.Minute)
		if n := l.Expire(); n != 1 {
			t.Fatalf("expired %d claims, want 1", n)
		}
		wait(cycle, cycle)
	}
	// The release hook keeps writing the stage and journal after the label
	// counts settle; drain it so nothing touches dir once the test returns.
	testutil.Eventually(t, 5*time.Second, func() bool {
		queue.mu.Lock()
		defer queue.mu.Unlock()
		return len(queue.pending) == 0
	}, "claim effect queue did not drain")
	mu.Lock()
	defer mu.Unlock()
	if labels[claims.LabelClaimed] {
		t.Fatal("claimed label remains after second expiry")
	}
}

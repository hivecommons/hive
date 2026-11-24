package github

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/prgate"
)

const (
	canaryForkHead       = "cccccccccccccccccccccccccccccccccccccccc"
	canaryDependabotHead = "dddddddddddddddddddddddddddddddddddddddd"
	canaryRepo           = "example-org/example-repo"
)

func TestPRGateOriginOf(t *testing.T) {
	tests := []struct {
		name string
		s    PRGateSubject
		want string
	}{
		{"same-repo branch", PRGateSubject{HeadRepo: "example-org/example-repo", Author: "someone"}, PRGateOriginSameRepo},
		{"same repo, other case", PRGateSubject{HeadRepo: "Example-Org/Example-Repo", Author: "someone"}, PRGateOriginSameRepo},
		{"fork", PRGateSubject{HeadRepo: "contributor/example-repo", Author: "contributor"}, PRGateOriginFork},
		{"deleted fork", PRGateSubject{HeadRepo: "", Author: "contributor"}, PRGateOriginFork},
		{"dependabot", PRGateSubject{HeadRepo: "example-org/example-repo", Author: "dependabot[bot]"}, PRGateOriginDependabot},
	}
	for _, tc := range tests {
		if got := PRGateOriginOf(canaryRepo, tc.s); got != tc.want {
			t.Errorf("%s: origin = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// canaryGitHub serves the recorded fork and Dependabot PRs and answers status
// writes with statusCode (201 when zero).
func canaryGitHub(t *testing.T, statusCode int) *Client {
	t.Helper()
	fork, err := os.ReadFile("testdata/pr_gate/pull_fork.json")
	if err != nil {
		t.Fatal(err)
	}
	dependabot, err := os.ReadFile("testdata/pr_gate/pull_dependabot.json")
	if err != nil {
		t.Fatal(err)
	}
	return prGateTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/31"):
			_, _ = w.Write(fork)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/32"):
			_, _ = w.Write(dependabot)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/statuses/"):
			if statusCode != 0 {
				w.WriteHeader(statusCode)
				_, _ = w.Write([]byte(`{"message": "Resource not accessible by integration"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 1}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func canaryGate(c *Client) *PRGate {
	return NewPRGate(c, func(string) string { return "shadow" }, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A status the base installation posts on a fork head and on a Dependabot
// head is recorded under that origin with the exact head, and shows on the
// health the dashboard serves.
func TestPRGateCanaryRecordsForkAndDependabotHeads(t *testing.T) {
	g := canaryGate(canaryGitHub(t, 0))
	ctx := context.Background()
	if out := g.HandleEvent(ctx, canaryRepo, 31, canaryForkHead); len(out.Posted) != 1 {
		t.Fatalf("fork head: %+v", out)
	}
	if out := g.HandleEvent(ctx, canaryRepo, 32, canaryDependabotHead); len(out.Posted) != 1 {
		t.Fatalf("dependabot head: %+v", out)
	}
	h := g.Health([]string{canaryRepo}, true, time.Now())
	if h == nil {
		t.Fatal("no health")
	}
	for origin, head := range map[string]string{PRGateOriginFork: canaryForkHead, PRGateOriginDependabot: canaryDependabotHead} {
		r, ok := h.Canary[origin]
		if !ok || r.Result != PRGateCanaryPosted || r.Head != head || r.LastSuccessHead != head || r.Context != prgate.ContextVerifyShadow || r.Repo != canaryRepo {
			t.Errorf("%s canary = %+v (present %t)", origin, r, ok)
		}
	}
	if _, ok := h.Canary[PRGateOriginSameRepo]; ok {
		t.Error("no same-repo head was posted, none may be reported")
	}
}

// A refused write on a fork head is recorded as such and proves nothing.
func TestPRGateCanaryRecordsARefusalWithoutProof(t *testing.T) {
	g := canaryGate(canaryGitHub(t, http.StatusForbidden))
	g.HandleEvent(context.Background(), canaryRepo, 31, canaryForkHead)
	r := g.Health([]string{canaryRepo}, true, time.Now()).Canary[PRGateOriginFork]
	if r.Result != PRGateCanaryForbidden || r.LastSuccessHead != "" {
		t.Errorf("fork canary after a 403 = %+v", r)
	}
}

func TestPRGateCanaryHeadMemoryIsBounded(t *testing.T) {
	k := newPRGateCanary(nil)
	for i := 0; i < maxPRGateCanaryHeads+10; i++ {
		k.noteHead(canaryRepo, fmt.Sprintf("%040x", i), PRGateOriginFork)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.origins) > maxPRGateCanaryHeads {
		t.Fatalf("head memory holds %d entries, want at most %d", len(k.origins), maxPRGateCanaryHeads)
	}
}

func TestPRGateCanaryIgnoresUnclassifiedWrites(t *testing.T) {
	k := newPRGateCanary(nil)
	k.record(canaryRepo, 7, canaryForkHead, prgate.ContextVerifyShadow, nil)
	if k.snapshot() != nil {
		t.Errorf("a write on a head never read must not be reported: %+v", k.snapshot())
	}
}

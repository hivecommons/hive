package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/sentinel"
)

func checkpointPatch(lines ...string) string {
	return fmt.Sprintf("@@ -0,0 +1,%d @@\n+%s\n", len(lines), strings.Join(lines, "\n+"))
}

func TestCheckpointWholeRegionsAndContinuation(t *testing.T) {
	c := newCheckpointSource(matrixTestConfig())
	patch := "@@ -1,4 +1,7 @@\n-old\n+\tnew\n+\n+repeat\n+repeat\n context\n-again\n+again\n tail\n"
	obs := matrixObs(3, "real-head", sentinel.File{Path: "a.go", Patch: patch})
	now := time.Now()
	if n := c.ingest(obs, c.policyEpoch(), now); n != 7 {
		t.Fatalf("admitted %d changed lines", n)
	}
	first := c.since(0, 1)
	if len(first.Regions) != 1 || len(first.Regions[0].Lines) != 5 || first.Seq != 1 || first.Latest != 2 {
		t.Fatalf("first page: %+v", first)
	}
	want := []checkpointLine{{"-", "old"}, {"+", "\tnew"}, {"+", ""}, {"+", "repeat"}, {"+", "repeat"}}
	r := first.Regions[0]
	if !reflect.DeepEqual(r.Lines, want) || r.Head != "real-head" || r.PRURL != "https://github.com/"+matrixTestRepo+"/pull/3" || r.Stage != "pushed-checkpoint" || !r.At.Equal(now) {
		t.Fatalf("region %+v", r)
	}
	next := c.since(first.Seq, 1)
	if len(next.Regions) != 1 || len(next.Regions[0].Lines) != 2 || next.Seq != 2 || next.Latest != 2 {
		t.Fatalf("next page %+v", next)
	}
	if n := c.ingest(obs, c.policyEpoch(), now); n != 0 {
		t.Fatal("unchanged head replayed")
	}
	obs.HeadSHA = "next-head"
	if n := c.ingest(obs, c.policyEpoch(), now); n != 7 {
		t.Fatal("new actual head was suppressed")
	}
}

func TestCheckpointDropsUnsafeWholeRegions(t *testing.T) {
	bad := []string{"password: hunter2", "token = \"short\"", "-----BEGIN PRIVATE KEY-----", "[REDACTED]", strings.Repeat("a", 33), "bad\x01text", string([]byte{0xff}), strings.Repeat("a ", checkpointMaxLineRunes)}
	for _, op := range []string{"+", "-"} {
		for i, line := range bad {
			t.Run(fmt.Sprintf("%s/%d", op, i), func(t *testing.T) {
				c := newCheckpointSource(matrixTestConfig())
				// The secret and innocent line form one region. A separate safe region
				// after context survives; no apparently complete fragment of the first does.
				oldN, newN := 1, 3
				if op == "-" {
					oldN, newN = 3, 1
				}
				patch := fmt.Sprintf("@@ -1,%d +1,%d @@\n%ssafe peer\n%s%s\n context\n@@ -10,0 +10 @@\n+visible\n", oldN, newN, op, op, line)
				obs := matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: patch})
				c.ingest(obs, c.policyEpoch(), time.Now())
				got := c.since(0, 200)
				if len(got.Regions) != 1 || !reflect.DeepEqual(got.Regions[0].Lines, []checkpointLine{{"+", "visible"}}) {
					t.Fatalf("unsafe region leaked: %+v", got.Regions)
				}
			})
		}
	}
}

func TestCheckpointRejectsIncompleteDiff(t *testing.T) {
	for _, patch := range []string{"+orphan", "@@ -0,0 +1,2 @@\n+only one\n", "@@ -1 +1 @@\n-old\n", "@@ malformed\n+x\n", "@@ -0,0 +1 @@\n+x\n+y\n", "@@ -0,0 +1 @@\n+x\n@@ -0,0 +2,2 @@\n+truncated\n", "@@ -0,0 +1 @@\n+binary\x00\n"} {
		if r := checkpointRegions(patch); len(r) != 0 {
			t.Fatalf("incomplete patch admitted: %+v", r)
		}
	}
	r := checkpointRegions("@@ -1 +1 @@\n--- literal\n+++ literal\n\\ No newline at end of file\n")
	if len(r) != 1 || len(r[0]) != 2 || r[0][1].Line != "++ literal" {
		t.Fatalf("literal diff signs lost: %+v", r)
	}
}

func TestCheckpointObservationAndPolicyBoundaries(t *testing.T) {
	for _, kind := range []string{"private-or-unknown", "unidentified", "unlisted", "no-head", "no-pr", "excluded", "secret-path"} {
		t.Run(kind, func(t *testing.T) {
			cfg := matrixTestConfig()
			cfg.ExcludePaths = []string{"skip/**"}
			c := newCheckpointSource(cfg)
			obs := matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: checkpointPatch("safe")})
			switch kind {
			case "private-or-unknown":
				obs.Private = true
			case "unidentified":
				obs.HiveIdentified = false
			case "unlisted":
				obs.Repo = "other/repo"
			case "no-head":
				obs.HeadSHA = ""
			case "no-pr":
				obs.Number = 0
			case "excluded":
				obs.Files[0].Path = "skip/a.go"
			case "secret-path":
				obs.Files[0].Path = ".env"
			}
			if c.ingest(obs, c.policyEpoch(), time.Now()) != 0 || len(c.since(0, 200).Regions) != 0 {
				t.Fatal("boundary admitted content")
			}
		})
	}
	c := newCheckpointSource(matrixTestConfig())
	epoch := c.policyEpoch()
	obs := matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: checkpointPatch("safe")})
	c.ingest(obs, epoch, time.Now())
	cfg := matrixTestConfig()
	cfg.ExcludePaths = []string{"elsewhere/**"}
	c.configure(cfg)
	if c.policyEpoch() == epoch || len(c.since(0, 200).Regions) != 0 || c.since(0, 200).Seq != c.since(0, 200).Latest {
		t.Fatal("policy change retained code or epoch")
	}
	if c.ingest(obs, epoch, time.Now()) != 0 {
		t.Fatal("old queued observation crossed policy epoch")
	}
	epoch = c.policyEpoch()
	c.configure(cfg)
	if c.policyEpoch() != epoch {
		t.Fatal("unchanged policy reset epoch")
	}
	c.ingest(obs, epoch, time.Now())
	cfg.Enabled = false
	c.configure(cfg)
	if len(c.since(0, 200).Regions) != 0 || c.ingest(obs, c.policyEpoch(), time.Now()) != 0 {
		t.Fatal("disable retained or admitted code")
	}
}

func TestCheckpointPolicyClearsSharedQueue(t *testing.T) {
	m := newMatrixSource(matrixTestConfig(), nil)
	m.Observe(matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: checkpointPatch("queued code")}))
	if len(m.in) != 1 {
		t.Fatal("observation did not enter queue")
	}
	cfg := matrixTestConfig()
	cfg.Enabled = false
	m.setConfig(cfg)
	if len(m.in) != 0 {
		t.Fatal("policy change retained queued code")
	}
	m.policyMu.Lock()
	done := make(chan struct{})
	go func() { m.Observe(matrixObs(2, "h")); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		m.policyMu.Unlock()
		t.Fatal("observer blocked on policy update")
	}
	m.policyMu.Unlock()

}

func TestCheckpointWholeBounds(t *testing.T) {
	c := newCheckpointSource(matrixTestConfig())
	now := time.Now()
	obs := matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: checkpointPatch(append(matrixUniqueLines("safe", 199), "last", "over")...)})
	if c.ingest(obs, c.policyEpoch(), now) != 0 {
		t.Fatal("per-head bound sliced oversized region")
	}
	for pr := 2; pr < 25; pr++ {
		obs = matrixObs(pr, "h", sentinel.File{Path: "a.go", Patch: checkpointPatch(matrixUniqueLines("safe", 99)...)})
		c.ingest(obs, c.policyEpoch(), now.Add(time.Duration(pr)*time.Minute))
	}
	got := c.since(0, 10000)
	if c.lines > matrixBacklogCap || c.bytes > checkpointMaxBytes || len(got.Regions) != matrixBacklogCap/99 {
		t.Fatalf("buffer bounds lines=%d bytes=%d regions=%d", c.lines, c.bytes, len(got.Regions))
	}
	for _, r := range got.Regions {
		if len(r.Lines) != 99 {
			t.Fatal("eviction sliced region")
		}
	}
	// Byte bound is independent of line count, and cloned text keeps retained
	// strings from pinning the much larger original patch backing storage.
	c = newCheckpointSource(matrixTestConfig())
	text := strings.Repeat("safe text ", 300)
	for pr := 1; pr <= 40; pr++ {
		c.ingest(matrixObs(pr, "h", sentinel.File{Path: "a.go", Patch: checkpointPatch(text, text, text, text, text)}), c.policyEpoch(), now.Add(time.Duration(pr)*time.Minute))
	}
	if c.bytes > checkpointMaxBytes || len(c.regions) >= 40 {
		t.Fatal("text byte bound not enforced")
	}
	c = newCheckpointSource(matrixTestConfig())
	for pr := 1; pr <= 4; pr++ {
		n := c.ingest(matrixObs(pr, "h", sentinel.File{Path: "a.go", Patch: checkpointPatch(matrixUniqueLines("safe", 199)...)}), c.policyEpoch(), now)
		if (pr <= 3 && n != 199) || (pr == 4 && n != 0) {
			t.Fatalf("rate bound split region: pr=%d n=%d", pr, n)
		}
	}
}

func TestCheckpointAPIAuthColdAndEpoch(t *testing.T) {
	s := NewServerWithAuth(0, "checkpoint-test-token", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.mux.HandleFunc("GET /api/matrix/lines", s.handleMatrixLines)
	// Active configuration exists before the sentinel has created its observer.
	cfg := matrixTestConfig()
	s.deps = &Dependencies{Config: &config.Config{}}
	s.deps.Config.Dashboard.Matrix = cfg
	defer func() {
		if s.matrix != nil {
			s.matrix.close()
			<-s.matrix.done
		}
	}()
	request := func(auth, role string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/matrix/lines?since=0&format=regions&max=1", nil)
		req.Header.Set("Authorization", auth)
		req.Header.Set("X-Hive-Role", role)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cacheable response")
		}
		return rr
	}
	for _, role := range []string{"", config.RoleOwner} {
		if rr := request("", role); rr.Code != 401 {
			t.Fatalf("anonymous/forged role got %d", rr.Code)
		}
	}
	rr := request("Bearer checkpoint-test-token", "")
	var cold checkpointResponse
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &cold) != nil || cold.Epoch == "" || cold.LastIngestAt != nil || len(cold.Regions) != 0 || cold.Source != "Pushed PR" || cold.Stage != "pushed-checkpoint" {
		t.Fatalf("cold response %d %s", rr.Code, rr.Body.String())
	}
	// Feed the real observer queue, including a deletion-only checkpoint.
	s.MatrixObserver(cfg)(matrixObs(1, "real-head", sentinel.File{Path: "a.go", Patch: "@@ -1,2 +0,0 @@\n-old\n-old\n"}))
	deadline := time.After(2 * time.Second)
	for len(s.matrix.checkpoints.since(0, 1).Regions) == 0 {
		select {
		case <-deadline:
			t.Fatal("observer did not feed checkpoint source")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	s.RecordMatrixIngest(time.Now().UTC(), time.Minute)
	rr = request("Bearer checkpoint-test-token", "")
	var page checkpointResponse
	if json.Unmarshal(rr.Body.Bytes(), &page) != nil || len(page.Regions) != 1 || len(page.Regions[0].Lines) != 2 || page.Seq != page.Latest || page.LastIngestAt == nil {
		t.Fatalf("whole response %s", rr.Body.String())
	}
	cfg.ExcludePaths = []string{"a.go"}
	s.deps.Config.Dashboard.Matrix = cfg
	rr = request("Bearer checkpoint-test-token", "")
	var changed checkpointResponse
	if json.Unmarshal(rr.Body.Bytes(), &changed) != nil || changed.Epoch == cold.Epoch || len(changed.Regions) != 0 {
		t.Fatalf("policy change %s", rr.Body.String())
	}
	s.deps.Config.Dashboard.Matrix.Enabled = false
	if rr := request("Bearer checkpoint-test-token", ""); rr.Code != 404 {
		t.Fatalf("disabled=%d", rr.Code)
	}
}

// The legacy persistence format cannot retain or restore checkpoint regions.
func TestCheckpointMemoryOnly(t *testing.T) {
	m := newMatrixSource(matrixTestConfig(), nil)
	obs := matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: checkpointPatch("safe")})
	m.checkpoints.ingest(obs, m.checkpoints.policyEpoch(), time.Now())
	restored := newMatrixSource(matrixTestConfig(), nil)
	if len(restored.checkpoints.since(0, 200).Regions) != 0 || restored.checkpoints.policyEpoch() == m.checkpoints.policyEpoch() {
		t.Fatal("new source restored checkpoint history")
	}
	// Compile-time assertion: the public observer still receives the existing
	// authoritative transport, with no extra patch collector or endpoint.
	var _ func(github.SentinelPatchObservation) = m.Observe
}

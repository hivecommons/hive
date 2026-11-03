package github

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

// withSandboxSpools points the relay at an owner-only manifest dir and
// returns it.
func withSandboxSpools(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sandbox-review-spools")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := sandboxReviewSpoolsDirForTest
	sandboxReviewSpoolsDirForTest = dir
	t.Cleanup(func() { sandboxReviewSpoolsDirForTest = old })
	return dir
}

// newSandboxSpool creates a kick workspace with its spool and registers it
// for agent, returning the spool and the manifest path.
func newSandboxSpool(t *testing.T, manifests, agent string, ended bool) (spool, manifest string) {
	t.Helper()
	workspace := t.TempDir()
	spool = filepath.Join(workspace, filepath.FromSlash(crux.SandboxReviewSpoolRelPath))
	if err := os.MkdirAll(spool, 0o770); err != nil {
		t.Fatal(err)
	}
	m := crux.SandboxReviewSpool{Agent: agent, Spool: spool, Key: testSpoolKey(agent), StartedAt: time.Now().UTC()}
	if ended {
		at := time.Now().UTC()
		m.EndedAt = &at
	}
	manifest = writeSandboxManifest(t, manifests, agent, m)
	return spool, manifest
}

// testSpoolKey is the spool key a test manifest for agent carries.
func testSpoolKey(agent string) string { return crux.SandboxReviewMAC("test-key", []byte(agent)) }

// writeSignedSpoolRequest writes req into spool as hive-review does inside a
// sandbox: the request JSON in an envelope MACed with key.
func writeSignedSpoolRequest(t *testing.T, spool, name, key string, req ReviewRequest) string {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(crux.SandboxReviewEnvelope{Request: string(raw), MAC: crux.SandboxReviewMAC(key, raw)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spool, name)
	if err := os.WriteFile(path, b, 0o660); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSandboxManifest(t *testing.T, manifests, agent string, m crux.SandboxReviewSpool) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manifests, agent+"-1.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A sandboxed reviewer's spooled comment is posted, attributed to the agent
// the MANIFEST names — not the one the file claims — and its result lands
// back in the spool where the sandbox can read it.
func TestSandboxSpool_PostsReviewAttributedToManifestAgent(t *testing.T) {
	reviewed := 0
	var apiEvent string
	srv := newReviewMockServer(t, &reviewed, &apiEvent)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	withReviewDir(t)
	manifests := withSandboxSpools(t)
	spool, _ := newSandboxSpool(t, manifests, "reviewer-spyre", false)

	var authzAgent string
	authzUID := -1
	c.reviewAuthz = func(agent string, uid int) error { authzAgent, authzUID = agent, uid; return nil }
	var auditAgent string
	c.SetAttributionAudit(func(action, detail, agent string) {
		if action == AuditActionPRReviewed {
			auditAgent = agent
		}
	})

	reqPath := writeSignedSpoolRequest(t, spool, "r.json", testSpoolKey("reviewer-spyre"), ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Body: "card run passed", Agent: "someone-else"})
	c.ProcessReviewRequestsOnce(context.Background())

	if reviewed != 1 || apiEvent != "COMMENT" {
		t.Fatalf("reviews posted = %d (event %q), want 1 COMMENT", reviewed, apiEvent)
	}
	if authzAgent != "reviewer-spyre" {
		t.Errorf("authorized as %q, want the manifest's agent reviewer-spyre", authzAgent)
	}
	if authzUID != 0 {
		t.Errorf("authorizer got uid %d, want 0: a staged file's owner proves nothing", authzUID)
	}
	if auditAgent != "reviewer-spyre" {
		t.Errorf("audited as %q, want reviewer-spyre", auditAgent)
	}
	if _, err := os.Stat(reqPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("request still in the spool after delivery: %v", err)
	}
	if resp := readReviewResult(t, reqPath); !resp.OK || resp.State != "commented" {
		t.Errorf("mirrored result = %+v, want ok commented", resp)
	}
}

// Relay policy is the same code path: a denial reaches the sandbox as a
// result file and nothing is posted.
func TestSandboxSpool_DenialIsMirroredAndNothingPosted(t *testing.T) {
	reviewed := 0
	srv := newReviewMockServer(t, &reviewed, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.reviewAuthz = func(agent string, uid int) error { return errors.New("not push-capable") }
	withReviewDir(t)
	manifests := withSandboxSpools(t)
	spool, _ := newSandboxSpool(t, manifests, "reviewer-spyre", false)

	reqPath := writeSignedSpoolRequest(t, spool, "r.json", testSpoolKey("reviewer-spyre"), ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Body: "x", Agent: "reviewer-spyre"})
	c.ProcessReviewRequestsOnce(context.Background())

	if reviewed != 0 {
		t.Fatalf("denied request posted %d reviews", reviewed)
	}
	resp := readReviewResult(t, reqPath)
	if resp.OK || !strings.Contains(resp.Error, "not push-capable") {
		t.Errorf("mirrored result = %+v, want the denial", resp)
	}
}

// The spool is group-writable, so another agent sharing hive's group can
// drop a request into it. Without a MAC under this run's key — unsigned, or
// signed with another run's key (a request copied from a different spool) —
// it is refused, set aside as .rejected and answered, and nothing is posted.
func TestSandboxSpool_RefusesRequestNotSignedForThisRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, spool string) string
	}{
		{"unsigned, as another agent would write it", func(t *testing.T, spool string) string {
			p, err := WriteReviewRequest(spool, ReviewRequest{Repo: "o/r", Number: 7, Event: "approve", Agent: "reviewer-spyre"})
			if err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"signed for another run", func(t *testing.T, spool string) string {
			return writeSignedSpoolRequest(t, spool, "r.json", testSpoolKey("other-run"), ReviewRequest{Repo: "o/r", Number: 7, Event: "approve"})
		}},
		{"MAC over different bytes", func(t *testing.T, spool string) string {
			raw, _ := json.Marshal(ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Body: "harmless"})
			forged, _ := json.Marshal(ReviewRequest{Repo: "o/r", Number: 7, Event: "approve"})
			b, _ := json.Marshal(crux.SandboxReviewEnvelope{Request: string(forged), MAC: crux.SandboxReviewMAC(testSpoolKey("reviewer-spyre"), raw)})
			p := filepath.Join(spool, "r.json")
			if err := os.WriteFile(p, b, 0o660); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reviewed := 0
			srv := newReviewMockServer(t, &reviewed, nil)
			defer srv.Close()
			c := reviewTestClient(t, srv.URL)
			authorized := false
			c.reviewAuthz = func(agent string, uid int) error { authorized = true; return nil }
			withReviewDir(t)
			manifests := withSandboxSpools(t)
			spool, _ := newSandboxSpool(t, manifests, "reviewer-spyre", false)

			reqPath := tc.write(t, spool)
			c.ProcessReviewRequestsOnce(context.Background())

			if reviewed != 0 || authorized {
				t.Fatalf("request not signed for this run reached the relay: %d reviews posted, authorized=%v", reviewed, authorized)
			}
			if _, err := os.Lstat(reqPath + ".rejected"); err != nil {
				t.Errorf("request should be set aside as .rejected: %v", err)
			}
			if resp := readReviewResult(t, reqPath); resp.OK || !strings.Contains(resp.Error, "rejected") {
				t.Errorf("result = %+v, want a rejection", resp)
			}
		})
	}
}

// A copy of a request this spool already accepted — the spool is
// group-readable — is a replay, refused even under a fresh name.
func TestSandboxSpool_RefusesReplayOfAcceptedRequest(t *testing.T) {
	reviewed := 0
	srv := newReviewMockServer(t, &reviewed, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.reviewAuthz = func(agent string, uid int) error { return nil }
	withReviewDir(t)
	manifests := withSandboxSpools(t)
	spool, _ := newSandboxSpool(t, manifests, "reviewer-spyre", false)

	req := ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Body: "card run passed"}
	first := writeSignedSpoolRequest(t, spool, "first.json", testSpoolKey("reviewer-spyre"), req)
	copied, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if reviewed != 1 {
		t.Fatalf("signed request: %d reviews posted, want 1", reviewed)
	}

	replay := filepath.Join(spool, "replay.json")
	if err := os.WriteFile(replay, copied, 0o660); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if reviewed != 1 {
		t.Fatalf("replayed request was posted: %d reviews, want 1", reviewed)
	}
	if _, err := os.Lstat(replay + ".rejected"); err != nil {
		t.Errorf("replay should be set aside as .rejected: %v", err)
	}
}

// The manifest is the whole of the trust: a manifest dir others can write is
// ignored, so nothing in it is delivered.
func TestSandboxSpool_IgnoresWritableManifestDir(t *testing.T) {
	reviewed := 0
	srv := newReviewMockServer(t, &reviewed, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	withReviewDir(t)
	manifests := withSandboxSpools(t)
	spool, _ := newSandboxSpool(t, manifests, "reviewer-spyre", false)
	if err := os.Chmod(manifests, 0o777); err != nil {
		t.Fatal(err)
	}
	reqPath, err := WriteReviewRequest(spool, ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if reviewed != 0 {
		t.Fatalf("world-writable manifest dir was trusted: %d reviews posted", reviewed)
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Errorf("request should be left untouched: %v", err)
	}
}

// The sandbox owns its workspace and can swap the spool for a symlink. The
// relay reads the spool through an os.Root at the workspace, so a link out
// of it is never followed: the request outside is neither posted nor moved.
func TestSandboxSpool_SymlinkedSpoolCannotEscapeWorkspace(t *testing.T) {
	reviewed := 0
	srv := newReviewMockServer(t, &reviewed, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	withReviewDir(t)
	manifests := withSandboxSpools(t)
	spool, _ := newSandboxSpool(t, manifests, "reviewer-spyre", false)

	outside := t.TempDir()
	victim, err := WriteReviewRequest(outside, ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(spool); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, spool); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if reviewed != 0 {
		t.Fatalf("relay followed the spool symlink out of the workspace: %d reviews posted", reviewed)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("file outside the workspace was moved: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(victim, ".json") + ".result.json"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("relay wrote a result outside the workspace: %v", err)
	}
}

// A request file that is itself a symlink is set aside, never read.
func TestSandboxSpool_RejectsSymlinkedRequest(t *testing.T) {
	reviewed := 0
	srv := newReviewMockServer(t, &reviewed, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	withReviewDir(t)
	manifests := withSandboxSpools(t)
	spool, _ := newSandboxSpool(t, manifests, "reviewer-spyre", false)

	target, err := WriteReviewRequest(filepath.Dir(spool), ReviewRequest{Repo: "o/r", Number: 7, Event: "comment", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(spool, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if reviewed != 0 {
		t.Fatalf("symlinked request was delivered: %d reviews posted", reviewed)
	}
	if _, err := os.Lstat(link + ".rejected"); err != nil {
		t.Errorf("symlinked request should be set aside as .rejected: %v", err)
	}
}

// Once the kick has ended and nothing is left to deliver, the manifest and
// its staging dir go; while the kick runs they stay.
func TestSandboxSpool_PrunesOnlyEndedAndDrainedManifests(t *testing.T) {
	c := reviewTestClient(t, "http://127.0.0.1:0")
	withReviewDir(t)
	manifests := withSandboxSpools(t)

	_, running := newSandboxSpool(t, manifests, "running", false)
	_, ended := newSandboxSpool(t, manifests, "ended", true)
	if !hasSandboxReviewSpools(manifests) {
		t.Fatal("hasSandboxReviewSpools = false with two manifests registered")
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if _, err := os.Stat(running); err != nil {
		t.Errorf("running kick's manifest was pruned: %v", err)
	}
	if _, err := os.Stat(ended); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ended, drained manifest was kept: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(ended, ".json") + ".d"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ended manifest's staging dir was kept: %v", err)
	}
}

// A manifest whose spool is not a workspace spool, or that outlived any
// kick, is dropped without reading anything.
func TestSandboxSpool_DropsInvalidAndStaleManifests(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		m    crux.SandboxReviewSpool
	}{
		{"no agent", crux.SandboxReviewSpool{Spool: "/data/agents/sandbox/x/.hive/review-requests", Key: testSpoolKey("a"), StartedAt: now}},
		{"not a workspace spool", crux.SandboxReviewSpool{Agent: "a", Spool: "/var/run/hive-metrics/review-requests", Key: testSpoolKey("a"), StartedAt: now}},
		{"relative", crux.SandboxReviewSpool{Agent: "a", Spool: "x/.hive/review-requests", Key: testSpoolKey("a"), StartedAt: now}},
		{"stale", crux.SandboxReviewSpool{Agent: "a", Spool: "/data/agents/sandbox/x/.hive/review-requests", Key: testSpoolKey("a"), StartedAt: now.Add(-sandboxReviewSpoolMaxAge - time.Hour)}},
		{"no spool key", crux.SandboxReviewSpool{Agent: "a", Spool: "/data/agents/sandbox/x/.hive/review-requests", StartedAt: now}},
		{"short spool key", crux.SandboxReviewSpool{Agent: "a", Spool: "/data/agents/sandbox/x/.hive/review-requests", Key: "0123456789abcdef", StartedAt: now}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := reviewTestClient(t, "http://127.0.0.1:0")
			withReviewDir(t)
			manifests := withSandboxSpools(t)
			path := writeSandboxManifest(t, manifests, "m", tc.m)
			if got := c.loadSandboxReviewSpools(now); len(got) != 0 {
				t.Fatalf("loaded %d spools, want 0", len(got))
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("invalid manifest was kept: %v", err)
			}
		})
	}
}

func TestHasSandboxReviewSpools_EmptyAndMissing(t *testing.T) {
	if hasSandboxReviewSpools(filepath.Join(t.TempDir(), "missing")) {
		t.Error("missing manifest dir reported spools")
	}
	empty := t.TempDir()
	if err := os.Mkdir(filepath.Join(empty, "staging.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if hasSandboxReviewSpools(empty) {
		t.Error("manifest dir holding only a staging dir reported spools")
	}
}

func TestHasSandboxReviewSpools_IgnoresUntrustedDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !hasSandboxReviewSpools(dir) {
		t.Fatal("trusted manifest dir with a manifest reported no spools")
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if hasSandboxReviewSpools(dir) {
		t.Error("world-writable manifest dir kept the fast poll cadence")
	}
}

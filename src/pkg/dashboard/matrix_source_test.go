package dashboard

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/sentinel"
)

const matrixTestRepo = "hivecommons/hive"

func matrixTestConfig() config.DashboardMatrixConfig {
	return config.DashboardMatrixConfig{Enabled: true, Repos: []string{matrixTestRepo}}
}

type matrixClock struct{ t time.Time }

func (c *matrixClock) now() time.Time          { return c.t }
func (c *matrixClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestMatrixSource(t *testing.T, cfg config.DashboardMatrixConfig) (*matrixSource, *matrixClock) {
	t.Helper()
	m := newMatrixSource(cfg, nil)
	clk := &matrixClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	m.now = clk.now
	return m, clk
}

func matrixObs(number int, head string, files ...sentinel.File) github.SentinelPatchObservation {
	return github.SentinelPatchObservation{
		Repo:           matrixTestRepo,
		Number:         number,
		HeadSHA:        head,
		Author:         "hive-bot",
		HiveIdentified: true,
		Files:          files,
	}
}

func matrixPatch(lines ...string) string {
	var b strings.Builder
	b.WriteString("--- a/x\n+++ b/x\n@@ -0,0 +1 @@\n")
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func matrixUniqueLines(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s line %d", prefix, i)
	}
	return out
}

func matrixAllLines(m *matrixSource) []matrixLine {
	lines, _ := m.Since(0, matrixBacklogCap+1)
	return lines
}

func TestMatrixSourceDropsSecretsFromFixturePatch(t *testing.T) {
	m, _ := newTestMatrixSource(t, matrixTestConfig())
	ghToken := "ghp_" + strings.Repeat("a1B2", 9)
	awsKey := "AKIA" + "IOSFODNN7EXAMPLE"
	scrubbed := "aws_key = " + awsKey
	paneToken := "key: sk-" + "abc123def"
	files := []sentinel.File{
		{Path: "pkg/client.go", Patch: matrixPatch(
			"func main() {",
			"token := \""+ghToken+"\"",
			scrubbed,
			paneToken,
			"note: [REDACTED] earlier",
			"-----BEGIN RSA PRIVATE KEY-----",
			"MIIEowIBAAKCAQEA",
			"abc",
			"-----END RSA PRIVATE KEY-----",
			"return nil",
		)},
		{Path: "certs/server.pem", Patch: matrixPatch("pem visible line")},
		{Path: ".env", Patch: matrixPatch("DB_HOST=localhost")},
		{Path: "deploy/secrets.yaml", Patch: matrixPatch("password: hunter2")},
	}
	if got := m.ingest(matrixObs(1, "h1", files...)); got != 2 {
		t.Fatalf("ingest added %d lines, want 2", got)
	}
	var got []string
	for _, l := range matrixAllLines(m) {
		got = append(got, l.Line)
		if l.Repo != matrixTestRepo || l.PR != 1 || l.Head != "h1" || l.Path != "pkg/client.go" || l.Lang != "go" || l.At.IsZero() {
			t.Errorf("unexpected line metadata: %+v", l)
		}
	}
	if strings.Join(got, "|") != "func main() {|return nil" {
		t.Fatalf("accepted lines = %q", got)
	}
	for _, l := range got {
		for _, bad := range []string{ghToken, awsKey, "sk-", "REDACTED", "PRIVATE KEY", "MIIE", "abc", "pem visible", "DB_HOST", "hunter2"} {
			if strings.Contains(l, bad) {
				t.Errorf("line %q leaks %q", l, bad)
			}
		}
	}
}

func TestMatrixSourceObservationGates(t *testing.T) {
	file := sentinel.File{Path: "main.go", Patch: matrixPatch("x := 1")}
	tests := []struct {
		name   string
		cfg    config.DashboardMatrixConfig
		mutate func(*github.SentinelPatchObservation)
		want   int
	}{
		{name: "accepted", cfg: matrixTestConfig(), want: 1},
		{name: "not hive identified", cfg: matrixTestConfig(), mutate: func(o *github.SentinelPatchObservation) { o.HiveIdentified = false }},
		{name: "private repo", cfg: matrixTestConfig(), mutate: func(o *github.SentinelPatchObservation) { o.Private = true }},
		{name: "repo not allowlisted", cfg: matrixTestConfig(), mutate: func(o *github.SentinelPatchObservation) { o.Repo = "other/repo" }},
		{name: "disabled", cfg: config.DashboardMatrixConfig{Repos: []string{matrixTestRepo}}},
		{name: "empty allowlist", cfg: config.DashboardMatrixConfig{Enabled: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestMatrixSource(t, tc.cfg)
			obs := matrixObs(7, "h", file)
			if tc.mutate != nil {
				tc.mutate(&obs)
			}
			m.Observe(obs)
			if tc.want == 0 && len(m.in) != 0 {
				t.Fatalf("Observe queued a rejected observation")
			}
			if got := m.ingest(obs); got != tc.want {
				t.Fatalf("ingest = %d, want %d", got, tc.want)
			}
			if got := len(matrixAllLines(m)); got != tc.want {
				t.Fatalf("backlog = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMatrixPathDenied(t *testing.T) {
	excludes := []string{"docs/*.md", "*.snap", "fixtures/**"}
	tests := []struct {
		path string
		want bool
	}{
		{"", true},
		{".env", true},
		{"config/.env.production", true},
		{"tls/server.pem", true},
		{"tls/server.KEY", true},
		{"cert.p12", true},
		{"home/.ssh/id_ed25519", true},
		{"deploy/Secrets.yaml", true},
		{"pkg/credentials/store.go", true},
		{"auth/token_cache.go", true},
		{"go.sum", true},
		{"web/package-lock.json", true},
		{"Cargo.lock", true},
		{"vendor/x/y.go", true},
		{"web/node_modules/a/b.js", true},
		{"static/app.min.js", true},
		{"api/zz_generated.deepcopy.go", true},
		{"proto/api.pb.go", true},
		{"pkg/model_gen.go", true},
		{"docs/guide.md", true},
		{"ui/__snapshots__/x.snap", true},
		{"fixtures/a/b.txt", true},
		{"src/pkg/dashboard/server.go", false},
		{"docs/sub/guide.md", false},
		{"README.md", false},
		{"pkg/vendor.go", false},
	}
	for _, tc := range tests {
		if got := matrixPathDenied(tc.path, append(excludes, " ")); got != tc.want {
			t.Errorf("matrixPathDenied(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestMatrixAcceptLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "plain", in: "  return x + y  ", want: "return x + y", ok: true},
		{name: "empty", in: "   "},
		{name: "invalid utf8", in: "bad \xff byte"},
		{name: "control char", in: "esc \x1b[31m red"},
		{name: "tab allowed", in: "a\tb", want: "a\tb", ok: true},
		{name: "exactly 160 runes", in: strings.Repeat("ab ", 53) + "x", want: strings.Repeat("ab ", 53) + "x", ok: true},
		{name: "over 160 runes dropped not truncated", in: strings.Repeat("ab ", 54)},
		{name: "entropy run", in: "sum := " + strings.Repeat("0f", 16)},
		{name: "entropy run 31 ok", in: "id " + strings.Repeat("a", 31), want: "id " + strings.Repeat("a", 31), ok: true},
		{name: "redaction marker", in: "value <redacted> here"},
		{name: "scrub changes", in: "key AKIA" + "IOSFODNN7EXAMPLE end"},
		{name: "yaml password", in: "password: hunter2"},
		{name: "env api key", in: "API_KEY=abc123"},
		{name: "quoted secret", in: `secret: "x"`},
		{name: "json password", in: `"password": "hunter2"`},
		{name: "json api key compact", in: `"api_key":"abc"`},
		{name: "php password", in: `'password' => 'hunter2'`},
		{name: "ruby symbol token", in: `token: "abc"`},
		{name: "go short declare kept", in: "token := parse(r)", want: "token := parse(r)", ok: true},
		{name: "comment kept", in: "// the token is refreshed hourly", want: "// the token is refreshed hourly", ok: true},
		{name: "identifier kept", in: "tokenize(line)", want: "tokenize(line)", ok: true},
		{name: "pane scrub changes", in: "use sk-" + "abcdef12 now"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matrixAcceptLine(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("matrixAcceptLine(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestMatrixAddedLines(t *testing.T) {
	if got := matrixAddedLines(""); got != nil {
		t.Fatalf("empty patch = %q", got)
	}
	if got := matrixAddedLines("+a\x00b"); got != nil {
		t.Fatalf("binary patch = %q", got)
	}
	got := matrixAddedLines("+++ b/f\n@@\n context\n-removed\n+added\n+-----BEGIN X-----\n+body\n+-----END X-----\n+after")
	if strings.Join(got, "|") != "added|after" {
		t.Fatalf("added lines = %q", got)
	}
}

func TestMatrixLang(t *testing.T) {
	for path, want := range map[string]string{"a/b.go": "go", "x.TSX": "typescript", "Makefile": "", "s.yml": "yaml"} {
		if got := matrixLang(path); got != want {
			t.Errorf("matrixLang(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestMatrixSourceDedup(t *testing.T) {
	m, clk := newTestMatrixSource(t, matrixTestConfig())
	file := sentinel.File{Path: "a.go", Patch: matrixPatch("one", "two", "one")}
	if got := m.ingest(matrixObs(1, "h1", file)); got != 2 {
		t.Fatalf("first ingest = %d, want 2", got)
	}
	if got := m.ingest(matrixObs(1, "h1", file)); got != 0 {
		t.Fatalf("same head re-observed added %d", got)
	}
	rebased := sentinel.File{Path: "a.go", Patch: matrixPatch("one", "two", "three")}
	if got := m.ingest(matrixObs(1, "h2", rebased)); got != 1 {
		t.Fatalf("rebased head added %d, want 1 new line", got)
	}
	other := sentinel.File{Path: "b.go", Patch: matrixPatch("one")}
	if got := m.ingest(matrixObs(1, "h2", other)); got != 1 {
		t.Fatalf("same line in another file added %d, want 1", got)
	}
	if got := m.ingest(matrixObs(2, "h1", file)); got != 2 {
		t.Fatalf("another PR added %d, want 2", got)
	}
	clk.advance(matrixDedupTTL + time.Minute)
	if got := m.ingest(matrixObs(3, "h", sentinel.File{Path: "c.go", Patch: matrixPatch("z")})); got != 1 {
		t.Fatalf("fresh PR added %d", got)
	}
	if _, ok := m.prs[matrixTestRepo+"#1"]; ok {
		t.Fatalf("PR set older than the TTL was not pruned")
	}
	if m.totalHashes != 1 {
		t.Fatalf("totalHashes = %d after prune, want 1", m.totalHashes)
	}
	if got := m.ingest(matrixObs(1, "h2", file)); got != 2 {
		t.Fatalf("after TTL expiry re-observed head added %d, want 2", got)
	}
}

func TestMatrixSourcePerHeadCap(t *testing.T) {
	m, _ := newTestMatrixSource(t, matrixTestConfig())
	files := []sentinel.File{
		{Path: "a.go", Patch: matrixPatch(matrixUniqueLines("a", 150)...)},
		{Path: "b.go", Patch: matrixPatch(matrixUniqueLines("b", 150)...)},
	}
	if got := m.ingest(matrixObs(1, "h1", files...)); got != matrixMaxLinesPerHead {
		t.Fatalf("ingest = %d, want %d", got, matrixMaxLinesPerHead)
	}
	if got := m.ingest(matrixObs(1, "h1", sentinel.File{Path: "c.go", Patch: matrixPatch("more")})); got != 0 {
		t.Fatalf("head over its cap still added %d", got)
	}
	if got := m.ingest(matrixObs(1, "h2", sentinel.File{Path: "c.go", Patch: matrixPatch("more")})); got != 1 {
		t.Fatalf("new head added %d, want 1", got)
	}
}

func TestMatrixSourceRingBound(t *testing.T) {
	m, clk := newTestMatrixSource(t, matrixTestConfig())
	for pr := 1; pr <= 15; pr++ {
		clk.advance(matrixRateWindow)
		m.ingest(matrixObs(pr, "h", sentinel.File{Path: "a.go", Patch: matrixPatch(matrixUniqueLines(fmt.Sprint(pr), matrixMaxLinesPerHead)...)}))
	}
	lines := matrixAllLines(m)
	if len(lines) != matrixBacklogCap {
		t.Fatalf("backlog = %d, want %d", len(lines), matrixBacklogCap)
	}
	if lines[0].Seq != 1001 || lines[len(lines)-1].Seq != 3000 || m.Latest() != 3000 {
		t.Fatalf("ring order wrong: first=%d last=%d latest=%d", lines[0].Seq, lines[len(lines)-1].Seq, m.Latest())
	}
	page, latest := m.Since(2990, 0)
	if len(page) != 10 || page[0].Seq != 2991 || latest != 3000 {
		t.Fatalf("Since(2990) = %d lines from %d, latest %d", len(page), page[0].Seq, latest)
	}
	page, _ = m.Since(0, 5)
	if len(page) != 5 || page[0].Seq != 1001 {
		t.Fatalf("Since(0, 5) = %d lines from %d", len(page), page[0].Seq)
	}
	page, _ = m.Since(0, -1)
	if len(page) != matrixDefaultSinceMax {
		t.Fatalf("Since default page = %d", len(page))
	}
}

func TestMatrixSourceRateLimit(t *testing.T) {
	m, clk := newTestMatrixSource(t, matrixTestConfig())
	total := 0
	for pr := 1; pr <= 4; pr++ {
		total += m.ingest(matrixObs(pr, "h", sentinel.File{Path: "a.go", Patch: matrixPatch(matrixUniqueLines(fmt.Sprint(pr), 175)...)}))
	}
	if total != matrixMaxLinesPerRate || m.rateDropped != 1 {
		t.Fatalf("accepted %d (dropped %d), want %d", total, m.rateDropped, matrixMaxLinesPerRate)
	}
	clk.advance(matrixRateWindow)
	if got := m.ingest(matrixObs(5, "h", sentinel.File{Path: "a.go", Patch: matrixPatch("next window")})); got != 1 {
		t.Fatalf("next window accepted %d", got)
	}
}

func TestMatrixSourceDedupBounds(t *testing.T) {
	m, clk := newTestMatrixSource(t, matrixTestConfig())
	for pr := 1; pr <= matrixMaxTrackedPRs+20; pr++ {
		clk.advance(time.Second)
		m.ingest(matrixObs(pr, "h", sentinel.File{Path: "a.go", Patch: matrixPatch(fmt.Sprintf("pr %d", pr))}))
	}
	if len(m.prs) != matrixMaxTrackedPRs {
		t.Fatalf("tracked PRs = %d, want %d", len(m.prs), matrixMaxTrackedPRs)
	}
	if _, ok := m.prs[matrixTestRepo+"#1"]; ok {
		t.Fatalf("oldest PR set was not evicted")
	}

	old := matrixMaxDedupHashes
	matrixMaxDedupHashes = 50
	t.Cleanup(func() { matrixMaxDedupHashes = old })
	m2, clk2 := newTestMatrixSource(t, matrixTestConfig())
	for pr := 1; pr <= 4; pr++ {
		clk2.advance(time.Second)
		m2.ingest(matrixObs(pr, "h", sentinel.File{Path: "a.go", Patch: matrixPatch(matrixUniqueLines(fmt.Sprint(pr), 20)...)}))
	}
	if m2.totalHashes > matrixMaxDedupHashes || len(m2.prs) != 2 {
		t.Fatalf("hash cap not held: total=%d prs=%d", m2.totalHashes, len(m2.prs))
	}
	m3, _ := newTestMatrixSource(t, matrixTestConfig())
	m3.ingest(matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: matrixPatch(matrixUniqueLines("x", 60)...)}))
	if m3.totalHashes != 0 || len(m3.prs) != 0 {
		t.Fatalf("single over-cap PR kept: total=%d prs=%d", m3.totalHashes, len(m3.prs))
	}
}

func TestMatrixSourceSkipsOversizedInput(t *testing.T) {
	m, _ := newTestMatrixSource(t, matrixTestConfig())
	files := []sentinel.File{{Path: "big.go", Patch: matrixPatch(strings.Repeat("x ", matrixMaxPatchBytes))}}
	for i := 0; i < matrixMaxFilesPerObs; i++ {
		files = append(files, sentinel.File{Path: fmt.Sprintf("f%d.go", i)})
	}
	files = append(files, sentinel.File{Path: "late.go", Patch: matrixPatch("beyond the file cap")})
	if got := m.ingest(matrixObs(1, "h", files...)); got != 0 {
		t.Fatalf("oversized input added %d lines", got)
	}
}

func TestMatrixSourceObserveNeverBlocks(t *testing.T) {
	m, _ := newTestMatrixSource(t, matrixTestConfig())
	obs := matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: matrixPatch("x")})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < matrixIngestCap+10; i++ {
			m.Observe(obs)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Observe blocked on a full channel")
	}
	if got := m.Dropped(); got != 10 {
		t.Fatalf("Dropped() = %d, want 10", got)
	}
	if len(m.in) != matrixIngestCap {
		t.Fatalf("channel holds %d, want %d", len(m.in), matrixIngestCap)
	}
}

func TestMatrixSourceRunNotifiesAndRecovers(t *testing.T) {
	m, _ := newTestMatrixSource(t, matrixTestConfig())
	seqs := make(chan uint64, 4)
	calls := 0
	m.setNotify(func(seq uint64) {
		calls++
		if calls == 1 {
			panic("notify boom")
		}
		seqs <- seq
	})
	m.start()
	m.start()
	m.Observe(matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: matrixPatch("first")}))
	m.Observe(matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: matrixPatch("second")}))
	select {
	case seq := <-seqs:
		if seq != 2 {
			t.Fatalf("notify seq = %d, want 2", seq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("processing goroutine did not survive a panic")
	}
	m.close()
	m.close()
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
		t.Fatal("processing goroutine did not stop")
	}
}

func TestServerMatrixObserver(t *testing.T) {
	var nilSrv *Server
	if nilSrv.MatrixObserver(matrixTestConfig()) != nil {
		t.Fatal("nil server returned an observer")
	}
	s := &Server{}
	if s.MatrixObserver(config.DashboardMatrixConfig{}) != nil || s.matrix != nil {
		t.Fatal("disabled matrix registered an observer or built a source")
	}
	if s.MatrixObserver(config.DashboardMatrixConfig{Enabled: true}) != nil || s.matrix != nil {
		t.Fatal("matrix without an allowlist registered an observer")
	}
	obs := s.MatrixObserver(matrixTestConfig())
	if obs == nil || s.matrix == nil {
		t.Fatal("active matrix returned no observer")
	}
	src := s.matrix
	t.Cleanup(src.close)
	widened := matrixTestConfig()
	widened.Repos = append(widened.Repos, "hivecommons/other")
	if s.MatrixObserver(widened) == nil || s.matrix != src {
		t.Fatal("second call rebuilt the source")
	}
	if !src.cfg.Load().RepoAllowed("hivecommons/other") {
		t.Fatal("second call did not refresh the allowlist")
	}
	src.ingest(matrixObs(1, "h", sentinel.File{Path: "a.go", Patch: matrixPatch("kept")}))
	if src.Latest() != 1 {
		t.Fatalf("Latest() = %d, want 1", src.Latest())
	}
	if s.MatrixObserver(config.DashboardMatrixConfig{}) != nil {
		t.Fatal("disabling still returned an observer")
	}
	if lines, latest := src.Since(0, 10); len(lines) != 0 || latest != 1 || len(src.prs) != 0 {
		t.Fatalf("disabling kept content: %d lines, latest %d, %d PRs", len(lines), latest, len(src.prs))
	}
	if got := src.ingest(matrixObs(2, "h", sentinel.File{Path: "a.go", Patch: matrixPatch("late")})); got != 0 {
		t.Fatalf("disabled source still ingested %d lines", got)
	}
}

func TestMatrixSourceExpiredHashInLivePR(t *testing.T) {
	m, clk := newTestMatrixSource(t, matrixTestConfig())
	m.ingest(matrixObs(1, "h1", sentinel.File{Path: "a.go", Patch: matrixPatch("a")}))
	clk.advance(matrixDedupTTL - time.Hour)
	m.ingest(matrixObs(1, "h2", sentinel.File{Path: "a.go", Patch: matrixPatch("b")}))
	clk.advance(2 * time.Hour)
	if got := m.ingest(matrixObs(1, "h3", sentinel.File{Path: "a.go", Patch: matrixPatch("a", "b")})); got != 1 {
		t.Fatalf("expired hash re-ingest added %d, want 1", got)
	}
	if m.totalHashes != 2 {
		t.Fatalf("totalHashes = %d, want 2", m.totalHashes)
	}
}

func TestServerCloseStopsMatrixSource(t *testing.T) {
	s := &Server{}
	if s.MatrixObserver(matrixTestConfig()) == nil {
		t.Fatal("active matrix returned no observer")
	}
	src := s.matrix
	s.Close()
	select {
	case <-src.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Server.Close did not stop the matrix goroutine")
	}
	s.Close()
	(&Server{}).Close()
}

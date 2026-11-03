package kubejob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// podLogAPI is a fake API server for the read-only pod observation path: a
// scripted pod list, one pod object, and a log that is delivered in chunks
// (each flushed) so a followed stream can be observed line by line.
type podLogAPI struct {
	mu        sync.Mutex
	pods      string // JSON list body
	pod       string // JSON pod body
	logChunks []string
	logStatus int
	selectors []string
	followed  []bool
	// release, when non-nil, gates the final log chunk so a test can observe
	// a followed stream mid-flight without sleeping.
	release chan struct{}
}

func (f *podLogAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pods"):
			f.mu.Lock()
			f.selectors = append(f.selectors, r.URL.Query().Get("labelSelector"))
			body := f.pods
			f.mu.Unlock()
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/log"):
			f.mu.Lock()
			f.followed = append(f.followed, r.URL.Query().Get("follow") == "true")
			chunks, status, release := f.logChunks, f.logStatus, f.release
			f.mu.Unlock()
			if status > 0 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"kind":"Status","message":"pods \"x\" is forbidden"}`))
				return
			}
			fl, _ := w.(http.Flusher)
			for i, c := range chunks {
				if release != nil && i == len(chunks)-1 {
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				_, _ = w.Write([]byte(c))
				if fl != nil {
					fl.Flush()
				}
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pods/"):
			f.mu.Lock()
			body := f.pod
			f.mu.Unlock()
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

const (
	podRunning = `{"metadata":{"name":"hive-kick-deps-1700000000-abcde","labels":{"job-name":"hive-kick-deps-1700000000"},"creationTimestamp":"2023-11-14T22:13:20Z"},"status":{"phase":"Running","containerStatuses":[{"name":"kick","state":{"running":{}}}]}}`
	podFailed  = `{"metadata":{"name":"hive-kick-deps-1700000900-zzzzz","labels":{"job-name":"hive-kick-deps-1700000900"},"creationTimestamp":"2023-11-14T22:28:20Z"},"status":{"phase":"Failed","containerStatuses":[{"name":"kick","state":{"terminated":{"exitCode":3}}}]}}`
	podWaiting = `{"metadata":{"name":"hive-kick-deps-1700001000-wwwww","labels":{"job-name":"hive-kick-deps-1700001000"},"creationTimestamp":"2023-11-14T22:30:00Z"},"status":{"phase":"Pending","containerStatuses":[{"name":"kick","state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}`
)

func newPodLogLauncher(srv *httptest.Server) *Launcher {
	return newLauncher(srv, Options{})
}

func TestAgentLabelSelectorMatchesManifestLabels(t *testing.T) {
	// The selector must find exactly what manifest() stamps on the pod, or
	// the terminal pane will never see a running kick.
	l := &Launcher{Options: Options{WorkspaceClaim: "hive-data"}}
	m := l.manifest("hive", "hive-kick-deps-1", "sandbox/deps-1", specFor("/data/sandbox/deps-1"))
	tmpl := m["spec"].(map[string]any)["template"].(map[string]any)
	labels := tmpl["metadata"].(map[string]any)["labels"].(map[string]string)
	sel := AgentLabelSelector("hive-deps-sandbox")
	for _, kv := range strings.Split(sel, ",") {
		k, v, _ := strings.Cut(kv, "=")
		if labels[k] != v {
			t.Fatalf("selector %q wants %s=%s but the manifest stamps %s=%q", sel, k, v, k, labels[k])
		}
	}
	if got := AgentLabelValue("Hive-Deps_Quality-sandbox"); got != "hive-deps-quality-sandbox" {
		t.Fatalf("AgentLabelValue = %q", got)
	}
}

func TestLatestPodPicksNewestAndEscapesSelector(t *testing.T) {
	api := &podLogAPI{pods: `{"items":[` + podRunning + `,` + podFailed + `,` + podWaiting + `]}`}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newPodLogLauncher(srv)

	info, ok, err := l.LatestPod(context.Background(), "hive-deps-sandbox")
	if err != nil || !ok {
		t.Fatalf("LatestPod: ok=%v err=%v", ok, err)
	}
	if info.Name != "hive-kick-deps-1700001000-wwwww" || info.Phase != PodPhasePending || info.Waiting != "ImagePullBackOff" {
		t.Fatalf("LatestPod picked %+v, want the newest (waiting) pod", info)
	}
	if info.Job != "hive-kick-deps-1700001000" || info.Terminated || info.ExitCode != ExitCodeUnknown || info.Finished() {
		t.Fatalf("unexpected pod info %+v", info)
	}
	api.mu.Lock()
	sel := api.selectors[0]
	api.mu.Unlock()
	if sel != AgentLabelSelector("hive-deps-sandbox") {
		t.Fatalf("selector not round-tripped through query escaping: %q", sel)
	}
	if _, err := url.ParseQuery("labelSelector=" + url.QueryEscape(sel)); err != nil {
		t.Fatalf("selector not URL-safe: %v", err)
	}
}

func TestLatestPodNoneAndErrors(t *testing.T) {
	api := &podLogAPI{pods: `{"items":[]}`}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newPodLogLauncher(srv)
	if _, ok, err := l.LatestPod(context.Background(), "hive-deps-sandbox"); ok || err != nil {
		t.Fatalf("empty list: ok=%v err=%v", ok, err)
	}

	api.mu.Lock()
	api.pods = `not json`
	api.mu.Unlock()
	if _, _, err := l.LatestPod(context.Background(), "hive-deps-sandbox"); err == nil {
		t.Fatal("malformed list must be an error, not an empty pane")
	}

	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","message":"pods is forbidden: missing pods list"}`))
	}))
	defer forbidden.Close()
	if _, _, err := newPodLogLauncher(forbidden).LatestPod(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "missing pods list") {
		t.Fatalf("RBAC failure must carry the API message, got %v", err)
	}

	noNS := &Launcher{APIServer: srv.URL, HTTPClient: srv.Client(), NamespacePath: "/nonexistent/ns"}
	if _, _, err := noNS.LatestPod(context.Background(), "x"); err == nil {
		t.Fatal("missing namespace must be an error")
	}
}

func TestPodStatusReadsExitCode(t *testing.T) {
	api := &podLogAPI{pod: podFailed}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	info, err := newPodLogLauncher(srv).PodStatus(context.Background(), "hive-kick-deps-1700000900-zzzzz")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Terminated || info.ExitCode != 3 || !info.Finished() || info.Phase != PodPhaseFailed {
		t.Fatalf("PodStatus = %+v", info)
	}
	api.mu.Lock()
	api.pod = `{}`
	api.mu.Unlock()
	if _, err := newPodLogLauncher(srv).PodStatus(context.Background(), "x"); err == nil {
		t.Fatal("nameless pod must be an error")
	}
}

func TestStreamLogWithoutFollowReturnsAtEOF(t *testing.T) {
	api := &podLogAPI{logChunks: []string{"line one\n", "line two\n", "tail without newline"}}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	var out bytes.Buffer
	if err := newPodLogLauncher(srv).StreamLog(context.Background(), "p", false, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "line one\nline two\ntail without newline" {
		t.Fatalf("stream = %q", out.String())
	}
	api.mu.Lock()
	followed := api.followed
	api.mu.Unlock()
	if len(followed) != 1 || followed[0] {
		t.Fatalf("follow must not be requested, got %v", followed)
	}
}

// lineRecorder is a FlushWriter that reports each flush, so a test can wait
// for a specific line to have been delivered instead of sleeping.
type lineRecorder struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	flushed chan string
}

func (r *lineRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *lineRecorder) Flush() {
	r.mu.Lock()
	s := r.buf.String()
	r.mu.Unlock()
	r.flushed <- s
}

func TestStreamLogFollowDeliversLinesAsTheyArrive(t *testing.T) {
	release := make(chan struct{})
	api := &podLogAPI{logChunks: []string{"starting\n", "partial", " line\n", "done\n"}, release: release}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()

	rec := &lineRecorder{flushed: make(chan string, 16)}
	lw := &LineFilterWriter{Dst: rec, Fn: strings.ToUpper}
	errc := make(chan error, 1)
	go func() {
		err := newPodLogLauncher(srv).StreamLog(context.Background(), "p", true, lw)
		errc <- errors.Join(err, lw.Close())
	}()

	// Two complete lines are flushed while the server still holds the last
	// chunk back: the stream is live, not buffered to the end.
	wantSeq := []string{"STARTING\n", "STARTING\nPARTIAL LINE\n"}
	for _, want := range wantSeq {
		select {
		case got := <-rec.flushed:
			if got != want {
				t.Fatalf("flushed %q, want %q", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for a flushed line")
		}
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	final := rec.buf.String()
	rec.mu.Unlock()
	if final != "STARTING\nPARTIAL LINE\nDONE\n" {
		t.Fatalf("final = %q", final)
	}
	api.mu.Lock()
	followed := api.followed
	api.mu.Unlock()
	if len(followed) != 1 || !followed[0] {
		t.Fatalf("follow=true must be requested, got %v", followed)
	}
}

func TestStreamLogContextCancelEndsFollow(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	api := &podLogAPI{logChunks: []string{"starting\n", "never\n"}, release: release}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()

	rec := &lineRecorder{flushed: make(chan string, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- newPodLogLauncher(srv).StreamLog(ctx, "p", true, &LineFilterWriter{Dst: rec})
	}()
	select {
	case <-rec.flushed:
	case <-time.After(10 * time.Second):
		t.Fatal("first line never arrived")
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StreamLog did not return after cancel")
	}
}

func TestStreamLogErrors(t *testing.T) {
	api := &podLogAPI{logStatus: http.StatusForbidden}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	err := newPodLogLauncher(srv).StreamLog(context.Background(), "p", false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("err = %v", err)
	}

	noNS := &Launcher{APIServer: srv.URL, HTTPClient: srv.Client(), NamespacePath: "/nonexistent/ns"}
	if err := noNS.StreamLog(context.Background(), "p", false, io.Discard); err == nil {
		t.Fatal("missing namespace must be an error")
	}
	noCA := &Launcher{APIServer: srv.URL, Namespace: "hive", CAPath: "/nonexistent/ca.crt"}
	if err := noCA.StreamLog(context.Background(), "p", false, io.Discard); err == nil {
		t.Fatal("missing CA must be an error")
	}

	failing := &LineFilterWriter{Dst: failWriter{}}
	api2 := &podLogAPI{logChunks: []string{"x\n"}}
	srv2 := httptest.NewServer(api2.handler(t))
	defer srv2.Close()
	if err := newPodLogLauncher(srv2).StreamLog(context.Background(), "p", false, failing); err == nil {
		t.Fatal("a failing destination must end the stream with an error")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestLineFilterWriterBuffersPartialLines(t *testing.T) {
	var out bytes.Buffer
	w := &LineFilterWriter{Dst: &out, Fn: func(s string) string { return strings.ReplaceAll(s, "secret", "***") }}
	for _, chunk := range []string{"sec", "ret one\nsec", "ret two"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != "*** one\n" {
		t.Fatalf("before Close = %q", out.String())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "*** one\n*** two" {
		t.Fatalf("after Close = %q", out.String())
	}
	if err := w.Close(); err != nil {
		t.Fatal("second Close must be a no-op")
	}
}

func TestParsePodDefaults(t *testing.T) {
	info, err := parsePod([]byte(`{"metadata":{"name":"p"},"status":{"phase":"Succeeded"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.Terminated || info.ExitCode != ExitCodeUnknown || !info.Finished() || !info.Created.IsZero() {
		t.Fatalf("info = %+v", info)
	}
	if _, err := parsePod([]byte(`nope`)); err == nil {
		t.Fatal("malformed pod must be an error")
	}
}

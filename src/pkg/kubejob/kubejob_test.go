package kubejob

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/sandbox"
)

// fakeAPI is a minimal in-cluster API server: it records the Job manifest it
// was sent, serves a scripted sequence of Job statuses, one pod, its log and
// its terminated exit code, and records deletes.
type fakeAPI struct {
	mu        sync.Mutex
	created   map[string]any
	statuses  []string // JSON bodies served to successive GET job calls
	getCalls  int
	deleted   bool
	podName   string
	exitCode  int
	noPod     bool
	logBody   string
	podStatus string // "terminated" or "running"
	failCode  int    // when >0, every request fails with this status
	createErr int    // when >0, POST fails with this status
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{podName: "hive-kick-deps-1-abcde", logBody: "compiled\n", podStatus: "terminated"}
}

func (f *fakeAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failCode > 0 {
			w.WriteHeader(f.failCode)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"jobs.batch is forbidden: User \"system:serviceaccount:hive:hive\" cannot create resource \"jobs\""}`))
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
			if f.createErr > 0 {
				w.WriteHeader(f.createErr)
				_, _ = w.Write([]byte("nope"))
				return
			}
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Errorf("manifest is not JSON: %v", err)
			}
			f.created = m
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(body)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/jobs/"):
			i := f.getCalls
			if i >= len(f.statuses) {
				i = len(f.statuses) - 1
			}
			f.getCalls++
			_, _ = w.Write([]byte(f.statuses[i]))
		case r.Method == http.MethodDelete:
			f.deleted = true
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Success"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pods"):
			if f.noPod {
				_, _ = w.Write([]byte(`{"items":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"` + f.podName + `"}}]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/log"):
			_, _ = w.Write([]byte(f.logBody))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pods/"):
			state := `{"terminated":{"exitCode":` + itoa(f.exitCode) + `}}`
			if f.podStatus != "terminated" {
				state = `{"running":{}}`
			}
			_, _ = w.Write([]byte(`{"status":{"containerStatuses":[{"name":"kick","state":` + state + `}]}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

const (
	statusPending  = `{"status":{"active":1}}`
	statusComplete = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
	statusFailed   = `{"status":{"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded","message":"Job has reached the specified backoff limit"}]}}`
)

func newLauncher(srv *httptest.Server, opts Options) *Launcher {
	if opts.WorkspaceClaim == "" {
		opts.WorkspaceClaim = "hive-data"
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Millisecond
	}
	return &Launcher{
		Options:    opts,
		APIServer:  srv.URL,
		Namespace:  "hive",
		HTTPClient: srv.Client(),
		TokenPath:  filepath.Join("/nonexistent", "token"),
		Now:        func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func specFor(workspace string) sandbox.LaunchSpec {
	return sandbox.LaunchSpec{
		Image:     "registry.example/dev:latest",
		Name:      "hive-deps-sandbox",
		Workspace: workspace,
		Command:   []string{"claude", "-p", "@.hive/kick-prompt.txt"},
		Env:       map[string]string{"ZED": "1", "ALPHA": "a", "GITHUB_TOKEN": "leak", "MY_SECRET": "leak"},
	}
}

func TestRunSuccessCollectsLogAndExitCodeAndDeletes(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusPending, statusPending, statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})

	res, err := l.Run(context.Background(), specFor("/data/agents/sandbox/deps-123"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "compiled\n" || res.ExitCode != 0 {
		t.Errorf("result = %+v, want the pod log and exit 0", res)
	}
	if !api.deleted {
		t.Error("a completed Job must be deleted")
	}
	if api.getCalls < 3 {
		t.Errorf("job status polled %d times, want the scripted 3", api.getCalls)
	}
}

func TestRunFailedJobIsLeftForInspection(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusFailed}
	api.exitCode = 3
	api.logBody = "error: no device\n"
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})

	res, err := l.Run(context.Background(), specFor("/data/agents/sandbox/deps-123"))
	if err == nil || !strings.Contains(err.Error(), "BackoffLimitExceeded") {
		t.Fatalf("Run error = %v, want the Job's failure reason", err)
	}
	if res.ExitCode != 3 || res.Stdout != "error: no device\n" {
		t.Errorf("failed result = %+v, want exit 3 and the log", res)
	}
	if !strings.Contains(res.Stderr, "backoff limit") {
		t.Errorf("Stderr = %q, want the condition message", res.Stderr)
	}
	if api.deleted {
		t.Error("a failed Job must be left in place until its TTL")
	}
}

func TestRunContextTimeoutDeletesTheJob(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusPending}
	api.podStatus = "running"
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	res, err := l.Run(ctx, specFor("/data/agents/sandbox/deps-123"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want deadline exceeded", err)
	}
	if !api.deleted {
		t.Error("a timed-out Job must be deleted so the workload stops")
	}
	if res.ExitCode != exitCodeUnknown {
		t.Errorf("ExitCode = %d, want unknown for a still-running pod", res.ExitCode)
	}
	if res.Stdout != "compiled\n" {
		t.Errorf("partial log must still be collected, got %q", res.Stdout)
	}
}

func TestRunCompleteWithNoPodStillSucceeds(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusComplete}
	api.noPod = true
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})
	res, err := l.Run(context.Background(), specFor("/data/agents/sandbox/deps-123"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "" {
		t.Errorf("result = %+v, want exit 0 (Complete) and no log", res)
	}
}

func TestRunCreateFailureCarriesAPIMessage(t *testing.T) {
	api := newFakeAPI()
	api.failCode = http.StatusForbidden
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})
	_, err := l.Run(context.Background(), specFor("/data/agents/sandbox/deps-123"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "cannot create resource") {
		t.Fatalf("Run error = %v, want HTTP 403 with the API's message", err)
	}
}

func TestRunCreateFailureNonStatusBody(t *testing.T) {
	api := newFakeAPI()
	api.createErr = http.StatusInternalServerError
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})
	_, err := l.Run(context.Background(), specFor("/data/agents/sandbox/deps-123"))
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Run error = %v, want the raw body", err)
	}
}

func TestRunTransientPollErrorKeepsWaiting(t *testing.T) {
	// First GET job returns garbage (parse failure), then Complete.
	api := newFakeAPI()
	api.statuses = []string{"{not json", statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{})
	if _, err := l.Run(context.Background(), specFor("/data/agents/sandbox/deps-123")); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request expected for an invalid launch, got %s %s", r.Method, r.URL)
	}))
	defer srv.Close()
	cases := []struct {
		name string
		opts Options
		spec sandbox.LaunchSpec
		want string
	}{
		{"no claim", Options{WorkspaceClaim: " "}, specFor("/data/x"), "workspace_claim"},
		{"negative ttl", Options{WorkspaceClaim: "c", TTLSeconds: -1}, specFor("/data/x"), "ttl_seconds"},
		{"bad volume", Options{WorkspaceClaim: "c", ExtraVolumes: []VolumeMount{{Claim: "x"}}}, specFor("/data/x"), "volumes[0]"},
		{"no image", Options{WorkspaceClaim: "c"}, sandbox.LaunchSpec{Workspace: "/data/x"}, "image"},
		{"no workspace", Options{WorkspaceClaim: "c"}, sandbox.LaunchSpec{Image: "i"}, "workspace is required"},
		{"workspace outside claim", Options{WorkspaceClaim: "c"}, specFor("/tmp/elsewhere"), "not under the workspace claim mount"},
		{"workspace is the mount itself", Options{WorkspaceClaim: "c"}, specFor("/data"), "not under the workspace claim mount"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &Launcher{Options: tc.opts, APIServer: srv.URL, Namespace: "hive", HTTPClient: srv.Client()}
			_, err := l.Run(context.Background(), tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestManifestShape(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{
		WorkspaceClaim:      "hive-data",
		WorkspaceClaimMount: "/data",
		NodeSelector:        map[string]string{"accelerator": "spyre"},
		Tolerations:         []Toleration{{Key: "spyre", Operator: "Exists", Effect: "NoSchedule"}},
		Resources:           Resources{Limits: map[string]string{"ibm.com/spyre_pf": "2"}},
		ServiceAccount:      "hive",
		EnvFromSecrets:      []string{"artifact-registry"},
		ExtraVolumes:        []VolumeMount{{Claim: "compile-cache", MountPath: "/cache", ReadOnly: true}},
	})
	if _, err := l.Run(context.Background(), specFor("/data/agents/sandbox/deps-123")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, _ := json.Marshal(api.created)
	m := string(raw)
	for _, want := range []string{
		`"name":"hive-kick-hive-deps-1700000000"`,
		`"backoffLimit":0`,
		`"ttlSecondsAfterFinished":3600`,
		`"restartPolicy":"Never"`,
		`"nodeSelector":{"accelerator":"spyre"}`,
		// The fake decodes the manifest into a map before we re-marshal it
		// here, so keys come back alphabetical regardless of struct order.
		`"tolerations":[{"effect":"NoSchedule","key":"spyre","operator":"Exists"}]`,
		`"limits":{"ibm.com/spyre_pf":"2"}`,
		`"serviceAccountName":"hive"`,
		`"envFrom":[{"secretRef":{"name":"artifact-registry"}}]`,
		`"claimName":"hive-data"`,
		`"subPath":"agents/sandbox/deps-123"`,
		`"mountPath":"/workspace"`,
		`"workingDir":"/workspace"`,
		`"command":["claude","-p","@.hive/kick-prompt.txt"]`,
		`"claimName":"compile-cache","readOnly":true`,
		`"mountPath":"/cache","name":"extra-0","readOnly":true`,
		`"env":[{"name":"ALPHA","value":"a"},{"name":"ZED","value":"1"}]`,
		`"hive.kubestellar.io/agent":"hive-deps-sandbox"`,
		`"app.kubernetes.io/managed-by":"hive-kubejob"`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %s\n%s", want, m)
		}
	}
	for _, leak := range []string{"GITHUB_TOKEN", "MY_SECRET", "leak"} {
		if strings.Contains(m, leak) {
			t.Errorf("credential-shaped env %q reached the Job manifest:\n%s", leak, m)
		}
	}
}

func TestManifestDefaultsAndCustomTTLAndMount(t *testing.T) {
	l := &Launcher{Options: Options{WorkspaceClaim: "c", TTLSeconds: 60}}
	m := l.manifest("ns", "job", "sub", sandbox.LaunchSpec{Image: "i", WorkspaceMount: "/w", WorkDir: "/w/sub"})
	raw, _ := json.Marshal(m)
	s := string(raw)
	for _, want := range []string{`"ttlSecondsAfterFinished":60`, `"mountPath":"/w"`, `"workingDir":"/w/sub"`} {
		if !strings.Contains(s, want) {
			t.Errorf("manifest missing %s\n%s", want, s)
		}
	}
	for _, absent := range []string{"nodeSelector", "tolerations", "serviceAccountName", `"resources"`, `"command"`} {
		if strings.Contains(s, absent) {
			t.Errorf("manifest must omit %s when unset\n%s", absent, s)
		}
	}
}

func TestWorkspaceSubPath(t *testing.T) {
	cases := []struct {
		ws, mount, want string
		wantErr         bool
	}{
		{"/data/agents/sandbox/x", "", "agents/sandbox/x", false},
		{"/data/agents/sandbox/x/", "/data", "agents/sandbox/x", false},
		{"/mnt/hive/ws/x", "/mnt/hive", "ws/x", false},
		{"/data", "/data", "", true},
		{"/other/x", "/data", "", true},
		{"/data/../etc/x", "/data", "", true},
	}
	for _, tc := range cases {
		got, err := workspaceSubPath(tc.ws, tc.mount)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("workspaceSubPath(%q,%q) = %q,%v; want %q,err=%v", tc.ws, tc.mount, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestJobName(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if got := jobName("hive-deps-sandbox", now); got != "hive-kick-hive-deps-1700000000" {
		t.Errorf("jobName = %q", got)
	}
	if got := jobName("", now); got != "hive-kick-agent-1700000000" {
		t.Errorf("jobName(empty) = %q", got)
	}
	if got := jobName("Weird Name_With.Chars", now); got != "hive-kick-weird-name-with-chars-1700000000" {
		t.Errorf("jobName(weird) = %q", got)
	}
	long := jobName(strings.Repeat("a", 100)+"-b", now)
	if len(long) > jobNameMax {
		t.Errorf("jobName too long: %d %q", len(long), long)
	}
	if strings.Contains(long, "--") || strings.HasSuffix(strings.TrimSuffix(long, "-1700000000"), "-") {
		t.Errorf("truncated name must not end in a dash before the suffix: %q", long)
	}
}

func TestParseJobStatus(t *testing.T) {
	if _, done := parseJobStatus([]byte("garbage")); done {
		t.Error("garbage must not read as finished")
	}
	if _, done := parseJobStatus([]byte(`{"status":{"conditions":[{"type":"Complete","status":"False"}]}}`)); done {
		t.Error("a False condition must not read as finished")
	}
	out, done := parseJobStatus([]byte(`{"status":{"conditions":[{"type":"Failed","status":"True","message":"boom"}]}}`))
	if !done || out.succeeded || out.reason != "boom" {
		t.Errorf("failed-with-message parse = %+v,%v", out, done)
	}
}

func TestPodHelpersDegradeToUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/log"):
			w.WriteHeader(http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/pods"):
			_, _ = w.Write([]byte("{bad"))
		default:
			_, _ = w.Write([]byte("{bad"))
		}
	}))
	defer srv.Close()
	l := newLauncher(srv, Options{})
	if got := l.podLog(context.Background(), "hive", "p"); got != "" {
		t.Errorf("podLog on 403 = %q, want empty", got)
	}
	if got := l.podExitCode(context.Background(), "hive", "p"); got != exitCodeUnknown {
		t.Errorf("podExitCode on bad JSON = %d, want unknown", got)
	}
	if _, err := l.findPod(context.Background(), "hive", "j"); err == nil {
		t.Error("findPod on bad JSON must error")
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":{"containerStatuses":[{"name":"other","state":{"terminated":{"exitCode":9}}},{"name":"kick","state":{"waiting":{}}}]}}`))
	}))
	defer srv2.Close()
	l2 := newLauncher(srv2, Options{})
	if got := l2.podExitCode(context.Background(), "hive", "p"); got != exitCodeUnknown {
		t.Errorf("podExitCode for a non-terminated kick container = %d, want unknown", got)
	}
	srvDown := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srvDown.Close()
	l3 := newLauncher(srvDown, Options{})
	if got := l3.podExitCode(context.Background(), "hive", "p"); got != exitCodeUnknown {
		t.Errorf("podExitCode with the API down = %d, want unknown", got)
	}
	if _, err := l3.findPod(context.Background(), "hive", "j"); err == nil {
		t.Error("findPod with the API down must error")
	}
}

func TestNamespaceFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "namespace")
	if err := os.WriteFile(path, []byte(" hive-hosted \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Launcher{NamespacePath: path}
	if ns, err := l.namespace(); err != nil || ns != "hive-hosted" {
		t.Errorf("namespace = %q,%v", ns, err)
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.namespace(); err == nil || !strings.Contains(err.Error(), "empty namespace") {
		t.Errorf("empty file error = %v", err)
	}
	l = &Launcher{NamespacePath: filepath.Join(dir, "missing")}
	if _, err := l.namespace(); err == nil {
		t.Error("missing namespace file must error")
	}
	// Run surfaces a namespace failure before touching the API.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL)
	}))
	defer srv.Close()
	l = &Launcher{Options: Options{WorkspaceClaim: "c"}, APIServer: srv.URL, HTTPClient: srv.Client(), NamespacePath: filepath.Join(dir, "missing")}
	if _, err := l.Run(context.Background(), specFor("/data/x")); err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("Run without a namespace = %v", err)
	}
}

func TestTokenHeaderAndInClusterTLS(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI()
	api.statuses = []string{statusComplete}
	var seenAuth string
	var mu sync.Mutex
	inner := api.handler(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenAuth = r.Header.Get("Authorization")
		mu.Unlock()
		inner(w, r)
	}))
	defer srv.Close()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Launcher{
		Options:   Options{WorkspaceClaim: "hive-data", PollInterval: time.Millisecond},
		APIServer: srv.URL, Namespace: "hive", TokenPath: tokenPath, CAPath: caPath,
	}
	if _, err := l.Run(context.Background(), specFor("/data/agents/sandbox/x")); err != nil {
		t.Fatalf("Run over in-cluster TLS: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenAuth != "Bearer sa-token" {
		t.Errorf("Authorization = %q, want the SA token", seenAuth)
	}
	tr, _ := l.HTTPClient.Transport.(*http.Transport)
	if tr == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Error("in-cluster client must pin TLS 1.2 minimum")
	}
}

func TestClientCAFailures(t *testing.T) {
	dir := t.TempDir()
	l := &Launcher{CAPath: filepath.Join(dir, "missing")}
	if _, err := l.client(); err == nil || !strings.Contains(err.Error(), "reading in-cluster CA") {
		t.Errorf("missing CA = %v", err)
	}
	bad := filepath.Join(dir, "bad.crt")
	if err := os.WriteFile(bad, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	l = &Launcher{CAPath: bad}
	if _, err := l.client(); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Errorf("bad CA = %v", err)
	}
	// And Run reports it rather than panicking on a nil client.
	l = &Launcher{Options: Options{WorkspaceClaim: "c"}, Namespace: "hive", CAPath: bad}
	if _, err := l.Run(context.Background(), specFor("/data/x")); err == nil {
		t.Error("Run with an unusable CA must error")
	}
}

func TestDoUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	l := newLauncher(srv, Options{})
	if _, err := l.do(context.Background(), http.MethodGet, "/x", nil); err == nil {
		t.Error("do against a closed server must error")
	}
	if err := l.delete(context.Background(), "/x"); err == nil {
		t.Error("delete against a closed server must error")
	}
}

func TestDoRejectsBadMethodAndDefaultsServer(t *testing.T) {
	l := &Launcher{HTTPClient: &http.Client{}}
	if _, err := l.do(context.Background(), "BAD METHOD", "/x", nil); err == nil {
		t.Error("invalid method must error at request build")
	}
	if l.APIServer != "" {
		t.Error("APIServer must stay unset so the default applies")
	}
}

func TestAPIMessage(t *testing.T) {
	if got := apiMessage([]byte(`{"message":"m"}`)); got != "m" {
		t.Errorf("apiMessage = %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := apiMessage([]byte(long)); len(got) != 200 {
		t.Errorf("apiMessage long body len = %d, want 200", len(got))
	}
}

func TestSortedKeysAndSanitize(t *testing.T) {
	got := sortedKeys(map[string]string{"b": "", "a": "", "c": ""})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("sortedKeys = %v", got)
	}
	if got := sanitizeLabel("--Hello World--"); got != "hello-world" {
		t.Errorf("sanitizeLabel = %q", got)
	}
	if l := (&Launcher{}); l.now().IsZero() {
		t.Error("now() must default to wall clock")
	}
}

func TestOptionsValidateAcceptsMinimal(t *testing.T) {
	if err := (Options{WorkspaceClaim: "hive-data", ExtraVolumes: []VolumeMount{{Claim: "c", MountPath: "/m"}}}).Validate(); err != nil {
		t.Errorf("Validate = %v", err)
	}
}

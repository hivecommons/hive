package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/logread"
	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/startrec"
)

func writeCgroupFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func useMemoryEvidenceRoot(t *testing.T, root string) {
	t.Helper()
	saved := memoryEvidenceRoot
	memoryEvidenceRoot = root
	t.Cleanup(func() { memoryEvidenceRoot = saved })
}

func TestReadMemoryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		files     map[string]string
		limit     string
		bytes     int64
		used      int64
		oom       int64
		oomUnread bool
	}{
		{
			name: "cgroup v2 with a limit",
			files: map[string]string{
				"memory.max":     "536870912\n",
				"memory.current": "1048576\n",
				"memory.events":  "low 0\nhigh 0\nmax 2\noom 1\noom_kill 1\n",
			},
			limit: "512 MiB", bytes: 536870912, used: 1048576, oom: 1,
		},
		{
			name:  "cgroup v2 unlimited without events",
			files: map[string]string{"memory.max": "max\n"},
			limit: startrec.LimitUnlimited, oomUnread: true,
		},
		{
			name: "cgroup v1 unlimited",
			files: map[string]string{
				"memory/memory.limit_in_bytes": "9223372036854771712\n",
				"memory/memory.usage_in_bytes": "2048\n",
				"memory/memory.oom_control":    "oom_kill_disable 0\nunder_oom 0\noom_kill 4\n",
			},
			limit: MemLimitUnlimited, used: 2048, oom: 4,
		},
		{
			name:  "cgroup v1 with a limit",
			files: map[string]string{"memory/memory.limit_in_bytes": "268435456\n"},
			limit: "256 MiB", bytes: 268435456, oomUnread: true,
		},
		{
			name:  "no memory controller",
			files: map[string]string{},
			limit: MemLimitUnknown, oomUnread: true,
		},
		{
			name: "unreadable values",
			files: map[string]string{
				"memory.max":    "garbage\n",
				"memory.events": "oom_kill many\n",
			},
			limit: MemLimitUnknown, oomUnread: true,
		},
		{
			name:  "garbage oom count",
			files: map[string]string{"memory.max": "max\n", "memory.events": "oom_kill -1\n"},
			limit: MemLimitUnlimited, oomUnread: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := readMemoryEvidence(writeCgroupFixture(t, tc.files))
			if ev.Limit != tc.limit || ev.LimitBytes != tc.bytes || ev.UsedBytes != tc.used {
				t.Fatalf("evidence = %+v, want limit %q bytes %d used %d", ev, tc.limit, tc.bytes, tc.used)
			}
			if tc.oomUnread {
				if ev.OOMKills != nil {
					t.Fatalf("oom kills = %d, want not reported", *ev.OOMKills)
				}
				return
			}
			if ev.OOMKills == nil || *ev.OOMKills != tc.oom {
				t.Fatalf("oom kills = %v, want %d", ev.OOMKills, tc.oom)
			}
		})
	}
}

// AC4: the status collector reports the out-of-memory kill count and
// "unlimited", and the fields the web UI reads are still there.
func TestCollectSystemResourcesReportsOOMEvidence(t *testing.T) {
	useMemoryEvidenceRoot(t, writeCgroupFixture(t, map[string]string{
		"memory.max":    "max\n",
		"memory.events": "oom_kill 3\n",
	}))
	res := collectSystemResources()
	if res.MemLimit != MemLimitUnlimited || res.MemLimitBytes != 0 || res.OOMKillCount == nil || *res.OOMKillCount != 3 {
		t.Fatalf("system resources = %+v", res)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"diskUsedGB", "diskTotalGB", "diskPct", "memUsedMB", "memTotalMB", "memPct", "cpuPct", "cpuCores", "memLimit", "oomKillCount"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("systemResources lacks %q: %s", key, data)
		}
	}
	if ev := ReadMemoryEvidence(); ev.Limit != MemLimitUnlimited {
		t.Fatalf("ReadMemoryEvidence = %+v", ev)
	}
}

// AC4: the admin MCP fleet status read carries the new fields.
func TestFleetStatusReportsOOMEvidence(t *testing.T) {
	useMemoryEvidenceRoot(t, writeCgroupFixture(t, map[string]string{
		"memory.max":    "1073741824\n",
		"memory.events": "oom_kill 2\n",
	}))
	s := newTestServer()
	payload := minimalPayload()
	payload.SystemResources = collectSystemResources()
	s.status = payload
	provider := dashboardAdminMCPProvider{server: s, role: config.RoleRead}
	data, err := provider.Read(context.Background(), adminmcp.ToolFleetStatus, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	sys, _ := data.(map[string]any)["systemResources"].(map[string]any)
	if sys["oomKillCount"] != float64(2) || sys["memLimit"] != "1024 MiB" || sys["memLimitBytes"] != float64(1073741824) {
		t.Fatalf("fleet status systemResources = %#v", sys)
	}
	if _, ok := sys["memPct"]; !ok {
		t.Fatalf("fleet status lost an existing field: %#v", sys)
	}
}

// AC6: canaries planted in the environment, a secret file, the
// configuration and a log line of every source never come back from the
// reads this feature adds (the starts and crash sources, and fleet status),
// nor from any other log source.
func TestEvidenceReadsNeverReturnCanaries(t *testing.T) {
	const (
		envCanary  = "env-canary-11945-Qm4XzR8vLp2W"
		fileCanary = "file-canary-11945-Hn7YkT3sBd6J"
		cfgCanary  = "config-canary-11945-Ws5PcV9eMa1K"
	)
	canaries := []string{envCanary, fileCanary, cfgCanary}
	planted := strings.Join(canaries, " ")
	t.Setenv("HIVE_CANARY_API_KEY", envCanary)
	secretDir := t.TempDir()
	t.Cleanup(config.SetSecretFileRootsForTest(secretDir))
	if err := os.WriteFile(filepath.Join(secretDir, "canary-key"), []byte(fileCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	logDir := writeHiveLog(t, 2, "hive line "+planted)
	outDir := t.TempDir()
	savedOut := outputLogDir
	outputLogDir = func() string { return outDir }
	t.Cleanup(func() { outputLogDir = savedOut })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, stem := range []string{logread.SourceProxy, logread.SourceHiveOutput, logread.SourceLiteLLM} {
		rec, _ := json.Marshal(map[string]string{"time": now, "stream": logread.StreamStdout, "text": stem + " line " + planted})
		if err := os.WriteFile(filepath.Join(outDir, stem+".log"), append(rec, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reqLine, _ := json.Marshal(map[string]string{"time": now, "level": "INFO", "msg": "request", "path": "/api/x/" + planted})
	if err := os.WriteFile(filepath.Join(outDir, logread.SourceRequests+".log"), append(reqLine, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	useMemoryEvidenceRoot(t, writeCgroupFixture(t, map[string]string{
		"memory.max":    "max\n",
		"memory.events": "oom_kill 5\nnote " + planted + "\n" + planted + "\n",
	}))
	memory := func() (startrec.MemoryReading, bool) {
		ev := ReadMemoryEvidence()
		return startrec.MemoryReading{UsedBytes: ev.UsedBytes, Limit: ev.Limit, LimitBytes: ev.LimitBytes, OOMKills: ev.OOMKills}, true
	}
	noCrash := func(*os.File) error { return nil }
	if _, err := startrec.Begin(startrec.Options{Dir: outDir, ID: "canary01", Memory: memory, SetCrashOutput: noCrash}); err != nil {
		t.Fatal(err)
	}
	crashFiles, _ := filepath.Glob(filepath.Join(outDir, logread.SourceCrash+"-*.log"))
	if len(crashFiles) != 1 {
		t.Fatalf("crash files = %v", crashFiles)
	}
	appendLine := func(path, line string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	appendLine(crashFiles[0], "panic: "+planted)
	appendLine(filepath.Join(outDir, startrec.StartsFilename), `{"time":"`+now+`","level":"WARN","msg":"`+planted+`"}`)
	if _, err := startrec.Begin(startrec.Options{Dir: outDir, ID: "canary02", Memory: memory, SetCrashOutput: noCrash}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Governor.Logging.Dir = logDir
	cfg.Governor.LiteLLM.LocalProxy = true
	cfg.Dashboard.AuthToken = cfgCanary
	s := &Server{mux: http.NewServeMux(), deps: &Dependencies{Config: cfg}}
	s.registerLogReadRoutes()

	assertClean := func(what, body string) {
		t.Helper()
		for _, c := range canaries {
			if strings.Contains(body, c) {
				t.Fatalf("%s returned canary %q: %s", what, c, body)
			}
		}
	}
	sources := serveLog(s, config.RoleOwner, "/api/logs/sources")
	assertClean("log_sources", sources.Body.String())
	var list logread.SourcesResult
	if err := json.Unmarshal(sources.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, src := range list.Sources {
		rec := serveLog(s, config.RoleOwner, "/api/logs/read?lines=1000&source="+src.Name)
		assertClean("log_read "+src.Name, rec.Body.String())
		if src.Name != logread.SourceStarts && src.Name != logread.SourceCrash {
			continue
		}
		var res logread.Result
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		redacted := 0
		for _, r := range res.Redactions {
			if r.Kind == logread.SecretValueKind {
				redacted += r.Count
			}
		}
		if res.Status != logread.StatusOK || redacted < len(canaries) {
			t.Fatalf("%s read: status=%s redactions=%+v", src.Name, res.Status, res.Redactions)
		}
		if src.Name == logread.SourceStarts && !strings.Contains(rec.Body.String(), `\"oom_kills\":5`) {
			t.Fatalf("starts source lacks the previous run's reading: %s", rec.Body.String())
		}
	}

	st := newTestServer()
	payload := minimalPayload()
	payload.SystemResources = collectSystemResources()
	st.status = payload
	provider := dashboardAdminMCPProvider{server: st, role: config.RoleOwner}
	data, err := provider.Read(context.Background(), adminmcp.ToolFleetStatus, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(data)
	assertClean("fleet_status", string(body))
	if !strings.Contains(string(body), `"oomKillCount":5`) {
		t.Fatalf("fleet status lacks the oom kill count: %s", body)
	}
}

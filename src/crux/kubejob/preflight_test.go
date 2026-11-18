package kubejob

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

const preflightWorkspace = "/data/agents/sandbox/deps-123"

// preflightLog is the pod log of a container whose preflight failed.
const preflightLog = "torch: no spyre device\n" + preflightMarker + " command 1 of 1 exited 1: python -c smoke\n"

// agentSpec is an agent kick: Relays is what marks it as one.
func agentSpec() sandbox.LaunchSpec {
	spec := specFor(preflightWorkspace)
	spec.Relays = true
	return spec
}

func preflightOptions() Options {
	return Options{Preflight: []string{"python -c smoke"}, RelayImage: RelayImageNone}
}

// runScript executes the real preflight wrapper with /bin/sh, as the Job's
// container would, and returns its combined output and exit code.
func runScript(t *testing.T, o Options, agent []string, env ...string) (string, int) {
	t.Helper()
	argv := preflightCommand(agent, o)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the preflight script: %v", err)
	}
	return string(out), code
}

func TestPreflightScriptPassingStartsTheAgent(t *testing.T) {
	out, code := runScript(t, Options{Preflight: []string{"true", "echo smoke-ok"}}, []string{"echo", "agent-started"})
	if code != 0 || !strings.Contains(out, "agent-started") || !strings.Contains(out, "smoke-ok") {
		t.Fatalf("code=%d out=%q, want the preflight to pass and the agent to start", code, out)
	}
	if strings.Contains(out, preflightMarker) {
		t.Fatalf("a passing preflight printed the failure marker: %q", out)
	}
}

func TestPreflightScriptFailureStopsBeforeTheAgent(t *testing.T) {
	out, code := runScript(t, Options{Preflight: []string{"true", "echo device-gone >&2; exit 3"}}, []string{"echo", "agent-started"})
	if code != ExitCodeNodeUnhealthy {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeNodeUnhealthy)
	}
	if strings.Contains(out, "agent-started") {
		t.Fatalf("the agent ran after a failed preflight: %q", out)
	}
	for _, want := range []string{"device-gone", preflightMarker + " command 2 of 2 exited 3: echo device-gone"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestPreflightScriptTimesOutAHungCommand(t *testing.T) {
	o := Options{Preflight: []string{"exec sleep 30"}, PreflightTimeout: time.Second}
	start := time.Now()
	out, code := runScript(t, o, []string{"echo", "agent-started"})
	if code != ExitCodeNodeUnhealthy || !strings.Contains(out, "timed out after 1s") {
		t.Fatalf("code=%d out=%q, want a timeout failure", code, out)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("the hung command was not cut off: took %s", time.Since(start))
	}
}

func TestPreflightScriptLogsNoCredential(t *testing.T) {
	const secret = "ghp_preflightsecretvalue0123456789"
	out, code := runScript(t, Options{Preflight: []string{"exit 1"}}, []string{"true"}, "GITHUB_TOKEN="+secret, "MY_SECRET="+secret)
	if code != ExitCodeNodeUnhealthy {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeNodeUnhealthy)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("the preflight output carries a credential from the environment: %q", out)
	}
}

func TestPreflightCommandUnchangedWithoutConfig(t *testing.T) {
	cmd := []string{"claude", "-p"}
	if got := preflightCommand(cmd, Options{}); !reflect.DeepEqual(got, cmd) {
		t.Fatalf("no preflight must leave the command alone, got %q", got)
	}
	if got := preflightCommand(cmd, Options{Preflight: []string{"  ", ""}}); !reflect.DeepEqual(got, cmd) {
		t.Fatalf("blank preflight entries must leave the command alone, got %q", got)
	}
	if got := preflightCommand(nil, Options{Preflight: []string{"true"}}); got != nil {
		t.Fatalf("an empty command cannot be wrapped, got %q", got)
	}
}

func TestPreflightDefaultsAreSafe(t *testing.T) {
	if got := (Options{}).scheduleTimeout(); got != 10*time.Minute {
		t.Errorf("default schedule timeout = %s, want 10m", got)
	}
	if DefaultPreflightTimeout != 2*time.Minute || DefaultPreflightTotalTimeout != 5*time.Minute {
		t.Errorf("preflight defaults = %s / %s", DefaultPreflightTimeout, DefaultPreflightTotalTimeout)
	}
	argv := preflightCommand([]string{"agent"}, Options{Preflight: []string{"true"}})
	if argv[4] != "120" || argv[5] != "300" || argv[6] != "1" {
		t.Errorf("default per/total/count args = %v", argv[4:7])
	}
}

func TestManifestWithoutPreflightIsUnchanged(t *testing.T) {
	l := &Launcher{Options: Options{WorkspaceClaim: "hive-data"}}
	spec := agentSpec()
	m := manifestJSON(t, l.jobManifest("hive", "hive-kick-x", "deps", spec, nil))
	if !strings.Contains(m, `"command":["claude","-p","@.hive/kick-prompt.txt"]`) {
		t.Fatalf("the command changed without a preflight: %s", m)
	}
	if strings.Contains(m, "hive-node-preflight") {
		t.Fatalf("preflight script in a manifest that configures none: %s", m)
	}
}

func TestManifestRunsPreflightAfterTheRelayCheck(t *testing.T) {
	l := &Launcher{Options: Options{WorkspaceClaim: "hive-data", Preflight: []string{"python -c smoke"}}}
	m := l.jobManifest("hive", "hive-kick-x", "deps", agentSpec(), &relayPlan{image: "hive:test"})
	cmd := firstContainerCommand(t, m)
	if len(cmd) < 8 || cmd[3] != "hive-relay-preflight" {
		t.Fatalf("command does not start with the relay check: %q", cmd)
	}
	rest := cmd[5:]
	if rest[0] != preflightShell || rest[3] != "hive-node-preflight" || rest[7] != "python -c smoke" {
		t.Fatalf("the node preflight does not follow the relay check: %q", rest)
	}
	if got := rest[8:]; !reflect.DeepEqual(got, []string{"claude", "-p", "@.hive/kick-prompt.txt"}) {
		t.Fatalf("the agent command must come last, got %q", got)
	}
}

func firstContainerCommand(t *testing.T, manifest map[string]any) []string {
	t.Helper()
	var m struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Command []string `json:"command"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(manifestJSON(t, manifest)), &m); err != nil || len(m.Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("decoding the manifest: %v", err)
	}
	return m.Spec.Template.Spec.Containers[0].Command
}

func manifestJSON(t *testing.T, manifest map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestPreflightOptionsValidation(t *testing.T) {
	good := Options{WorkspaceClaim: "c", Preflight: []string{"true"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid preflight refused: %v", err)
	}
	for name, mutate := range map[string]func(*Options){
		"empty command":     func(o *Options) { o.Preflight = []string{"true", "  "} },
		"negative timeout":  func(o *Options) { o.PreflightTimeout = -time.Second },
		"negative total":    func(o *Options) { o.PreflightTotalTimeout = -time.Second },
		"negative delay":    func(o *Options) { o.PreflightRetryDelay = -time.Second },
		"negative schedule": func(o *Options) { o.ScheduleTimeout = -time.Second },
		"too many":          func(o *Options) { o.Preflight = make([]string, config.MaxSandboxPreflightCommands+1) },
	} {
		o := good
		mutate(&o)
		if err := o.Validate(); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
}

func TestAffinityAvoidingNodes(t *testing.T) {
	avoid := config.SandboxJobNodeSelectorRequirement{Key: "kubernetes.io/hostname", Operator: "NotIn", Values: []string{"node-a"}}
	got := affinityAvoiding(nil, []string{"node-a"})
	terms := got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || !reflect.DeepEqual(terms[0].MatchExpressions, []config.SandboxJobNodeSelectorRequirement{avoid}) {
		t.Fatalf("nil affinity: %+v", got)
	}
	in := &config.SandboxJobAffinity{NodeAffinity: &config.SandboxJobNodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &config.SandboxJobNodeSelector{NodeSelectorTerms: []config.SandboxJobNodeSelectorTerm{
			{MatchExpressions: []config.SandboxJobNodeSelectorRequirement{{Key: "pool", Operator: "In", Values: []string{"spyre"}}}},
			{MatchExpressions: []config.SandboxJobNodeSelectorRequirement{{Key: "pool", Operator: "In", Values: []string{"spyre-b"}}}},
		}},
		PreferredDuringSchedulingIgnoredDuringExecution: []config.SandboxJobPreferredSchedulingTerm{{Weight: 5, Preference: config.SandboxJobNodeSelectorTerm{MatchExpressions: []config.SandboxJobNodeSelectorRequirement{{Key: "x", Operator: "Exists"}}}}},
	}}
	before, _ := json.Marshal(in)
	got = affinityAvoiding(in, []string{"node-a"})
	// Terms are ORed, so the exclusion has to be ANDed into every one.
	for i, term := range got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if n := len(term.MatchExpressions); n != 2 || !reflect.DeepEqual(term.MatchExpressions[1], avoid) {
			t.Errorf("term %d = %+v, want the operator's expression plus the exclusion", i, term)
		}
	}
	if len(got.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Error("the preferred terms were dropped")
	}
	if after, _ := json.Marshal(in); string(after) != string(before) {
		t.Fatalf("the operator's shared affinity was mutated:\n%s\n%s", before, after)
	}
}

// events collects launcher events from the polling goroutine.
type events struct {
	mu  sync.Mutex
	all []Event
}

func (e *events) add(ev Event) { e.mu.Lock(); e.all = append(e.all, ev); e.mu.Unlock() }
func (e *events) kinds() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var k []string
	for _, ev := range e.all {
		k = append(k, ev.Kind)
	}
	return k
}

func TestRunPassingPreflightStartsTheAgentOnce(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusComplete}
	api.podNodes = []string{"node-a"}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, preflightOptions())

	res, err := l.Run(context.Background(), agentSpec())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if api.jobCreations != 1 || res.FailReason != "" || res.Node != "node-a" {
		t.Fatalf("creations=%d result=%+v, want one Job on node-a and no failure", api.jobCreations, res)
	}
	cmd := firstContainerCommand(t, api.created)
	if len(cmd) < 8 || cmd[3] != "hive-node-preflight" || cmd[len(cmd)-1] != "@.hive/kick-prompt.txt" {
		t.Fatalf("the Job does not run the preflight then the agent: %q", cmd)
	}
}

func TestRunPreflightFailureFailsFastAndRetriesOnAnotherNode(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusFailed, statusComplete}
	api.exitCodes = []int{ExitCodeNodeUnhealthy, 0}
	api.podNodes = []string{"node-a", "node-b"}
	api.logs = []string{preflightLog, "agent ok\n"}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	opts := preflightOptions()
	var ev events
	opts.OnEvent = ev.add
	opts.Affinity = &config.SandboxJobAffinity{NodeAffinity: &config.SandboxJobNodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &config.SandboxJobNodeSelector{NodeSelectorTerms: []config.SandboxJobNodeSelectorTerm{
			{MatchExpressions: []config.SandboxJobNodeSelectorRequirement{{Key: "pool", Operator: "In", Values: []string{"spyre"}}}},
		}},
	}}
	l := newLauncher(srv, opts)

	res, err := l.Run(context.Background(), agentSpec())
	if err != nil {
		t.Fatalf("Run: %v (the retry on node-b passed)", err)
	}
	if api.jobCreations != 2 {
		t.Fatalf("Jobs created = %d, want the original and one retry", api.jobCreations)
	}
	first, second := api.createdAll[0], api.createdAll[1]
	if manifestName(first) == manifestName(second) || !strings.HasSuffix(manifestName(second), "-r1") {
		t.Errorf("job names %q / %q: the retry needs its own name", manifestName(first), manifestName(second))
	}
	if strings.Contains(manifestJSON(t, first), "NotIn") {
		t.Errorf("the first attempt already excluded a node: %s", manifestJSON(t, first))
	}
	retry := manifestJSON(t, second)
	for _, want := range []string{`"key":"kubernetes.io/hostname"`, `"operator":"NotIn"`, `"values":["node-a"]`, `"key":"pool"`} {
		if !strings.Contains(retry, want) {
			t.Errorf("retry manifest lacks %s: %s", want, retry)
		}
	}
	if res.Node != "node-b" || res.FailReason != "" {
		t.Errorf("result = %+v, want a clean run on node-b", res)
	}
	for _, want := range []string{"attempt 1 on node node-a failed the node preflight", preflightMarker, "agent ok"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("transcript lacks %q: %q", want, res.Stdout)
		}
	}
	if got := ev.kinds(); !reflect.DeepEqual(got, []string{EventNodeRetry}) {
		t.Errorf("events = %v, want one node_retry", got)
	}
}

func manifestName(manifest map[string]any) string {
	md, _ := manifest["metadata"].(map[string]any)
	n, _ := md["name"].(string)
	return n
}

func TestRunPreflightFailsOnTheRetryNodeToo(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusFailed}
	api.exitCodes = []int{ExitCodeNodeUnhealthy}
	api.podNodes = []string{"node-a", "node-b"}
	api.logs = []string{preflightLog}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, preflightOptions())

	res, err := l.Run(context.Background(), agentSpec())
	var fe *sandbox.FailError
	if !errors.As(err, &fe) || fe.Reason != sandbox.FailReasonNodeUnhealthy || fe.Node != "node-b" {
		t.Fatalf("Run error = %v, want node_unhealthy on node-b", err)
	}
	if !strings.Contains(fe.Message, preflightMarker) || !strings.Contains(fe.Message, "python -c smoke") {
		t.Errorf("message = %q, want the failing command", fe.Message)
	}
	if api.jobCreations != 2 {
		t.Errorf("Jobs created = %d, want exactly one retry", api.jobCreations)
	}
	if res.FailReason != sandbox.FailReasonNodeUnhealthy || res.Node != "node-b" || res.ExitCode != ExitCodeNodeUnhealthy {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(res.Stdout, "no spyre device") {
		t.Errorf("the command's output is missing from the transcript: %q", res.Stdout)
	}
	if api.deleted {
		t.Error("failed Jobs are left for inspection until their TTL")
	}
}

func TestRunAgentExitingWithTheSameCodeIsNotANodeFailure(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusFailed}
	api.exitCode = ExitCodeNodeUnhealthy
	api.podNodes = []string{"node-a"}
	api.logBody = "agent gave up\n"
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, preflightOptions())

	res, err := l.Run(context.Background(), agentSpec())
	var fe *sandbox.FailError
	if err == nil || errors.As(err, &fe) || res.FailReason != "" {
		t.Fatalf("err=%v result=%+v: an agent's own exit 75 without the marker is an ordinary failure", err, res)
	}
	if api.jobCreations != 1 {
		t.Errorf("Jobs created = %d, want no retry", api.jobCreations)
	}
}

func TestRunWithoutAKnownNodeDoesNotRetry(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusFailed}
	api.exitCode = ExitCodeNodeUnhealthy
	api.logBody = preflightLog
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, preflightOptions())

	_, err := l.Run(context.Background(), agentSpec())
	var fe *sandbox.FailError
	if !errors.As(err, &fe) || api.jobCreations != 1 {
		t.Fatalf("err=%v creations=%d: with no node to exclude there is nothing to retry on", err, api.jobCreations)
	}
}

func TestRunOnlyAgentKicksGetThePreflight(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusComplete}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, preflightOptions())
	spec := specFor(preflightWorkspace) // Relays unset: a publish.verify launch
	if _, err := l.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cmd := firstContainerCommand(t, api.created); !reflect.DeepEqual(cmd, spec.Command) {
		t.Fatalf("a non-agent launch was gated on the node preflight: %q", cmd)
	}
	if len(l.Options.Preflight) != 1 {
		t.Error("the shared launcher's options were modified")
	}
}

func TestRunPreflightWithoutACommandIsRefused(t *testing.T) {
	spec := agentSpec()
	spec.Command = nil
	srv := httptest.NewServer(newFakeAPI().handler(t))
	defer srv.Close()
	l := newLauncher(srv, preflightOptions())
	if _, err := l.Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "explicit command") {
		t.Fatalf("Run error = %v, want a refusal: there is no agent command to run after the preflight", err)
	}
}

func TestRetryDelayIsHonoredAndCancellable(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusFailed}
	api.exitCode = ExitCodeNodeUnhealthy
	api.podNodes = []string{"node-a"}
	api.logBody = preflightLog
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	opts := preflightOptions()
	opts.PreflightRetryDelay = time.Hour
	l := newLauncher(srv, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := l.Run(ctx, agentSpec())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want the context to end the retry delay", err)
	}
	if api.jobCreations != 1 {
		t.Errorf("Jobs created = %d, want no retry before the delay", api.jobCreations)
	}
}

// steppingClock advances a minute per reading, so a poll loop sees time pass
// without the test sleeping.
func steppingClock() func() time.Time {
	var mu sync.Mutex
	t := time.Unix(1700000000, 0)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(time.Minute)
		return t
	}
}

const (
	unschedulablePod = `{"items":[{"metadata":{"name":"hive-kick-deps-1-abcde"},"spec":{},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/38 nodes are available: 38 node(s) didn't match Pod's node affinity/selector."}]}}]}`
	scheduledPod     = `{"items":[{"metadata":{"name":"hive-kick-deps-1-abcde"},"spec":{"nodeName":"node-a"},"status":{"phase":"Running"}}]}`
	creatingPod      = `{"items":[{"metadata":{"name":"hive-kick-deps-1-abcde"},"spec":{},"status":{"phase":"Pending"}}]}`
)

func TestRunUnschedulablePastTheTimeoutFailsFastAndDeletesTheJob(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusPending}
	api.podLists = []string{unschedulablePod}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	var ev events
	l := newLauncher(srv, Options{ScheduleTimeout: 3 * time.Minute, OnEvent: ev.add})
	l.Now = steppingClock()

	res, err := l.Run(context.Background(), specFor(preflightWorkspace))
	var fe *sandbox.FailError
	if !errors.As(err, &fe) || fe.Reason != sandbox.FailReasonWaitingForNode {
		t.Fatalf("Run error = %v, want waiting_for_node", err)
	}
	if !strings.Contains(fe.Message, "0/38 nodes are available") {
		t.Errorf("message = %q, want the scheduler's reason", fe.Message)
	}
	if !api.deleted {
		t.Error("the unschedulable Job must be deleted so the agent's slot is released")
	}
	if res.FailReason != sandbox.FailReasonWaitingForNode || res.ExitCode != exitCodeUnknown || !strings.Contains(res.Stderr, "0/38 nodes") {
		t.Errorf("result = %+v", res)
	}
	if got := ev.kinds(); !reflect.DeepEqual(got, []string{EventWaitingForNode}) {
		t.Errorf("events = %v, want one waiting_for_node", got)
	}
}

func TestRunUnschedulableBelowTheTimeoutKeepsWaiting(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusPending, statusPending, statusPending, statusComplete}
	api.podLists = []string{unschedulablePod, scheduledPod}
	api.podNodes = []string{"node-a"}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	var ev events
	l := newLauncher(srv, Options{ScheduleTimeout: time.Hour, OnEvent: ev.add})
	l.Now = steppingClock()

	res, err := l.Run(context.Background(), specFor(preflightWorkspace))
	if err != nil {
		t.Fatalf("Run: %v (the pod was scheduled before the timeout)", err)
	}
	if res.Node != "node-a" || res.FailReason != "" {
		t.Errorf("result = %+v", res)
	}
	if got := ev.kinds(); !reflect.DeepEqual(got, []string{EventWaitingForNode, EventScheduled}) {
		t.Errorf("events = %v, want waiting then scheduled", got)
	}
}

func TestRunPendingWithoutUnschedulableConditionNeverTrips(t *testing.T) {
	api := newFakeAPI()
	api.statuses = []string{statusPending, statusPending, statusPending, statusComplete}
	api.podLists = []string{creatingPod}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	l := newLauncher(srv, Options{ScheduleTimeout: time.Minute})
	l.Now = steppingClock()
	if _, err := l.Run(context.Background(), specFor(preflightWorkspace)); err != nil {
		t.Fatalf("Run: %v (a pulling image is not unschedulable)", err)
	}
}

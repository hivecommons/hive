package dashboard

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/pkg/config"
)

// Job-runtime terminal (hivecommons/hive#14798).
//
// An agent with sandbox.runtime: job runs each kick as a Kubernetes Job in
// its own pod, so there is no tmux session in the hive pod for ttyd to
// attach to and the ordinary "▶ terminal" sat on "tmux session not created
// yet" until its wait expired. For those agents the SPA's terminal link
// points here instead (terminalUrl in static/index.html, keyed on the
// sandboxRuntime field of the agent status), and this page streams the kick
// pod's log read-only.
//
// Mounted UNDER /terminal on purpose: the terminal handoff code, the
// Path=/terminal assertion cookie and the terminal role check in
// authenticate() are all keyed on that prefix, so the existing openTerminal()
// flow in the SPA authenticates this page exactly as it does ttyd, with no
// second auth path to maintain. The Go 1.22 mux picks these more specific
// patterns over the "/terminal/" reverse proxy.
//
// Why the Go server rather than ttyd-tmux.sh: the hive image carries no
// kubectl (the kubejob launcher deliberately speaks to the API server over
// plain HTTP with the pod's ServiceAccount), the script runs as the
// unprivileged ttyd user with no view of hive.yaml to learn an agent's
// runtime, and streaming the pod log from Go reuses the launcher's client,
// namespace discovery and the RBAC already granted for the launcher's own
// end-of-run log read (pods, pods/log get+list in deploy/k8s/sandbox-job-rbac.yaml).
//
// Three states, each named in the first line of output so an operator never
// mistakes one for another:
//   - a pod exists for the agent: its log is streamed, following while the
//     kick container runs, then the exit code is reported;
//   - no pod, but a persisted transcript: the last run's transcript and its
//     recorded exit code (a completed Job is deleted by the launcher as soon
//     as the kick finishes, so this is the normal state between kicks);
//   - neither: the agent has not run yet.
//
// OpenShell runtime (hivecommons/hive#17645). An agent with
// sandbox.runtime: openshell has no tmux session either: each kick runs in
// a sandbox an OpenShell gateway owns. The same page, route and three
// states apply; only the live source differs. Instead of the Job pod log it
// follows the newest sandbox the launcher created for the agent through
// sandbox.OpenShellLogReader, which keeps the OpenShell SDK out of this
// package, and the launcher deletes the sandbox when the kick ends, so
// between kicks the pane shows the persisted transcript exactly as above.
//
// The live source is weaker than the pod log, and the pane says so. The
// gateway's sandbox log holds supervisor, policy (OCSF) and tracing
// events; the kick runs as an exec session whose output reaches neither
// that log nor the container log (OpenShell docs, "Accessing Logs"), and
// Sandbox.Status.ExitCode is the sandbox main process's, not the kick's.
// So while a kick runs the pane shows what the sandbox did around it
// (network and filesystem decisions, supervisor state) and then points the
// operator at the transcript, which is where the kick's own output and
// exit code land once the launcher's Exec().Run returns. The job pod log
// and the substrate stream below show the kick's stdout live.
//
// Substrate runtime (hivecommons/hive#17777). An agent with
// sandbox.runtime: substrate runs each kick in a Substrate actor, again with
// no tmux session, so it uses the same page, route and three states. The
// Control API has no ListActors and Substrate no logs RPC, so the live
// source is sandbox.SubstrateLogReader: the launcher, in this same process,
// records the actor (name, atespace, every state it passes through, the
// worker's SPIFFE id) and tees the kick's stdout and stderr from the
// bridge's streamed /run into it. Unlike OpenShell this is the kick's own
// output and exit code. When the launcher is done the actor is deleted or,
// with snapshot_on_idle, suspended; the pane names which, and between kicks
// shows how the last actor ended above the persisted transcript.

const (
	// Both live under terminalPathPrefix so the terminal handoff code,
	// the Path=/terminal assertion cookie and the terminal role check all
	// apply unchanged (isTerminalPath).
	jobTerminalPagePath   = "/terminal/job/{name}"
	jobTerminalStreamPath = "/terminal/job/{name}/stream"

	// jobTerminalMarker prefixes every line this handler writes itself, so
	// status lines are distinguishable from the agent's own output.
	jobTerminalMarker = "── "

	// defaultJobTerminalPollInterval is how often a Pending pod (image pull,
	// scheduling) is re-read before its log can be followed; the API server
	// refuses a log request until the container has started.
	defaultJobTerminalPollInterval = 3 * time.Second
)

// jobPodLogReader is the slice of *kubejob.Launcher the terminal uses, so
// tests can substitute a fake without an API server.
type jobPodLogReader interface {
	LatestPod(ctx context.Context, launchName string) (kubejob.PodInfo, bool, error)
	PodStatus(ctx context.Context, pod string) (kubejob.PodInfo, error)
	StreamLog(ctx context.Context, pod string, follow bool, w io.Writer) error
}

// jobTerminalDeps are the test seams for the job-runtime terminal; nil
// fields take the in-cluster defaults.
type jobTerminalDeps struct {
	reader        jobPodLogReader
	openshell     sandbox.OpenShellLogReader
	substrate     sandbox.SubstrateLogReader
	workspaceRoot string
	pollInterval  time.Duration
}

func (s *Server) registerJobTerminal() {
	// Literal patterns (not the constants) so that
	// scripts/check-api-reference-citations.sh can verify docs/api-reference.md.
	s.mux.HandleFunc("GET /terminal/job/{name}", s.handleJobTerminalPage)
	s.mux.HandleFunc("GET /terminal/job/{name}/stream", s.handleJobTerminalStream)
}

func (s *Server) jobPodLogReader() jobPodLogReader {
	if s.jobTerminal.reader != nil {
		return s.jobTerminal.reader
	}
	return &kubejob.Launcher{}
}

// openshellLogReader is the OpenShell counterpart of jobPodLogReader: a
// launcher pointed at the gateway in the agent's effective openshell block.
// Only the fields a read needs are set (where the gateway is and how to
// authenticate to it); the kick-shaping ones belong to the agent manager's
// launcher, which this package does not import (crux boundary ratchet).
func (s *Server) openshellLogReader(name string) sandbox.OpenShellLogReader {
	if s.jobTerminal.openshell != nil {
		return s.jobTerminal.openshell
	}
	var opts sandbox.OpenShellOptions
	if s.deps != nil && s.deps.Config != nil {
		if agentCfg, ok := s.deps.Config.Agents[name]; ok {
			oc := agentCfg.SandboxOpenShell(s.deps.Config.AgentSandbox)
			opts = sandbox.OpenShellOptions{GatewayURL: oc.GatewayURL, Workspace: oc.Namespace, InsecureTLS: oc.InsecureTLS}
			if env := strings.TrimSpace(oc.TokenEnv); env != "" {
				opts.Token = os.Getenv(env)
			}
		}
	}
	return &sandbox.OpenShellLauncher{Options: opts}
}

// substrateLogReader is the substrate counterpart: the process-wide record
// the substrate launcher writes each kick into.
func (s *Server) substrateLogReader() sandbox.SubstrateLogReader {
	if s.jobTerminal.substrate != nil {
		return s.jobTerminal.substrate
	}
	return sandbox.DefaultSubstrateKicks
}

func (s *Server) jobTerminalWorkspaceRoot() string {
	if s.jobTerminal.workspaceRoot != "" {
		return s.jobTerminal.workspaceRoot
	}
	configured := ""
	if s.deps != nil && s.deps.Config != nil {
		configured = s.deps.Config.AgentSandbox.WorkspaceDir
	}
	// Same resolution as the agent manager (crux/agent.NewManager), whose
	// work dir is HIVE_WORK_DIR or the sandbox package default.
	return sandbox.WorkspaceRoot(configured, os.Getenv("HIVE_WORK_DIR"))
}

func (s *Server) jobTerminalPollInterval() time.Duration {
	if s.jobTerminal.pollInterval > 0 {
		return s.jobTerminal.pollInterval
	}
	return defaultJobTerminalPollInterval
}

// sandboxRuntimeWithoutTmux reports whether an agent on this sandbox
// runtime runs its kicks outside the hive pod, so it has no tmux session
// and its terminal is this read-only viewer.
func sandboxRuntimeWithoutTmux(rt string) bool {
	return rt == config.SandboxRuntimeJob || rt == config.SandboxRuntimeOpenShell || rt == config.SandboxRuntimeSubstrate
}

// jobTerminalAgent resolves the path's agent and checks that its effective
// sandbox runtime has no tmux session (job, openshell or substrate), returning that
// runtime. The status code says which precondition failed: 404 for an
// unknown agent, 409 for one that has a tmux terminal instead.
func (s *Server) jobTerminalAgent(r *http.Request) (string, string, int, string) {
	if s.deps == nil || s.deps.Config == nil {
		return "", "", http.StatusServiceUnavailable, "configuration unavailable"
	}
	name := s.resolveAgentParam(r.PathValue("name"))
	cfg := s.deps.Config
	agentCfg, ok := cfg.Agents[name]
	if !ok {
		return "", "", http.StatusNotFound, fmt.Sprintf("agent %q not found", name)
	}
	if !agentCfg.SandboxEnabled(cfg.AgentSandbox) {
		return "", "", http.StatusConflict, fmt.Sprintf("agent %q is not sandboxed; its terminal is the tmux session at /terminal/?arg=hive-%s", name, name)
	}
	rt := agentCfg.SandboxRuntime(cfg.AgentSandbox)
	if !sandboxRuntimeWithoutTmux(rt) {
		return "", "", http.StatusConflict, fmt.Sprintf("agent %q runs sandbox kicks with runtime %q, not as a Kubernetes Job, OpenShell sandbox or Substrate actor; there is no sandbox log to show", name, rt)
	}
	return name, rt, http.StatusOK, ""
}

// handleJobTerminalPage serves the viewer document: a <pre> the inline
// script fills from the stream endpoint as lines arrive. The script is
// inline because /terminal documents carry the blanket script-src
// 'unsafe-inline' policy (securityHeaders) rather than the SPA's hash
// allowlist; no on*= attributes are used, matching the rest of the server.
func (s *Server) handleJobTerminalPage(w http.ResponseWriter, r *http.Request) {
	name, rt, code, msg := s.jobTerminalAgent(r)
	if code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	terms := sandboxTermsFor(rt)
	esc := html.EscapeString(name)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, jobTerminalPageHTML, esc, terms.title, esc, terms.title, terms.about, esc)
}

// %s placeholders: agent and log kind (title), agent and log kind
// (heading), what the log is, agent (stream URL path segment). The stream
// URL is relative so the page works behind any prefix the dashboard host is
// served at.
const jobTerminalPageHTML = `<!DOCTYPE html><html><head><meta charset="utf-8"><title>%s — %s</title>
<style>
html,body{height:100%%;margin:0}
body{font-family:ui-monospace,monospace;background:#0d1117;color:#c9d1d9;display:flex;flex-direction:column}
header{padding:.5rem 1rem;border-bottom:1px solid #21262d;display:flex;gap:1rem;align-items:baseline;flex:none}
h1{font-size:1rem;margin:0}
.muted{color:#8b949e;font-size:.85rem}
#status{margin-left:auto}
button{background:#21262d;color:#c9d1d9;border:1px solid #30363d;border-radius:4px;padding:.2rem .6rem;cursor:pointer;font:inherit}
pre{flex:1;overflow:auto;margin:0;padding:1rem;white-space:pre-wrap;word-break:break-word;font-size:.85rem;line-height:1.35}
.marker{color:#58a6ff}
</style></head><body>
<header><h1>▶ %s — %s</h1><span class="muted">read-only: %s, not a shell</span><span id="status" class="muted">connecting…</span><button id="reconnect" type="button">⟳ reconnect</button></header>
<pre id="log"></pre>
<script>
(function(){
  var pre=document.getElementById('log'),status=document.getElementById('status'),btn=document.getElementById('reconnect');
  var streamURL=new URL('./%s/stream',window.location.href).toString();
  var controller=null;
  function nearBottom(){return pre.scrollHeight-pre.scrollTop-pre.clientHeight<48;}
  function append(text){
    var follow=nearBottom();
    pre.appendChild(document.createTextNode(text));
    if(follow)pre.scrollTop=pre.scrollHeight;
  }
  function connect(){
    if(controller)controller.abort();
    controller=new AbortController();
    pre.textContent='';
    status.textContent='connecting…';
    fetch(streamURL,{credentials:'same-origin',cache:'no-store',signal:controller.signal}).then(function(resp){
      if(!resp.ok){return resp.text().then(function(t){throw new Error('HTTP '+resp.status+': '+t);});}
      status.textContent='streaming';
      var reader=resp.body.getReader(),decoder=new TextDecoder();
      function pump(){
        return reader.read().then(function(res){
          if(res.done){append(decoder.decode());status.textContent='stream ended';return;}
          append(decoder.decode(res.value,{stream:true}));
          return pump();
        });
      }
      return pump();
    }).catch(function(err){
      if(err&&err.name==='AbortError')return;
      status.textContent='disconnected';
      append('\n── '+(err&&err.message?err.message:err)+'\n');
    });
  }
  btn.addEventListener('click',connect);
  connect();
})();
</script>
</body></html>
`

// handleJobTerminalStream writes the kick pod's log as plain text, flushed
// line by line, with status lines prefixed by jobTerminalMarker. Everything
// from the pod is passed through the same credential scrubber the executor
// applies before persisting a transcript (CWE-532): the live log is the one
// path where unscrubbed agent output could otherwise reach a browser.
func (s *Server) handleJobTerminalStream(w http.ResponseWriter, r *http.Request) {
	name, rt, code, msg := s.jobTerminalAgent(r)
	if code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Ask intermediaries (the hub nginx, an Ingress) not to buffer the
	// response, or a followed stream arrives all at once when the pod ends.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	out := &jobTerminalWriter{w: w}
	ctx := r.Context()
	if rt == config.SandboxRuntimeOpenShell {
		s.streamOpenShellSandbox(ctx, out, name, s.openshellLogReader(name))
		return
	}
	if rt == config.SandboxRuntimeSubstrate {
		s.streamSubstrateActor(ctx, out, name, s.substrateLogReader())
		return
	}
	reader := s.jobPodLogReader()

	pod, found, err := reader.LatestPod(ctx, sandbox.LaunchName(name))
	if err != nil {
		out.status("cannot list Job pods for agent %q: %v", name, err)
		out.status("hint: the hive ServiceAccount needs get+list on pods and pods/log in its own namespace (deploy/k8s/sandbox-job-rbac.yaml); showing the last persisted transcript instead")
		s.writeJobTranscript(out, name)
		return
	}
	if !found {
		s.writeJobTranscript(out, name)
		return
	}
	s.streamJobPod(ctx, out, reader, pod)
}

// streamJobPod follows a live pod, or dumps a finished one, then reports the
// exit code. A Pending pod is re-read on a ticker until it is Running (or
// finished, or the client leaves): the API server answers a log request for
// a container that has not started with 400, which would otherwise end the
// stream before the kick has produced a line.
func (s *Server) streamJobPod(ctx context.Context, out *jobTerminalWriter, reader jobPodLogReader, pod kubejob.PodInfo) {
	out.status("job %s · pod %s · %s", orUnknown(pod.Job), pod.Name, describePodPhase(pod))
	if pod.Phase == kubejob.PodPhasePending && !pod.Finished() {
		ticker := time.NewTicker(s.jobTerminalPollInterval())
		defer ticker.Stop()
		for pod.Phase == kubejob.PodPhasePending && !pod.Finished() {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			next, err := reader.PodStatus(ctx, pod.Name)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				out.status("waiting for pod %s to start: %v", pod.Name, err)
				continue
			}
			if next.Waiting != pod.Waiting || next.Phase != pod.Phase {
				out.status("pod %s · %s", next.Name, describePodPhase(next))
			}
			pod = next
		}
	}
	follow := !pod.Finished()
	scrub := &kubejob.LineFilterWriter{Dst: out, Fn: scrubLogLine}
	err := reader.StreamLog(ctx, pod.Name, follow, scrub)
	_ = scrub.Close()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		out.status("log stream ended with an error: %v", err)
	}
	if follow {
		// The follow ended because the container exited; re-read for the code.
		// The request context may already be gone if the browser left; a
		// short detached context still answers the common case.
		statusCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if next, err := reader.PodStatus(statusCtx, pod.Name); err == nil {
			pod = next
		}
	}
	if pod.Terminated {
		out.status("kick container exited with code %d", pod.ExitCode)
	} else {
		out.status("pod %s · %s (exit code not reported)", pod.Name, describePodPhase(pod))
	}
}

// streamOpenShellSandbox is the openshell-runtime counterpart of the pod
// path above: follow the agent's live sandbox's gateway log through the
// launcher's log reader, scrubbed line by line, then say how the sandbox
// ended and where the kick's own output and exit code are; with no live
// sandbox, fall back to the persisted transcript.
func (s *Server) streamOpenShellSandbox(ctx context.Context, out *jobTerminalWriter, name string, reader sandbox.OpenShellLogReader) {
	info, found, err := reader.LatestSandbox(ctx, sandbox.LaunchName(name))
	if err != nil {
		out.status("cannot list OpenShell sandboxes for agent %q: %v", name, err)
		out.status("hint: check the agent's sandbox.openshell gateway_url and token_env; showing the last persisted transcript instead")
		s.writeSandboxTranscript(out, name, openshellTerms)
		return
	}
	if !found {
		s.writeSandboxTranscript(out, name, openshellTerms)
		return
	}
	out.status("openshell sandbox %s · %s", info.Name, orUnknown(info.Phase))
	out.status("%s", openshellLiveLogNote)
	scrub := &kubejob.LineFilterWriter{Dst: out, Fn: scrubLogLine}
	final, err := reader.StreamLogs(ctx, info.Name, s.jobTerminalPollInterval(), scrub)
	_ = scrub.Close()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		out.status("log stream ended with an error: %v", err)
	}
	if final.Name == "" {
		final.Name = info.Name
	}
	switch {
	case final.Phase == sandbox.OpenShellPhaseDeleting:
		// The launcher deletes the sandbox the moment Exec().Run returns,
		// so this is the normal end of a kick.
		out.status("sandbox %s was deleted by the launcher: the kick has finished. %s", final.Name, openshellKickResultNote)
	case final.Exited:
		out.status("sandbox %s main process exited with code %d (that is the sandbox workload, not the kick). %s", final.Name, final.ExitCode, openshellKickResultNote)
	default:
		out.status("sandbox %s · %s. %s", final.Name, orUnknown(final.Phase), openshellKickResultNote)
	}
}

// streamSubstrateActor is the substrate-runtime counterpart: name the
// agent's newest actor and its state and SPIFFE id, follow the kick's
// output as the bridge streams it, scrubbed line by line, with each actor
// state transition as a status line, then report the kick's exit code and
// whether the actor was suspended or deleted. A finished kick's actor is
// still named, above the persisted transcript.
func (s *Server) streamSubstrateActor(ctx context.Context, out *jobTerminalWriter, name string, reader sandbox.SubstrateLogReader) {
	info, found, err := reader.LatestActor(ctx, sandbox.LaunchName(name))
	if err != nil {
		out.status("cannot read Substrate actors for agent %q: %v", name, err)
		out.status("showing the last persisted transcript instead")
		s.writeSandboxTranscript(out, name, substrateTerms)
		return
	}
	if !found {
		s.writeSandboxTranscript(out, name, substrateTerms)
		return
	}
	out.status("substrate actor %s", describeSubstrateActor(info))
	if info.Finished() {
		out.status("%s", substrateActorEnd(info))
		s.writeSandboxTranscript(out, name, substrateTerms)
		return
	}
	out.status("%s", substrateLiveLogNote)
	last := info
	scrub := &kubejob.LineFilterWriter{Dst: out, Fn: scrubLogLine}
	final, err := reader.StreamRun(ctx, info.Name, s.jobTerminalPollInterval(), scrub, func(next sandbox.SubstrateActorInfo) {
		if next.State == last.State && next.SpiffeID == last.SpiffeID {
			return
		}
		last = next
		out.status("actor %s", describeSubstrateActor(next))
	})
	_ = scrub.Close()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		out.status("output stream ended with an error: %v", err)
	}
	if final.Name == "" {
		final.Name = info.Name
	}
	if final.Exited {
		out.status("kick exited with code %d", final.ExitCode)
	}
	out.status("%s", substrateActorEnd(final))
}

// describeSubstrateActor is "<name> · <STATE> · <spiffe id>", leaving out
// what the launcher has not learned yet.
func describeSubstrateActor(i sandbox.SubstrateActorInfo) string {
	parts := []string{i.Name, orUnknown(i.State)}
	if i.Atespace != "" {
		parts[0] = i.Atespace + "/" + i.Name
	}
	if i.SpiffeID != "" {
		parts = append(parts, i.SpiffeID)
	}
	return strings.Join(parts, " · ")
}

// substrateActorEnd says what became of the actor once the launcher was done
// with it, or that it has not finished.
func substrateActorEnd(i sandbox.SubstrateActorInfo) string {
	switch {
	case i.Suspended:
		return fmt.Sprintf("actor %s was suspended with a snapshot (snapshot_on_idle): the kick has finished. %s", i.Name, substrateKickResultNote)
	case i.Done:
		return fmt.Sprintf("actor %s was deleted by the launcher: the kick has finished. %s", i.Name, substrateKickResultNote)
	default:
		return fmt.Sprintf("actor %s · %s (exit code not reported). %s", i.Name, orUnknown(i.State), substrateKickResultNote)
	}
}

const (
	// substrateLiveLogNote opens every live substrate stream.
	substrateLiveLogNote = "this is the kick's stdout and stderr as the actor's bridge streams /run, scrubbed; actor state changes are shown as they happen"
	// substrateKickResultNote closes it.
	substrateKickResultNote = "the full output and exit code are in the persisted transcript; reopen this pane to see them"
)

const (
	// openshellLiveLogNote opens every live OpenShell stream so an operator
	// never mistakes the sandbox's event log for the kick's output.
	openshellLiveLogNote = "this is the sandbox's gateway log (supervisor, policy and tracing events), not the kick's output: " +
		"the kick runs as an exec session whose stdout and exit code are written to the transcript when it ends"
	// openshellKickResultNote closes it, pointing at where the kick's own
	// result lives.
	openshellKickResultNote = "the kick's output and exit code are in the persisted transcript; reopen this pane to see them"
)

// sandboxTerms are the runtime-specific words in the viewer's page and
// status lines; everything else is shared between the runtimes.
type sandboxTerms struct {
	title, about string
	// live names the per-kick workload; where says where it was looked for.
	live, where string
	// gone explains why it is normally absent between kicks.
	gone string
}

var (
	jobTerms = sandboxTerms{
		title: "sandbox job log",
		about: "the kick runs as a Kubernetes Job in its own pod, so this is the pod's log",
		live:  "Job pod", where: "in the namespace",
		gone: "a completed Job is deleted when its kick finishes",
	}
	openshellTerms = sandboxTerms{
		title: "openshell sandbox log",
		about: "the kick runs in an OpenShell sandbox on the gateway, so this is the sandbox's gateway event log while it runs and the kick's transcript afterwards",
		live:  "OpenShell sandbox", where: "on the gateway",
		gone: "the launcher deletes a sandbox when its kick finishes",
	}
	substrateTerms = sandboxTerms{
		title: "substrate actor log",
		about: "the kick runs in a Substrate actor, so this is the kick's output as the actor's bridge streams it while it runs and the kick's transcript afterwards",
		live:  "Substrate actor", where: "started by this hive process",
		gone: "the launcher deletes, or with snapshot_on_idle suspends, an actor when its kick finishes",
	}
)

func sandboxTermsFor(rt string) sandboxTerms {
	switch rt {
	case config.SandboxRuntimeOpenShell:
		return openshellTerms
	case config.SandboxRuntimeSubstrate:
		return substrateTerms
	}
	return jobTerms
}

// writeJobTranscript is the no-pod path: the last persisted transcript with
// its recorded exit code, or an explicit "no run yet".
func (s *Server) writeJobTranscript(out *jobTerminalWriter, name string) {
	s.writeSandboxTranscript(out, name, jobTerms)
}

func (s *Server) writeSandboxTranscript(out *jobTerminalWriter, name string, terms sandboxTerms) {
	root := s.jobTerminalWorkspaceRoot()
	path, ok := sandbox.LatestTranscript(root, name)
	if !ok {
		out.status("no sandbox run yet for agent %q: no %s %s and no transcript under %s. Kick the agent to start one.", name, terms.live, terms.where, root)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		out.status("no %s for agent %q right now; its last transcript %s could not be read: %v", terms.live, name, path, err)
		return
	}
	written := "unknown time"
	if st, err := os.Stat(path); err == nil {
		written = st.ModTime().UTC().Format(time.RFC3339)
	}
	out.status("no %s for agent %q right now (%s); showing the last run's transcript, written %s", terms.live, name, terms.gone, written)
	out.status("%s", path)
	text := string(data)
	// The executor already scrubbed this at write time; scrubbing again is
	// idempotent and guards transcripts written by other tooling.
	if _, err := io.WriteString(out, scrubLogLine(strings.TrimRight(text, "\n")+"\n")); err != nil {
		return
	}
	if code, ok := sandbox.TranscriptExitCode(text); ok {
		out.status("last run exited with code %d", code)
	} else {
		out.status("last run's exit code was not recorded in this transcript")
	}
}

func scrubLogLine(line string) string {
	// Same seam the agent log pane uses (crux.RedactPaneTokens), so the Job
	// log is redacted exactly like tmux pane output.
	return redactTokens(line)
}

func describePodPhase(p kubejob.PodInfo) string {
	switch {
	case p.Terminated:
		return fmt.Sprintf("%s (exit code %d)", orUnknown(p.Phase), p.ExitCode)
	case p.Waiting != "":
		return fmt.Sprintf("%s (%s)", orUnknown(p.Phase), p.Waiting)
	default:
		return orUnknown(p.Phase)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// jobTerminalWriter writes to the response and flushes after every write,
// so each line reaches the browser as it is produced. It satisfies
// kubejob.FlushWriter for LineFilterWriter.
type jobTerminalWriter struct {
	w http.ResponseWriter
}

func (j *jobTerminalWriter) Write(p []byte) (int, error) {
	n, err := j.w.Write(p)
	j.Flush()
	return n, err
}

func (j *jobTerminalWriter) Flush() {
	if f, ok := j.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (j *jobTerminalWriter) status(format string, args ...any) {
	_, _ = io.WriteString(j, jobTerminalMarker+fmt.Sprintf(format, args...)+"\n")
}

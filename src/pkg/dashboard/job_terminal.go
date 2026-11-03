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

// jobTerminalAgent resolves the path's agent and checks that its effective
// sandbox runtime is job. The status code says which precondition failed:
// 404 for an unknown agent, 409 for one that has a tmux terminal instead.
func (s *Server) jobTerminalAgent(r *http.Request) (string, int, string) {
	if s.deps == nil || s.deps.Config == nil {
		return "", http.StatusServiceUnavailable, "configuration unavailable"
	}
	name := s.resolveAgentParam(r.PathValue("name"))
	cfg := s.deps.Config
	agentCfg, ok := cfg.Agents[name]
	if !ok {
		return "", http.StatusNotFound, fmt.Sprintf("agent %q not found", name)
	}
	if !agentCfg.SandboxEnabled(cfg.AgentSandbox) {
		return "", http.StatusConflict, fmt.Sprintf("agent %q is not sandboxed; its terminal is the tmux session at /terminal/?arg=hive-%s", name, name)
	}
	if rt := agentCfg.SandboxRuntime(cfg.AgentSandbox); rt != config.SandboxRuntimeJob {
		return "", http.StatusConflict, fmt.Sprintf("agent %q runs sandbox kicks with runtime %q, not as a Kubernetes Job; there is no Job pod log to show", name, rt)
	}
	return name, http.StatusOK, ""
}

// handleJobTerminalPage serves the viewer document: a <pre> the inline
// script fills from the stream endpoint as lines arrive. The script is
// inline because /terminal documents carry the blanket script-src
// 'unsafe-inline' policy (securityHeaders) rather than the SPA's hash
// allowlist; no on*= attributes are used, matching the rest of the server.
func (s *Server) handleJobTerminalPage(w http.ResponseWriter, r *http.Request) {
	name, code, msg := s.jobTerminalAgent(r)
	if code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	esc := html.EscapeString(name)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, jobTerminalPageHTML, esc, esc, esc)
}

// %s placeholders: agent (title), agent (heading), agent (stream URL path
// segment). The stream URL is relative so the page works behind any prefix
// the dashboard host is served at.
const jobTerminalPageHTML = `<!DOCTYPE html><html><head><meta charset="utf-8"><title>%s — sandbox job log</title>
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
<header><h1>▶ %s — sandbox job log</h1><span class="muted">read-only: the kick runs as a Kubernetes Job in its own pod, so this is the pod's log, not a shell</span><span id="status" class="muted">connecting…</span><button id="reconnect" type="button">⟳ reconnect</button></header>
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
	name, code, msg := s.jobTerminalAgent(r)
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

// writeJobTranscript is the no-pod path: the last persisted transcript with
// its recorded exit code, or an explicit "no run yet".
func (s *Server) writeJobTranscript(out *jobTerminalWriter, name string) {
	root := s.jobTerminalWorkspaceRoot()
	path, ok := sandbox.LatestTranscript(root, name)
	if !ok {
		out.status("no sandbox run yet for agent %q: no Job pod in the namespace and no transcript under %s. Kick the agent to start one.", name, root)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		out.status("no Job pod for agent %q right now; its last transcript %s could not be read: %v", name, path, err)
		return
	}
	written := "unknown time"
	if st, err := os.Stat(path); err == nil {
		written = st.ModTime().UTC().Format(time.RFC3339)
	}
	out.status("no Job pod for agent %q right now (a completed Job is deleted when its kick finishes); showing the last run's transcript, written %s", name, written)
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

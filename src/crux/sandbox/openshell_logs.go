package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	openshell "github.com/ai-openshell/sdk-go/openshell/v1"
)

// OpenShell sandbox log reader (hivecommons/hive#17645).
//
// An openshell-runtime agent has no tmux session in the hive pod, exactly
// like a job-runtime one, so the dashboard's read-only kick viewer needs a
// way to find the agent's live sandbox and follow its log. This is that
// seam: plain hive types in, plain text out, so pkg/dashboard never imports
// the OpenShell SDK and its tests drive a fake instead of a gateway.
//
// What the log is, and is not. GetLogs returns the gateway's in-memory
// buffer of supervisor, policy (OCSF) and tracing events for the sandbox
// (≤ 2,000 lines per sandbox, see openshellLogLinesPerRead). The kick
// itself runs as an exec session (OpenShellLauncher.Run → Exec().Run), and
// exec output reaches neither this buffer nor the container log — see
// "Accessing Logs" in the OpenShell docs. So this reader shows what the
// sandbox did around the kick (network and filesystem decisions, supervisor
// state), not the kick's own stdout, which the launcher collects from the
// exec result and the executor persists to the transcript when the kick
// ends. The dashboard says so in its status lines.

// openshellLogLinesPerRead caps each GetLogs read. The gateway keeps at
// most 2,000 lines per sandbox, so asking for that many returns the whole
// buffer instead of the server's smaller default and keeps a chatty
// sandbox from losing lines between two polls.
const openshellLogLinesPerRead uint32 = 2000

// OpenShellPhaseDeleting is the Phase OpenShellSandboxInfo carries once the
// sandbox is being deleted or is already gone (NotFound). In the normal
// lifecycle that is how a kick ends: the launcher deletes the sandbox the
// moment Exec().Run returns.
const OpenShellPhaseDeleting = string(openshell.SandboxDeleting)

// OpenShellSandboxInfo is what the viewer needs to know about a sandbox.
type OpenShellSandboxInfo struct {
	Name  string
	Phase string
	// Exited is true once the gateway reports an exit code for the
	// sandbox's main process (Sandbox.Status.ExitCode). That is the
	// sandbox workload, not the kick: the kick runs as an exec session and
	// its exit code is only in the launcher's Result and the persisted
	// transcript. In the normal lifecycle the launcher deletes the sandbox
	// as soon as the kick ends, so a viewer usually sees NotFound rather
	// than an exit code here.
	Exited   bool
	ExitCode int
}

// Finished reports whether the sandbox will produce no more output.
func (i OpenShellSandboxInfo) Finished() bool {
	if i.Exited {
		return true
	}
	switch openshell.SandboxPhase(i.Phase) {
	case openshell.SandboxCompleted, openshell.SandboxError, openshell.SandboxStopped, openshell.SandboxDeleting:
		return true
	}
	return false
}

// OpenShellLogReader is the dashboard's view of an OpenShell gateway.
// *OpenShellLauncher implements it.
type OpenShellLogReader interface {
	// LatestSandbox finds the newest live sandbox the launcher created for
	// launchName (see LaunchName). found is false when none is running —
	// the launcher deletes a sandbox as soon as its kick finishes, so that
	// is the normal state between kicks.
	LatestSandbox(ctx context.Context, launchName string) (info OpenShellSandboxInfo, found bool, err error)
	// StreamLogs writes the sandbox's gateway log (supervisor, policy and
	// tracing events — not the kick's exec output, see the package note
	// above) to w one line at a time, polling the gateway every poll until
	// the sandbox finishes, disappears or ctx ends, and returns the last
	// state it observed.
	StreamLogs(ctx context.Context, sandboxName string, poll time.Duration, w io.Writer) (OpenShellSandboxInfo, error)
}

var _ OpenShellLogReader = (*OpenShellLauncher)(nil)

// openshellLogSandboxes is the read slice of the SDK's SandboxInterface the
// log reader needs on top of openshellSandboxes.
type openshellLogSandboxes interface {
	Get(ctx context.Context, workspace, name string) (*openshell.Sandbox, error)
	ListAll(ctx context.Context, workspace string, opts ...openshell.ListOptions) ([]*openshell.Sandbox, error)
	GetLogs(ctx context.Context, workspace, sandboxName string, opts ...openshell.LogOption) (*openshell.LogResult, error)
}

func (l *OpenShellLauncher) logClient() (openshellClient, openshellLogSandboxes, error) {
	if strings.TrimSpace(l.Options.GatewayURL) == "" {
		return nil, nil, errors.New("openshell: gateway_url is not configured")
	}
	client, err := l.client()
	if err != nil {
		return nil, nil, err
	}
	sb, ok := client.Sandboxes().(openshellLogSandboxes)
	if !ok {
		_ = client.Close()
		return nil, nil, errors.New("openshell: client cannot read sandbox logs")
	}
	return client, sb, nil
}

// LatestSandbox implements OpenShellLogReader.
func (l *OpenShellLauncher) LatestSandbox(ctx context.Context, launchName string) (OpenShellSandboxInfo, bool, error) {
	client, sb, err := l.logClient()
	if err != nil {
		return OpenShellSandboxInfo{}, false, err
	}
	defer func() { _ = client.Close() }()
	all, err := sb.ListAll(ctx, l.Options.workspace(), openshell.ListOptions{LabelSelector: openshellManagedLabel + "=" + openshellManagedValue})
	if err != nil {
		return OpenShellSandboxInfo{}, false, fmt.Errorf("openshell: listing sandboxes: %w", err)
	}
	var (
		best     *openshell.Sandbox
		bestUnix int64
	)
	for _, s := range all {
		if s == nil || s.DeletionTimestamp != nil {
			continue
		}
		unix, ok := openshellSandboxNameUnix(launchName, s.Name)
		if !ok {
			continue
		}
		if best == nil || unix > bestUnix {
			best, bestUnix = s, unix
		}
	}
	if best == nil {
		return OpenShellSandboxInfo{}, false, nil
	}
	return openshellSandboxInfo(best), true, nil
}

// openshellSandboxNameUnix reports whether name is one openshellSandboxName
// built for launchName, and the timestamp it carries. Matching by rebuilding
// the name keeps one naming rule: "deps" never matches "deps-bot"'s sandbox.
func openshellSandboxNameUnix(launchName, name string) (int64, bool) {
	i := strings.LastIndexByte(name, '-')
	if i < 0 || !strings.HasPrefix(name, openshellNamePrefix) {
		return 0, false
	}
	unix, err := strconv.ParseInt(name[i+1:], openshellNameUnixBase, 64)
	if err != nil {
		return 0, false
	}
	if openshellSandboxName(launchName, time.Unix(unix, 0)) != name {
		return 0, false
	}
	return unix, true
}

func openshellSandboxInfo(s *openshell.Sandbox) OpenShellSandboxInfo {
	info := OpenShellSandboxInfo{Name: s.Name, Phase: string(s.Status.Phase)}
	if s.DeletionTimestamp != nil && info.Phase == "" {
		info.Phase = OpenShellPhaseDeleting
	}
	if s.Status.ExitCode != nil {
		info.Exited = true
		info.ExitCode = int(*s.Status.ExitCode)
	}
	return info
}

// StreamLogs implements OpenShellLogReader. GetLogs is a snapshot, not a
// stream, so following is a poll with WithLogSince; the gateway's "since"
// is inclusive, so lines at the last seen timestamp are de-duplicated.
func (l *OpenShellLauncher) StreamLogs(ctx context.Context, sandboxName string, poll time.Duration, w io.Writer) (OpenShellSandboxInfo, error) {
	info := OpenShellSandboxInfo{Name: sandboxName}
	client, sb, err := l.logClient()
	if err != nil {
		return info, err
	}
	defer func() { _ = client.Close() }()
	if poll <= 0 {
		poll = openshellReadyPoll
	}
	ws := l.Options.workspace()
	var (
		since  time.Time
		atLast = map[string]bool{}
	)
	drain := func() error {
		opts := []openshell.LogOption{openshell.WithLogLines(openshellLogLinesPerRead)}
		if !since.IsZero() {
			opts = append(opts, openshell.WithLogSince(since))
		}
		res, err := sb.GetLogs(ctx, ws, sandboxName, opts...)
		if err != nil {
			return err
		}
		if res == nil {
			return nil
		}
		for _, line := range res.Lines {
			if line.Timestamp.Before(since) {
				continue
			}
			key := line.Source + "\x00" + line.Level + "\x00" + line.Target + "\x00" + line.Message
			if line.Timestamp.Equal(since) {
				if atLast[key] {
					continue
				}
			} else {
				since, atLast = line.Timestamp, map[string]bool{}
			}
			atLast[key] = true
			if _, err := io.WriteString(w, formatOpenShellLogLine(line)); err != nil {
				return err
			}
		}
		return nil
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		logErr := drain()
		s, err := sb.Get(ctx, ws, sandboxName)
		switch {
		case err != nil && openshell.IsNotFound(err):
			// Deleted by the launcher after its kick: nothing more to read.
			info.Phase = OpenShellPhaseDeleting
			return info, nil
		case err != nil:
			return info, err
		}
		info = openshellSandboxInfo(s)
		if logErr != nil && !openshell.IsNotFound(logErr) {
			return info, logErr
		}
		if info.Finished() {
			// Lines written between the last read and the exit.
			if err := drain(); err != nil && !openshell.IsNotFound(err) {
				return info, err
			}
			return info, nil
		}
		select {
		case <-ctx.Done():
			return info, ctx.Err()
		case <-ticker.C:
		}
	}
}

func formatOpenShellLogLine(line openshell.LogLine) string {
	var b strings.Builder
	if !line.Timestamp.IsZero() {
		b.WriteString(line.Timestamp.UTC().Format(time.RFC3339))
		b.WriteByte(' ')
	}
	if line.Source != "" {
		b.WriteString("[" + line.Source + "] ")
	}
	if line.Level != "" {
		b.WriteString(line.Level + " ")
	}
	b.WriteString(strings.TrimRight(line.Message, "\n"))
	b.WriteByte('\n')
	return b.String()
}

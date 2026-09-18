package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/pushbroker"
	"github.com/hivecommons/hive/pkg/sandbox"
	"github.com/hivecommons/hive/pkg/watchdog"
)

type ProcessState string

const (
	StateIdle    ProcessState = "idle"
	StateRunning ProcessState = "running"
	StateStopped ProcessState = "stopped"
	StateFailed  ProcessState = "failed"
	StatePaused  ProcessState = "paused"
)

type KickRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Agent     string    `json:"agent"`
	Snippet   string    `json:"snippet"`
}

const (
	outputBufferCapacity = 500
	kickHistoryCapacity  = 50
	tmuxCaptureLines     = 2000
	proxyListenPort      = 18443
	proxyCACertPath      = "/data/proxy-ca.pem"

	// fullLogCaptureLines bounds the "download/view full log" capture (see
	// CaptureFullLog). tmux's -S - captures the entire scrollback, but a wedged
	// agent that has spammed for hours can hold a very large buffer; this caps the
	// number of lines pulled back from the tail so the endpoint stays bounded.
	// It matches defaultTmuxHistoryLimit — the history-limit agent sessions are
	// created with — so in practice it returns the WHOLE retained session.
	fullLogCaptureLines = 50000

	// tmuxHistoryLimitEnv overrides the scrollback depth (in lines) that agent
	// tmux sessions are created with (see newSessionCommands).
	tmuxHistoryLimitEnv = "HIVE_TMUX_HISTORY_LIMIT"

	// defaultTmuxHistoryLimit is the history-limit applied when creating an
	// agent's tmux session. tmux's own default is only 2000 lines, which capped
	// both browser copy-mode scrollback (#3694) and the "full log" capture
	// (#3693) at ~2000 lines no matter how deep CaptureFullLog reached. Matches
	// fullLogCaptureLines so the full-log endpoint can return the entire
	// retained buffer.
	defaultTmuxHistoryLimit = fullLogCaptureLines

	// tmuxPaneWidthEnv overrides the column count agent tmux sessions are
	// created with (see newSessionCommands).
	tmuxPaneWidthEnv = "HIVE_TMUX_PANE_WIDTH"

	// defaultTmuxPaneWidth is the column count agent tmux panes are created
	// with (#3878). tmux gives a DETACHED session (new-session -d, which is how
	// every agent session is created) a default pane of 80x24, because there is
	// no attached client whose terminal size it could adopt. The agent CLI
	// renders its tool-call lines — "Bash(git log --oneline …)" — to fit the
	// pane it sees, TRUNCATING with an ellipsis rather than wrapping. That
	// truncation happens at render time, inside the CLI, before any byte
	// reaches the scrollback: capture-pane -J rejoins wrapped lines but cannot
	// recover characters the CLI never emitted. So an 80-column pane means long
	// bash commands are permanently unrecoverable from the log, which is
	// exactly the debugging wall reported in #3878.
	//
	// 500 columns is chosen to sit well beyond the longest tool invocations
	// observed in practice (multi-flag kubectl/gh/git pipelines run 200-300
	// columns) while staying a bounded, sane terminal geometry. It costs
	// nothing at rest: tmux allocates scrollback per line by actual content
	// length, not by pane width, so a wide pane does not inflate memory for
	// short lines.
	defaultTmuxPaneWidth = 500

	// defaultTmuxPaneHeight is the row count agent tmux panes are created with.
	// It only needs to exceed a normal terminal screenful — scrollback depth is
	// governed by history-limit, not by pane height — but tmux requires -y
	// whenever -x is given, so it is pinned here rather than left to the 24-row
	// default.
	defaultTmuxPaneHeight = 50

	// tmuxStatusRight is the status line agent tmux sessions carry (#4399).
	//
	// THE PROBLEM IT SOLVES. Two things sat in the top-right of the browser
	// terminal and neither said what it was:
	//
	//   * tmux's DEFAULT status-right is a live WALL CLOCK
	//     (`"#{=21:pane_title}" %H:%M %d-%b-%y`). An operator reasonably read it
	//     as a timestamp OF THE CONTENT and tried to line it up with the
	//     scrollback — which can never work, because it is simply the time now.
	//   * copy-mode draws a black-on-yellow `[position/total]` counter (tmux's
	//     `mode-style bg=yellow,fg=black`). That is the only on-screen hint that
	//     the pane is scrolled back, and it looks like a line counter, not a
	//     warning.
	//
	// And copy-mode is the important state: while a pane is in it the pane
	// STOPS FOLLOWING LIVE OUTPUT. #3694 deliberately turned mouse mode on so
	// the wheel scrolls history, which means an operator reaches that state by
	// doing the most natural thing in a terminal. Worse, copy-mode is PANE
	// state held by the tmux server, so closing the browser tab and reopening
	// it re-attaches to a pane that is still frozen — exactly the "no more
	// output appeared, and reopening showed no more output" report in #4399.
	//
	// So: say which state the pane is in, and label the clock as the current
	// time rather than leaving it to be misread as a content timestamp.
	// Deliberately plain ASCII with no `#[...]` style blocks — style specs are
	// comma-separated and a comma inside a `#{?...}` branch has to be escaped,
	// which is exactly the kind of format-string subtlety that renders as
	// garbage instead of failing loudly.
	//
	// The SCROLLBACK branch also carries the scroll position
	// (`#{scroll_position}/#{history_size} lines back`) because the wheel
	// rebind below hides tmux's own black-on-yellow marker — this is where
	// that information now lives, with a label.
	//
	// THE THIRD STATE (#4681). The two branches above are not exhaustive, and
	// the gap swallowed the position indicator entirely for the panes that
	// matter most. The wheel rebind below only enters copy-mode when the
	// running program has NOT grabbed the mouse:
	//
	//	if -F '#{||:#{pane_in_mode},#{mouse_any_flag}}' 'send-keys -M' 'copy-mode -eH'
	//
	// Every agent CLI is a full-screen TUI that turns mouse reporting ON, so
	// `mouse_any_flag` is 1 and the wheel is forwarded to the APPLICATION. The
	// pane therefore never enters copy-mode, `pane_in_mode` stays 0, and the
	// status line reported a bare `[live]` however far back the operator had
	// scrolled inside the agent's own buffer. Combined with the `-H` that hides
	// tmux's native `HH:MM [pos/total]` marker, that left no position and no
	// content timestamp anywhere on screen — #4681's "I never saw an indication
	// of my scroll position or timestamp", and why it read as a regression:
	// before `-H` the native marker was at least SOMETIMES there.
	//
	// tmux genuinely cannot report a position here — the scrollback being moved
	// belongs to the application, not to the pane — so this says exactly that
	// instead of claiming `[live]`. Naming the reason is the point: `[live]`
	// asserts the viewport is at the bottom of live output, which is false and
	// unfalsifiable from the operator's side.
	//
	// The `#,` is an ESCAPED COMMA. tmux splits `#{?cond,a,b}` on unescaped
	// commas, so prose commas inside a branch must be written `#,` or the
	// branch silently truncates at the comma.
	tmuxStatusRight = "#{?pane_in_mode,[SCROLLBACK #{scroll_position}/#{history_size} lines back - not following live output - press q to resume] ,#{?mouse_any_flag,[live - this app handles its own scrolling#, so tmux has no line position] ,[live] }}now %H:%M:%S "

	// tmuxStatusRightLength bounds how many columns status-right may occupy.
	// tmux's DEFAULT is 40, which silently truncated the message above to
	// "[SCROLLBACK - not following live outp…" — losing both the "press q to
	// resume" instruction and the labelled clock. The truncation is invisible
	// to `display-message -p` (which expands the format without applying the
	// length limit), which is how it escaped the original #4439 render test;
	// the test now renders through a real attached client. 140 comfortably
	// fits the longest expansion (position counters included) with headroom.
	tmuxStatusRightLength = 140

	// tmuxStatusInterval is how often (seconds) tmux redraws the status line.
	// tmux's default is 15s, which would leave the SCROLLBACK marker above up
	// to 15 seconds stale — long enough for an operator to scroll, see nothing
	// change, and conclude the terminal is broken.
	tmuxStatusInterval = 2

	// tmuxWheelBindingKey/...Cond/...Then/...Else rebind the mouse wheel's
	// copy-mode entry (#4399). tmux's default WheelUpPane binding is
	//
	//   if -F '#{||:#{pane_in_mode},#{mouse_any_flag}}' { send -M } { copy-mode -e }
	//
	// and plain `copy-mode` draws tmux's built-in black-on-yellow marker in
	// the pane's top-right: `<time> [position/total]`, where <time> is the
	// WRITE TIME OF THE TOP VISIBLE LINE (tmux window-copy.c, gl->time) — a
	// reference point no operator could be expected to guess, and the exact
	// "timestamp whose reference point is unintelligible" reported in #4399.
	// The rebind is byte-for-byte tmux's default with `-H` added (hide the
	// marker; tmux >= 3.2), because the same information now appears LABELLED
	// in the status line above. Bound server-wide on the agent's own private
	// tmux server (each agent runs its own socket under its own UID), so no
	// operator tmux is touched.
	tmuxWheelBindingKey  = "WheelUpPane"
	tmuxWheelBindingCond = "#{||:#{pane_in_mode},#{mouse_any_flag}}"
	tmuxWheelBindingThen = "send-keys -M"
	tmuxWheelBindingElse = "copy-mode -eH"
)

// BreakerTrigger is the distinct PausedTrigger stamped on every pause the fleet
// breaker performs. It serves two jobs: the audit log attributes the pause to
// the breaker, and ReleaseBreaker uses it as the guard that distinguishes a
// pause the breaker still owns (safe to auto-resume) from one an operator
// re-applied during the breaker window (must stay paused). Anything whose
// current PausedTrigger != BreakerTrigger was last paused by something else.
const BreakerTrigger = "fleet-breaker"

type AgentProcess struct {
	Name          string
	ID            string
	Config        config.AgentConfig
	State         ProcessState
	PID           int
	UID           int
	StartedAt     *time.Time
	LastKick      *time.Time
	Paused        bool
	PausedAt      time.Time
	PausedReason  string
	PausedTrigger string
	// PausedBy is the acting user behind the pause when one is known — the
	// authenticated dashboard user for a dashboard-api pause, empty for
	// system-initiated pauses (login-detector, fleet-breaker, acmm-pack).
	// It exists because trigger+reason alone made a deliberate owner
	// quiesce indistinguishable from a malfunction days later (#4041): the
	// audit log had the actor, but nothing the UI or the fleet view reads
	// carried it.
	PausedBy          string
	PinnedCLI         string
	PinnedModel       string
	ModelOverride     string
	BackendOverride   string
	RestartCount      int
	RestartEvents     []RestartEvent
	LastRestartReason string
	OutputBuffer      *RingBuffer
	lastPaneCapture   []string
	paneMu            sync.RWMutex
	KickHistory       []KickRecord
	LastKickMessage   string
	KickRefused       bool
	KickRefusalReason string
	LaunchedMode      AgentMode
	HasLaunched       bool
	tmuxSession       string
	tmuxSocket        string
	cancel            context.CancelFunc
	forceRelaunch     bool
	// launching is set true under m.mu while Start runs this agent's launch
	// with m.mu RELEASED (so a slow /data NFS write or a hung MITM-proxy token
	// mint during launch cannot block AllStatuses()/the heartbeat collect() and
	// flap /api/livez). It is cleared under m.mu when the launch finishes on
	// every path (success, error, or panic — via a deferred clear). It exists
	// solely to serialize concurrent Start(sameName): with m.mu no longer held
	// across the launch, a second Start would otherwise race the first one's
	// tmux launch and its guarded-field writes. Guarded by m.mu.
	launching         bool
	BootstrapOverride string // when set, replaces buildBootstrapPrompt output
	LastError         string // captured from bare copilot diagnostic launch
	// kickDelivering is true for exactly as long as deliverKickLocked is
	// typing a kick into this agent's pane. Delivery is NOT instantaneous: a
	// kick is typed as 400-rune chunks with a pause between them, so a 37KB
	// governor kick occupies the pane for ~100s. For that whole window the
	// CLI's idle chrome is scrolled out of the captured pane, which makes the
	// pane poller's health predicates read a perfectly healthy agent as dead
	// (#7169: "copilot hung with no CLI prompt" fired 68s into a scanner kick,
	// recreated the tmux session, and the remaining chunks landed nowhere —
	// eleven consecutive "send-keys failed" lines followed by a restart that
	// threw the kick away; it happened five times before it was caught).
	//
	// Nothing that destroys or replaces the pane may run while this is set.
	//
	// An atomic, not an m.mu-guarded field, on purpose: deliverKickLocked runs
	// with m.mu HELD for the entire delivery, so a poller that had to take
	// m.mu to read this would serialize behind the very delivery it is trying
	// to observe.
	kickDelivering   atomic.Bool
	lastTokenRestart time.Time // cooldown for auto-restart after token detection
	// tokenRestartAttempts counts CONSECUTIVE token-triggered restarts that did
	// not clear the login prompt. The restart is a falsifiable theory — "a valid
	// token exists, the agent just has not picked it up yet" — and this is what
	// makes it falsifiable: it is incremented when such a restart is issued and
	// reset the moment the pane stops showing a login prompt. Once the theory
	// has failed tokenRestartMaxAttempts times the restart stops firing, because
	// something the restart cannot reach is holding the agent at the prompt
	// (#4596: the credential is valid but $HOME/.claude.json carries no
	// signed-in identity). Written only by this agent's pane poller, like
	// lastTokenRestart beside it.
	tokenRestartAttempts int
	// tokenRestartGaveUp latches once the cap is hit so the diagnosis is logged
	// a single time rather than every poll (~3s). Cleared alongside the counter.
	tokenRestartGaveUp bool
	NeedsLogin         bool // true when pane shows a login prompt
	QuotaExhausted     bool // true when pane shows provider/monthly quota exhaustion
	// WatchdogConditions is the k8s-style observed-health condition set the
	// watchdog reconciler publishes for this agent (RFC #4665): Ready /
	// Authenticated / Producing with lastTransitionTime + reason. Written by
	// the reconciler via SetConditions, read by snapshot() for the dashboard.
	// Guarded by paneMu, like the poller-written observation fields beside it.
	WatchdogConditions []watchdog.Condition
	// LastPaneChange is when the agent's tmux pane content last CHANGED, as
	// observed by the 3s pane poller. It is the spoke's only evidence of an
	// agent actually doing something: State says what the manager intends,
	// StartedAt says when the CLI launched, and LastKick says when the
	// governor last spoke to it — none of them move when a running,
	// authenticated CLI sits there producing nothing. Written under paneMu by
	// pollTmuxOutputForAgent alongside lastPaneCapture; zero until the poller
	// has seen two differing captures, which reads as "unknown", never "idle".
	LastPaneChange       time.Time
	consentSeenAt        time.Time // watcher: when a consent screen was first seen in the pane
	lastConsentDismiss   time.Time // watcher: cooldown for re-running dismissInferencePrompts
	lastInferKickAt      time.Time // stall watchdog: when the last kick was delivered to an inference agent
	lastInferKickPane    string    // stall watchdog: hash of the visible pane just after kick delivery
	lastInferKickVisible string    // stall watchdog: visible pane text just after kick delivery
	stallNudgeSent       bool      // stall watchdog: at most one nudge per kick
	StallNudges          int       // total post-kick stall nudges sent (surfaced to the dashboard)
	// Transient API-error recovery (#4697), for CLI backends. lastTransientNudge
	// is the cooldown anchor — the poller runs every 3s and the error text stays
	// on screen after the nudge is typed, so without it one incident would fire
	// a nudge per tick. transientNudgesThisKick is the per-kick cap; both it and
	// the cooldown reset on the next kick.
	lastTransientNudge          time.Time
	transientNudgesThisKick     int
	TransientNudges             int // total transient-API-error nudges sent (surfaced to the dashboard)
	launchGen                   int // increments per launch; stale deliverStartupKick goroutines check it and drop
	lastInferKickMarks          int // no-action watchdog: tool-marker count in pane+scrollback just after kick delivery
	ProviderErrorClass          string
	ProviderErrorLine           string
	ProviderErrorBackoffUntil   time.Time
	providerErrorBackoffAttempt int
	// BackendAuth is the per-agent backend-auth canary (#6558), derived from
	// the same classifyProviderError verdict as ProviderErrorClass above and
	// guarded by the same lock (Manager.mu). See backend_auth.go.
	BackendAuth BackendAuthState
	// kickLogPending is true while the current tmux session holds kick output
	// that has not yet been archived to a per-kick log file (see
	// kick_logs.go). Set after every kick delivery; cleared when the
	// scrollback is archived (next kick, restart, shutdown). Guarded by m.mu.
	kickLogPending bool
	// TurnLoss accumulates what teardowns have discarded from this agent's
	// in-flight turns (#4002 open question 3): RestartCount says how many
	// restarts happened, this says what they cost. Guarded by m.mu, persisted
	// through snapshot.AgentState so it survives the restart it measures. See
	// turn_loss.go.
	TurnLoss        TurnLoss
	actionNudgeSent bool // no-action watchdog: at most one action nudge per kick
	ActionNudges    int  // total prose-only-response action nudges sent (surfaced to the dashboard)
	// KickOutcome is how the most recent kicked turn ENDED (#7421): a
	// clarifying question, a policy stand-down, an explicit nothing-produced
	// report, or plain "ended". Settled(LastKick) is false while the current
	// turn is still running. Guarded by m.mu; see kick_outcome.go.
	KickOutcome KickOutcome
	// sandboxResumeAfterCancel is set when an operator resumes a paused
	// sandbox agent while the canceled sandbox goroutine is still draining.
	// The completion handler then turns the expected cancellation into Idle
	// instead of Failed.
	sandboxResumeAfterCancel bool

	// awaitingBobKey marks an agent that launchInTmux parked in StateFailed
	// for the single, fully-recoverable reason "bob backend with no API key".
	// It is what makes RelaunchBobAgentsAwaitingKey precise: StateFailed alone
	// is ambiguous (a missing backend binary, a copilot auth timeout, and a
	// hung diagnostic all land there too), and relaunching those on a bob-key
	// save would restart agents whose problem the key does not fix.
	// Set only on the missing-key branch, cleared on every launch attempt.
	awaitingBobKey bool

	// Start-failure record (#5958, incident #5921). StartFailureClass is the
	// stable kind, StartFailureReason the operator-facing sentence, and
	// StartFailureCount how many CONSECUTIVE failures of that same class have
	// happened. StartBlocked is set once the count reaches
	// startFailureBlockThreshold(); StartBackoffUntil paces the automatic
	// relaunch loop from the first failure onward. See start_failure.go — the
	// mechanism deliberately mirrors the ProviderError* fields above.
	StartFailureClass    string
	StartFailureReason   string
	StartFailureCount    int
	StartFailureLastAt   time.Time
	StartFailureExitCode *int
	StartFailureSignal   string
	StartBlocked         bool
	StartBackoffUntil    time.Time

	// lastLaunchFailureBanner is the exact in-pane shell line typed by the most
	// recent aborted launch (see announceLaunchFailureInPane), "" after a
	// successful launch. A launch aborted before send-keys used to leave a
	// BARE interactive shell with the only explanation in the hive log — an
	// operator attached via ttyd saw a silent prompt and nothing else
	// (observed live: backend "bob" on a hive whose launch was refused). The
	// pane itself is the production surface for the banner; this field exists
	// so tests can assert the announcement actually happened without a tmux
	// server.
	lastLaunchFailureBanner string
	// kickEpoch increments on every restart TEARDOWN (not launch): a kick that
	// captured an older epoch while waiting for the input prompt is dropped
	// instead of being typed into the relaunched session (#7363). launchGen is
	// not reused for this because it only moves on a COMPLETED launch — a
	// pending kick must die the moment the operator's restart begins.
	kickEpoch int
	// kickHoldUntil / kickHoldReason are the restart/kick loop breaker (#7363):
	// a restart that destroyed a PRODUCING turn arms a short hold during which
	// SendKick/SendKickAsync refuse with a reason, so the restart cannot be
	// followed within seconds by a kick that will itself be restarted.
	kickHoldUntil  time.Time
	kickHoldReason string
}

// effectiveBackend returns the agent's backend accounting for any override.
func effectiveBackend(agent *AgentProcess) string {
	if agent.BackendOverride != "" {
		return agent.BackendOverride
	}
	return agent.Config.Backend
}

// ProjectContext holds project-level config injected into agent boot prompts.
type ProjectContext struct {
	Org             string
	Repos           []string
	PrimaryRepoName string
	ACMMLevel       int
	PRsAllowed      bool
	PolicyDir       string
	// GHHost is the bare hostname of the source forge when it is NOT public
	// github.com (e.g. "github.ibm.com" for a GHE spoke), derived from the
	// configured github.api_url. Exported to agents as GH_HOST so the gh CLI
	// targets the right host — without it every agent gh call went to
	// api.github.com where the project's repos do not exist (root-caused live
	// 2026-08-20: the security agent's issue/PR creation failed silently on
	// every GHE-hosted hive). Empty ⇒ public github.com, nothing exported.
	GHHost string
	// AppAuthoredPRs mirrors config github.app_authored_prs: when true, push-
	// capable agents get the App installation token as GITHUB_TOKEN so the GitHub
	// MCP server authors PRs/commits as the App bot. Default false → no token is
	// injected and behavior is unchanged (opt-in per hive).
	AppAuthoredPRs bool
}

func (p ProjectContext) PrimaryRepo() string {
	if strings.TrimSpace(p.PrimaryRepoName) != "" {
		return strings.TrimPrefix(p.PrimaryRepoName, p.Org+"/")
	}
	if len(p.Repos) > 0 {
		return strings.TrimPrefix(p.Repos[0], p.Org+"/")
	}
	return ""
}

type Manager struct {
	agents   map[string]*AgentProcess
	idToName map[string]string
	mu       sync.RWMutex
	// thrashMu guards thrash — its own mutex, NEVER m.mu: the breaker runs on
	// the output-capture goroutines, and taking m.mu there risks the startup
	// re-entrancy deadlock class (see the 2026-07 provisionWG incident).
	thrashMu sync.Mutex
	thrash   map[string]*thrashState
	// statusSnapshots backs GetStatusFast. Own mutex, NEVER m.mu — its whole
	// purpose is to be readable while m.mu is held by a restart (#7417).
	statusSnapMu sync.RWMutex
	statusSnaps  map[string]*AgentProcess
	// consentWedges records consent-screen restarts for the heartbeat's
	// ConsentWedged signal (#5577). Own mutex, NEVER m.mu — the recording
	// sites run with m.mu held. Zero value ready.
	consentWedges                 consentWedgeTracker
	logger                        *slog.Logger
	workDir                       string
	project                       ProjectContext
	copilotAuthToken              string
	copilotAuthTokenAuthoritative bool
	// copilotAuthTokenSource names where copilotAuthToken came from — one of
	// the CopilotTokenSource* constants, "" when no token is held. Never a
	// secret: it is the ONE thing a "not licensed" verdict cannot be triaged
	// without (#7302). GitHub's rejection is a statement about whichever
	// credential hive presented; an owner reading "check the account's seat"
	// after a successful dashboard login cannot tell whether the rejected
	// credential was their login, a provisioned COPILOT_GITHUB_TOKEN, or a
	// stale identity inherited from the shared CLI config.
	copilotAuthTokenSource string
	// copilotAuthTokenRejected records that the currently-held authoritative
	// Copilot token has been observed being rejected upstream by GitHub's
	// Copilot API ("not licensed to use Copilot" — #6500/#6767). Once set, the
	// reconciler stops treating the authoritative claim as inviolate: a
	// different, real token seen in the shared CLI config (an in-agent
	// /login recovery) is PROMOTED over the known-bad authoritative token
	// instead of being clobbered by it 30 s later. Cleared whenever
	// setCopilotToken installs a value that differs from the current one, so
	// a successful recovery (or dashboard re-login) rearms authoritative
	// precedence for whatever fresh token the operator supplied.
	copilotAuthTokenRejected bool
	claudeAuthToken          string
	uidMap                   *UIDMap
	appAuth                  AppTokenMinter
	agentMint                AgentMintIssuer // optional, opt-in mint credential (nil ⇒ off)

	// bobAPIKeyResolver resolves the IBM bobshell API key at LAUNCH time (not
	// boot), so a key an operator adds via a Secret/PVC file or the config UI
	// takes effect without restarting the hive. Returns "" when unconfigured.
	//
	// Stored as an atomic.Pointer, NOT under m.mu, for the same reason as
	// isGatewayBackend above: it is read from launchInTmux/agentEnvPairs, which
	// already hold m.mu.Lock(). Re-locking a non-reentrant RWMutex on the same
	// goroutine would deadlock startup before MarkReady and crash-loop every
	// spoke. An atomic read is lock-free and safe from any lock context.
	bobAPIKeyResolver atomic.Pointer[func() string]

	// linearCredentialResolver resolves the Linear write credential handed to
	// ISSUES_ONLY+ agents at LAUNCH and token-refresh time — the Linear
	// analogue of the App token pushed as GITHUB_TOKEN. Resolved live (not at
	// boot) so a workspace connected from the dashboard after startup reaches
	// agents on their next launch / hourly refresh without a restart. Same
	// atomic.Pointer discipline and deadlock reasoning as bobAPIKeyResolver.
	linearCredentialResolver atomic.Pointer[func() LinearCredential]

	// explainModeDefaultResolver returns the hive-wide default explain mode
	// (governor.explain_mode, falling back to HIVE_EXPLAIN_MODE) at KICK and
	// LAUNCH time rather than at boot, so an operator who turns explanation on
	// from the dashboard while debugging sees it on the next kick instead of
	// after a restart. Returns "" when no resolver was injected (tests / bare
	// setups), which leaves resolveExplainMode on its env-only path.
	//
	// Same atomic.Pointer discipline and same deadlock reasoning as
	// bobAPIKeyResolver above: it is read from deliverKickLocked and
	// agentEnvPairs, both of which already hold m.mu.
	explainModeDefaultResolver atomic.Pointer[func() string]

	// bobKeySourceResolver reports WHERE the key was found ("file:<path>" or
	// "env:<NAME>"), never the value. The launch path needs the PATH so it can
	// verify the file is readable by the AGENT UID — the hive process can read
	// it as dev even when the agent cannot, so key presence alone is a false
	// positive (see verifyBobKeyReadable). Same atomic.Pointer discipline and
	// same deadlock reasoning as bobAPIKeyResolver above.
	bobKeySourceResolver atomic.Pointer[func() string]

	// auditSink, when set, receives agent lifecycle events (start, stop,
	// launch failure, backend/model change) for durable, queryable recording
	// in the dashboard's audit store. Nil in tests / non-dashboard setups, in
	// which case every audit call is a no-op. See pkg/agent/audit.go for why
	// this is an injected interface rather than a direct pkg/dashboard import,
	// and why it is an atomic.Pointer rather than m.mu-guarded state.
	auditSink atomic.Pointer[AuditSink]

	// kickObserver, when set, receives kick lifecycle events ("kick-delivered",
	// "kick-log-archived") for external progress surfaces — the Linear
	// AgentActivity emitter (RFC #4492 Part 2) is the first consumer. Same
	// atomic.Pointer discipline as auditSink: both notification sites run under
	// m.mu, so the pointer must be readable from a locked context, and the
	// observer is always invoked on its own goroutine. See kick_observer.go.
	kickObserver atomic.Pointer[func(agentName, event, detail string)]
	// kickOutcomeObserver receives the verdict on how each kicked turn ended
	// (#7421); the governor consumes it. Same discipline as kickObserver.
	kickOutcomeObserver atomic.Pointer[func(agentName string, outcome KickOutcome)]

	// kickDispatches tracks asynchronous kick dispatches (#5325): the in-flight
	// guard that makes delivery exactly-once, and the latest outcome per agent
	// so the dashboard can report the true result after answering the POST with
	// 202. It carries its OWN mutex rather than living under m.mu, because the
	// delivery goroutine settles a dispatch from a context that holds no
	// manager lock and must not contend with the launch path. See kick_async.go.
	kickDispatches kickDispatchRegistry

	inferenceRouteCallback      func(agentName, backend, model string)
	clearInferenceRouteCallback func(agentName string)

	// isGatewayBackend reports whether a backend string names a configured model
	// gateway (in addition to the built-in inference backends vllm/llm-d/litellm).
	// Injected from config so an agent whose backend is a gateway name (e.g.
	// "openrouter") is treated as inference-routable and its route resolved.
	// Nil in tests/bare setups → only built-in inference backends route.
	//
	// Stored as an atomic.Pointer, NOT under m.mu: routableBackend() is called
	// from the agent-launch path (launchInTmux via Start), which ALREADY holds
	// m.mu.Lock(). Reading this under m.mu.RLock() there would re-lock a
	// non-reentrant RWMutex on the same goroutine and DEADLOCK the whole startup
	// — the process never reaches MarkReady, /api/health stays "starting", and
	// the startup probe kills the pod (a cluster-wide crash-loop we hit live).
	// An atomic read is lock-free and safe from any lock context.
	isGatewayBackend atomic.Pointer[func(backend string) bool]

	// persistPauseCallback, when set, persists an agent's paused state to
	// the on-disk config so it survives restarts. Nil in tests / bare setups.
	//
	// Invocation contract: Pause/Resume snapshot this under m.mu but invoke
	// it only AFTER releasing m.mu. The callback does config disk I/O and is
	// allowed to re-enter the manager (AllStatuses, GetStatus, ...); m.mu is
	// a non-reentrant sync.RWMutex, so invoking it with the write lock held
	// would deadlock the pause path and wedge every operation queued behind
	// m.mu (heartbeat AllStatuses, SendKick, terminal ResolveAgent) — the
	// same failure class as the mint-issuer deadlock fixed in ca5f0f00.
	persistPauseCallback func(name string, paused bool)
	pauseObserver        atomic.Pointer[func(PauseTransitionEvent)]

	// breakerEngaged and breakerPaused hold the fleet-breaker's state, guarded
	// by m.mu. When an operator throws the breaker, EngageBreaker pauses every
	// running, non-on-demand agent and records the set of names it paused here.
	// Releasing resumes ONLY that set, and only for agents whose pause is still
	// attributable to the breaker (PausedTrigger == BreakerTrigger) — an agent
	// an operator re-paused during the breaker window keeps its manual pause.
	// Both fields persist into /data/hive-state.json (see cmd/hive persistState)
	// so an engaged breaker survives the frequent pod restarts: on boot the
	// agents restore paused from their own persisted pause, and RestoreBreaker
	// re-associates them with the breaker so a later release resumes them.
	breakerEngaged bool
	breakerPaused  map[string]bool

	// recordPromptCallback, when set, persists the fully-expanded prompt text
	// delivered to an agent so owners can review it later.
	//
	// Held as an atomic.Pointer rather than behind m.mu because the kick path
	// (deliverKickLocked) already holds m.mu when it fires this. m.mu is a
	// non-reentrant RWMutex, so reading the callback under a second Lock there
	// would deadlock the kick path.
	recordPromptCallback atomic.Pointer[func(agent, trigger, prompt string)]
	// deadSessionRecoveryOwnedElsewhere is set when the watchdog reconciler
	// owns restarting missing-session / bare-pane agents (RFC #4665), so this
	// manager's crash loop observes those two conditions without restarting
	// them. Guarded by m.mu, like the agent map it gates work over.
	deadSessionRecoveryOwnedElsewhere bool
	sandboxConfig                     config.AgentSandboxConfig
	sandboxLauncher                   sandbox.Launcher
	sandboxRunner                     sandboxCommandRunner
	sandboxPushMinter                 pushbroker.TokenMinter
	sandboxPRClient                   PRCreator
	sandboxAuditCallback              atomic.Pointer[func(agent, action, detail string)]

	// terminal is every interaction with the agent's interactive terminal
	// (pane capture, keystrokes, scrollback). nil means the real tmux-backed
	// implementation (see Manager.term / tmuxTerminal in terminal.go); tests
	// install a funcTerminal to fake individual methods. Replaces the eight
	// ad-hoc func-typed seam fields removed in issue #5636 phase 1.
	terminal             TerminalSession
	promptDismissTimeout time.Duration

	// Per-kick durable log archiving (#4296, #4295) — see kick_logs.go.
	// kickLogDir/kickLogRetention/kickLogMaxBytes are resolved once in
	// NewManager from env overrides; the capture/clear-history subprocesses
	// are reached through m.terminal above.
	kickLogDir       string
	kickLogRetention int
	kickLogMaxBytes  int64
}

// IsInferenceBackend returns true if the backend is a self-hosted inference
// backend (vllm, llm-d, litellm) rather than a CLI tool. Delegates to the
// canonical list in the config package (shared with the proxy package,
// which cannot be imported from here without a cycle).
func IsInferenceBackend(backend string) bool {
	return config.IsInferenceBackend(backend)
}

// SetInferenceCallbacks registers callbacks that the manager uses to
// configure/clear inference routing on the proxy when launching agents.
func (m *Manager) SetInferenceCallbacks(
	setRoute func(agentName, backend, model string),
	clearRoute func(agentName string),
) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inferenceRouteCallback = setRoute
	m.clearInferenceRouteCallback = clearRoute
}

// SetGatewayBackendChecker injects a predicate that reports whether a backend
// string names a configured model gateway. This makes an agent whose backend is
// a gateway name inference-routable, so its route is resolved via the inference
// callback exactly like the built-in litellm/vllm/llm-d backends.
func (m *Manager) SetGatewayBackendChecker(fn func(backend string) bool) {
	// Atomic store — no m.mu — so routableBackend can read it lock-free from the
	// lock-holding launch path without deadlocking (see isGatewayBackend docs).
	m.isGatewayBackend.Store(&fn)
}

// routableBackend reports whether a backend should be routed through the
// inference proxy: either a built-in inference backend, or a configured gateway
// name. Safe to call while holding m.mu (isGatewayBackend is read atomically).
func (m *Manager) routableBackend(backend string) bool {
	if IsInferenceBackend(backend) {
		return true
	}
	// Lock-free atomic read: this is invoked from the launch path while m.mu is
	// already held, so it MUST NOT take m.mu (non-reentrant RWMutex → deadlock).
	fnp := m.isGatewayBackend.Load()
	return fnp != nil && *fnp != nil && (*fnp)(backend)
}

// validateBackendName reports whether backend is one the launcher can actually
// dispatch: an agentic CLI, a model-gateway backend, or a configured gateway
// name. An empty backend is valid (it means "the hive default").
//
// This is the manager-side half of the accept-then-fail fix. It dispatches on
// the SAME canonical lists as config.ValidateBackend and backendBinary, so a
// backend accepted by any write path is one the launch path can start.
// Safe to call while holding m.mu (routableBackend reads atomically).
func (m *Manager) validateBackendName(backend string) error {
	if backend == "" || config.IsCLIBackend(backend) || m.routableBackend(backend) {
		return nil
	}
	return fmt.Errorf("unsupported backend %q (supported: %s; or the name of a configured model gateway)",
		backend, strings.Join(config.SupportedBackends(), ", "))
}

// SetACMMLevel updates the cached ACMM level used by agentMode() when
// launching agents. Call this whenever the ACMM level changes.
func (m *Manager) SetACMMLevel(level int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.project.ACMMLevel = level
	m.removeAgentsBelowACMMGateLocked(level)
}

func (m *Manager) GetACMMLevel() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.project.ACMMLevel
}

func NewManager(agents map[string]config.AgentConfig, logger *slog.Logger, project ProjectContext) *Manager {
	workDir := os.Getenv("HIVE_WORK_DIR")
	if workDir == "" {
		workDir = "/data/agents"
	}

	// Save COPILOT_GITHUB_TOKEN for explicit injection via tmux set-environment.
	// The token stays in the process env so all agents can authenticate for AI
	// completions; write access is gated by --enable-all-github-mcp-tools flag.
	copilotToken := os.Getenv("COPILOT_GITHUB_TOKEN")
	copilotTokenAuthoritative := strings.TrimSpace(copilotToken) != ""
	copilotTokenSource := CopilotTokenSourceEnv
	if copilotToken == "" {
		// Fall back to the token persisted by the dashboard's device-flow login.
		copilotTokenSource = CopilotTokenSourceDurableFile
		if data, err := os.ReadFile(CopilotUserTokenPath); err == nil {
			copilotToken = strings.TrimSpace(string(data))
		}
	}
	if strings.TrimSpace(copilotToken) == "" {
		copilotTokenSource = ""
	}
	claudeToken := claude.ReadAccessToken(claude.CredentialsPath)

	var uidMap *UIDMap
	if loaded, err := LoadUIDMap(UIDMapPath); err == nil {
		uidMap = loaded
		logger.Info("UID map loaded", "agents", len(uidMap.Agents), "iptables", uidMap.IptablesActive)
	} else {
		logger.Info("no UID map found, agents will share dev UID", "path", UIDMapPath)
	}

	kickLogDir, kickLogRetention, kickLogMaxBytes := kickLogSettingsFromEnv()

	m := &Manager{
		agents:                        make(map[string]*AgentProcess),
		idToName:                      make(map[string]string),
		logger:                        logger,
		workDir:                       workDir,
		project:                       project,
		copilotAuthToken:              copilotToken,
		copilotAuthTokenAuthoritative: copilotTokenAuthoritative,
		copilotAuthTokenSource:        copilotTokenSource,
		claudeAuthToken:               claudeToken,
		uidMap:                        uidMap,
		kickLogDir:                    kickLogDir,
		kickLogRetention:              kickLogRetention,
		kickLogMaxBytes:               kickLogMaxBytes,
	}

	for name, cfg := range agents {
		if !AgentAvailableAtACMMLevel(name, project.ACMMLevel) {
			logger.Info("agent below ACMM gate; not instantiating", "agent", name, "level", project.ACMMLevel)
			continue
		}
		agentID := cfg.ID
		if agentID == "" {
			agentID = name
		}
		agentUID := 0
		tmuxSocket := ""
		if uidMap != nil {
			agentUID = uidMap.LookupByName(name)
			if agentUID > 0 {
				tmuxSocket = "hive-" + name
			}
		}
		m.agents[name] = &AgentProcess{
			Name:   name,
			ID:     agentID,
			Config: cfg,
			State:  StateStopped,
			UID:    agentUID,
			// Restore a persisted operator pause so a restart/upgrade
			// doesn't silently un-pause the agent.
			Paused:       cfg.Paused,
			OutputBuffer: NewRingBuffer(outputBufferCapacity),
			tmuxSession:  "hive-" + name,
			tmuxSocket:   tmuxSocket,
		}
		m.idToName[agentID] = name
	}

	return m
}

// ResolveAgent returns the YAML key (name) for a given name or ID.
// If the input matches neither, it returns the input unchanged (callers
// will get a "not found" error from the specific method).
func (m *Manager) ResolveAgent(nameOrID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.agents[nameOrID]; ok {
		return nameOrID
	}
	if name, ok := m.idToName[nameOrID]; ok {
		return name
	}
	return nameOrID
}

func (m *Manager) Start(ctx context.Context, name string) error {
	// PHASE 1 — brief critical section: map lookup, the pure in-memory
	// decisions (running/sandbox), and claiming the per-agent launch guard.
	// Nothing here does /data NFS I/O or a subprocess/outbound call, so m.mu is
	// held only for microseconds.
	m.mu.Lock()

	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent %s not found", name)
	}
	if !AgentAvailableAtACMMLevel(name, m.project.ACMMLevel) {
		m.mu.Unlock()
		return fmt.Errorf("agent %s is not available below ACMM L5", name)
	}

	if agent.State == StateRunning {
		m.mu.Unlock()
		return fmt.Errorf("agent %s already running", name)
	}

	if m.agentSandboxEnabledLocked(agent) {
		// Sandbox agents never launch a CLI here — this branch only sets
		// in-memory state and does no I/O, so it completes entirely inside the
		// Phase-1 lock and never claims the launch guard.
		if agent.Paused {
			agent.State = StatePaused
			m.logger.Info("sandbox agent starting paused", "name", agent.Name, "trigger", agent.PausedTrigger, "persisted", agent.Config.Paused)
			m.mu.Unlock()
			return nil
		}
		now := time.Now()
		agent.State = StateIdle
		agent.StartedAt = &now
		agent.HasLaunched = true
		agent.LaunchedMode = m.agentMode(agent)
		m.logger.Info("audit: sandbox agent ready", "name", name)
		m.mu.Unlock()
		return nil
	}

	// Serialize concurrent Start(sameName). Once we release m.mu for the
	// out-of-lock launch below, this guard is the only thing preventing a
	// second Start from racing this one's tmux launch and guarded-field writes
	// (m.mu no longer covers the whole method). Refuse the second caller fast.
	if agent.launching {
		m.mu.Unlock()
		return fmt.Errorf("agent %s launch already in progress", name)
	}
	agent.launching = true
	m.mu.Unlock()

	// Clear the guard on EVERY exit from here down (error, park-and-return,
	// success, or panic). Phase 1's returns above happen before the guard is
	// set, so they neither set nor need to clear it.
	defer func() {
		m.mu.Lock()
		agent.launching = false
		m.mu.Unlock()
	}()

	// PHASE 2 — launch preparation with m.mu RELEASED. sanitizeGitRemotes walks
	// /data/agents/<name> and runs git subprocesses; ensureTmuxSession does
	// os.MkdirAll("/data/agents/<name>") + a tmux subprocess. On the NFS RWX
	// PVC these can block in uninterruptible D-state when the server has stale
	// locks — but no longer WHILE HOLDING m.mu, so AllStatuses()/the heartbeat
	// collect() keep taking the RLock, the heartbeat-attempt clock keeps
	// advancing, and /api/livez stays 200. Neither call mutates m.mu-guarded
	// AgentProcess fields (they read only immutable Name/UID/tmuxSession/
	// tmuxSocket and Config), so running them lock-free is race-free.
	m.sanitizeGitRemotes(agent)

	if err := m.ensureTmuxSession(agent); err != nil {
		// No tmux session means no pane to announce into, so this failure
		// cannot ride announceLaunchFailureInPane like the park-and-return
		// branches do — record it here or it stays invisible.
		m.audit(AuditAgentStartFailed, name, auditFields(
			"outcome", "failure",
			"backend", agent.effectiveBackend(),
			"model", agent.effectiveModel(),
			"error", err.Error(),
			"stage", "tmux_session",
		))
		return err
	}

	backend := agent.Config.Backend
	if agent.BackendOverride != "" {
		backend = agent.BackendOverride
	}
	if agent.Paused {
		// Auto-unpause inference agents that were only transiently paused —
		// but NEVER override a persisted operator pause (Config.Paused set via
		// the dashboard and saved to hive.yaml). Previously this cleared EVERY
		// inference-backend pause on startup, so an operator pause of a
		// litellm/vllm/llm-d agent was silently undone on every restart AND
		// the auto-unpause overwrote the persisted flag — corrupting the saved
		// pause set (issue: kellyaa's pauses reverted on restart despite being
		// on inference backends).
		// Auto-unpause inference agents ONLY for a non-operator (transient/
		// system) pause. An operator pause — dashboard-api trigger, or the
		// persisted Config.Paused flag — must ALWAYS survive, exactly like a
		// copilot-backed agent does. Keying on the backend alone wiped
		// operator pauses of litellm/vllm/llm-d agents on every restart
		// (kellyaa: her litellm-routed agents un-paused while the copilot
		// strategist stayed paused, which is what exposed this).
		operatorPaused := agent.Config.Paused || agent.PausedTrigger == "dashboard-api"
		// A login-detector pause describes a LIVE pane condition ("a login
		// prompt is on screen right now") that cannot outlive the pane — after
		// a restart/pod roll there is no pane, so restoring the pause just
		// strands the agent forever with nothing left to re-evaluate it
		// (hivecommons/hive, 2026-08-22: four copilot agents stayed
		// persisted-paused across every roll). Drop it on startup and let the
		// agent launch; if the condition still holds, the detector re-pauses
		// within one tick — and with PaneShowsBlockingPrompt it now only
		// pauses for REAL login prompts. Keyed strictly on the trigger so an
		// operator pause (dashboard-api / hand-set Config.Paused without a
		// system trigger) is never touched — the kellyaa regression above is
		// exactly what happens when this distinction is dropped.
		loginDetectorPaused := agent.PausedTrigger == "login-detector"
		if (IsInferenceBackend(backend) && !operatorPaused) || loginDetectorPaused {
			// agent.Paused is an m.mu-guarded field; brief re-lock around the
			// write so it stays atomic against AllStatuses()/setters.
			m.mu.Lock()
			agent.Paused = false
			if loginDetectorPaused {
				// The system pause persisted Config.Paused; clear it so the
				// launch below isn't re-blocked and a later save doesn't
				// re-persist a pause nobody owns anymore.
				agent.Config.Paused = false
			}
			m.mu.Unlock()
			m.logger.Info("auto-unpaused transiently paused agent on startup", "name", agent.Name, "backend", backend, "trigger", agent.PausedTrigger)
		} else {
			m.mu.Lock()
			agent.State = StatePaused
			m.mu.Unlock()
			m.logger.Info("agent starting paused", "name", agent.Name, "backend", backend, "trigger", agent.PausedTrigger, "persisted", agent.Config.Paused)
			return nil
		}
	}

	// Runs with m.mu RELEASED — see mintAgentTokenUnlocked for why holding
	// m.mu across the outbound mint calls caused a fleet-wide liveness flap.
	m.mintAgentTokenUnlocked(ctx, agent)

	// PHASE 3 — launchInTmux. It was written to be called WITH m.mu held: it
	// mutates m.mu-guarded AgentProcess fields (State, StartedAt, HasLaunched,
	// LaunchedMode, LastKick/LastKickMessage/KickHistory, LastError, cancel,
	// launchGen, forceRelaunch, awaitingBobKey, ...) directly with no internal
	// locking. Re-acquire m.mu for the duration so those writes stay race-free
	// against AllStatuses()/snapshot() and the model/backend/pause setters —
	// preserving its original contract exactly (the function is unchanged).
	//
	// The launch's own /data reads/writes (ensureTmuxSession has already run
	// lock-free above; the remaining /data touch is ensureBobAuthSettings on
	// /data/home for bob agents) are NOT hoisted here — pulling launchInTmux's
	// deeply interleaved guarded-field writes and NFS I/O apart is a larger,
	// riskier refactor left for a separate maintainer decision. The three
	// biggest and most common NFS/proxy blockers (sanitizeGitRemotes,
	// ensureTmuxSession, WriteAgentToken/mint) are already off the lock above,
	// which is what breaks the observed fleet-wide liveness flap; a bob-only
	// /data/home stall under the lock remains a narrower residual.
	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-verify under the re-acquired lock: while m.mu was released for Phase 2,
	// a concurrent Stop/Remove could have deleted this agent from the map or a
	// racing path could have started it. The launching guard prevents a second
	// concurrent Start of THIS agent, but not a Stop/delete, so re-check both
	// before mutating launch state. (agent still points at the same struct; the
	// map re-lookup is what detects a delete.)
	if cur, ok := m.agents[name]; !ok || cur != agent {
		return fmt.Errorf("agent %s removed during launch", name)
	}
	if agent.State == StateRunning {
		// Another path won the launch race while we were unlocked; nothing to do.
		return nil
	}
	return m.launchInTmux(ctx, agent)
}

// tmuxPaneHasCLI reports whether a CLI is running in the pane by inspecting
// the visible pane content for known CLI UI markers.
func (m *Manager) tmuxPaneHasCLI(session string) bool {
	return paneHasCLIMarker(m.captureTmuxPane(session))
}

const (
	// consentConfirmFooter appears at the bottom of Claude Code interactive
	// selection screens (consent dialogs, settings-error menus).
	consentConfirmFooter = "Enter to confirm"
	// bypassConsentTitle is the heading of the --dangerously-skip-permissions
	// consent screen. Its default selection is "No, exit" — confirming it
	// terminates the CLI and leaves a bare bash pane.
	bypassConsentTitle = "Bypass Permissions mode"
	// bypassConsentDefaultOption is the default (negative) option on the
	// bypass-permissions consent screen.
	bypassConsentDefaultOption = "No, exit"
	// bypassConsentAcceptOption is the affirmative option on the
	// bypass-permissions consent screen. Its position varies between CLI
	// versions, so acceptance navigates by matching the selected-line text.
	bypassConsentAcceptOption = "Yes, I accept"
	// apiKeyPromptTitle is the heading of the custom-API-key approval prompt,
	// shown when ANTHROPIC_API_KEY is not in customApiKeyResponses.approved.
	// Its default selection is "No (recommended)" with the affirmative option
	// above it.
	apiKeyPromptTitle = "Detected a custom API key"
	// apiKeyPromptAcceptOption is the affirmative option on the
	// custom-API-key approval prompt.
	apiKeyPromptAcceptOption = "Yes"
	// cliWorkingMarker is shown while Claude Code is actively processing a
	// request; a pane in this state is never a consent screen.
	cliWorkingMarker = "esc to interrupt"
)

// paneShowsConsentScreen reports whether the pane is showing an interactive
// consent/selection screen rather than a ready CLI input prompt. Such screens
// contain a "❯"-selected menu option (e.g. "❯ 1. No, exit"), so they satisfy
// marker-based CLI presence checks ("❯" is also a cliPaneMarkers entry) — a
// kick typed into one is consumed by the menu, or by bash once the default
// "No, exit" selection terminates the CLI. Callers should pass the visible
// pane only (no scrollback): dismissed consent screens linger in history.
func paneShowsConsentScreen(pane string) bool {
	if pane == "" || strings.Contains(pane, cliWorkingMarker) {
		return false
	}
	// A known startup-blocking menu is not a ready prompt either. The generic
	// test below needs the "Enter to confirm" footer AND a "❯"-marked line;
	// codex renders neither (its footer is "Press enter to continue" and its
	// marker is "›" U+203A), so its update menu read as READY. Everything that
	// gates on readiness — the startup kick, caveman activation — then typed
	// into the menu, and the Enter confirmed its pre-selected option:
	// "1. Update now", which runs `npm install -g` as the agent UID, fails, and
	// kills the CLI. Blocking on these lets the prompt watcher answer them.
	if paneHasBlockingPrompt(pane) {
		return true
	}
	if strings.Contains(pane, bypassConsentTitle) && strings.Contains(pane, bypassConsentDefaultOption) {
		return true
	}
	if !strings.Contains(pane, consentConfirmFooter) {
		return false
	}
	for _, line := range strings.Split(pane, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "❯") {
			return true
		}
	}
	return false
}

// copilotGitHubWriteDenyFlags and claudeGitHubWriteDenyFlags are defined together
// near the bottom of this file (alongside the codex/bob backend constants). v2
// independently added a copy of copilotGitHubWriteDenyFlags here; the v4 grouped
// definition (which also carries claudeGitHubWriteDenyFlags) is kept as the single
// source of truth, so this duplicate was dropped in the v2→v4 sync merge.

// kickOutcomePollEvery throttles the post-kick turn-ended check to one visible
// pane capture per this many 3s poll ticks (15s), so a long turn does not
// cost an extra tmux exec every tick.
const kickOutcomePollEvery = 5

// watchForTrustPrompt monitors a tmux session for Copilot's "Confirm folder trust"
// prompt and auto-selects "Yes, and remember for future sessions" (option 2).
func (m *Manager) watchForTrustPrompt(session string, ctx context.Context) {
	deadline := time.After(trustMaxWait)
	ticker := time.NewTicker(trustPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-ticker.C:
			output := m.captureTmuxPane(session)
			if strings.Contains(output, "Confirm folder trust") || strings.Contains(output, "Do you trust the files") {
				time.Sleep(paneCaptureSleep)
				_ = m.tmuxRawCmd("send-keys", "-t", session, "2").Run()
				time.Sleep(enterDelay)
				_ = m.tmuxRawCmd("send-keys", "-t", session, "Enter").Run()
				m.logger.Info("auto-answered folder trust prompt", "session", session)
				time.Sleep(trustCooldown)
			}
		}
	}
}

// acmmLevelNames maps ACMM level numbers to human-readable names. Kept in
// sync with the canonical pack definitions in src/pkg/config/packs/level-*.yaml.
var acmmLevelNames = map[int]string{
	1: "Inception",
	2: "Advisory",
	3: "Quality-Gated",
	4: "Security-Aware",
	5: "Semi-Autonomous",
	6: "Fully Autonomous",
}

func (m *Manager) buildBootstrapPrompt(agent *AgentProcess) string {
	// No boot prompt — the governor's first eval cycle (10s after startup)
	// kicks all due agents via BuildKickMessages with fully substituted
	// templates. Sending a boot prompt here caused unsubstituted ${ISSUE_LIST}
	// and other vars to reach the agent. The policy-file path list this
	// function used to assemble was dead code once the boot prompt was
	// removed, so it is gone too.
	_ = agent // signature kept for the call site; the arg is no longer read
	return ""
}

// findACMMFragments returns paths to ACMM policy files the agent should read.
// Order: base.md (shared rules) then l<N>.md (level-specific).
var acmmFragmentFallbackDirs = []string{
	"/data/policies/examples/acmm",
	"/opt/hive/examples/acmm",
}

func (m *Manager) findACMMFragments() []string {
	level := m.project.ACMMLevel
	if level <= 0 {
		return nil
	}

	// Look for ACMM fragments in the policies directory first, then fallback to baked-in paths.
	policiesRoot := filepath.Dir(m.project.PolicyDir)
	if policiesRoot == "." || policiesRoot == "" {
		policiesRoot = "/data/policies"
	}

	acmmDirs := append([]string{filepath.Join(policiesRoot, "examples", "acmm")}, acmmFragmentFallbackDirs...)

	var acmmDir string
	for _, d := range acmmDirs {
		if _, err := os.Stat(d); err == nil {
			acmmDir = d
			break
		}
	}
	if acmmDir == "" {
		return nil
	}

	var files []string
	basePath := filepath.Join(acmmDir, "base.md")
	if _, err := os.Stat(basePath); err == nil {
		files = append(files, basePath)
	}
	levelPath := filepath.Join(acmmDir, fmt.Sprintf("l%d.md", level))
	if _, err := os.Stat(levelPath); err == nil {
		files = append(files, levelPath)
	}
	return files
}

func (m *Manager) buildProjectPreamble(agent *AgentProcess) string {
	p := m.project
	if p.Org == "" || len(p.Repos) == 0 {
		return ""
	}

	repos := make([]string, len(p.Repos))
	for i, r := range p.Repos {
		repos[i] = fmt.Sprintf("%s/%s", p.Org, r)
	}

	levelName := acmmLevelNames[p.ACMMLevel]
	if levelName == "" {
		levelName = fmt.Sprintf("Level %d", p.ACMMLevel)
	}

	mode := m.agentMode(agent)
	var prPolicy string
	if !p.PRsAllowed {
		prPolicy = "PRs NOT allowed (project-wide)."
	} else {
		switch mode {
		case ModeAdvisory:
			prPolicy = "\U0001F4DD Advisory only — beads, no issues/PRs."
		case ModeIssuesOnly:
			prPolicy = "\U0001F3AB Issues ONLY — can open issues. NO PRs."
		case ModeIssuesAndPRs:
			if p.ACMMLevel == 5 {
				prPolicy = "\U0001F527 Issues + PRs allowed (hold-labeled, human merges)."
			} else {
				prPolicy = "\U0001F527 Issues + PRs allowed."
			}
		case ModeIssuesPRsMerge:
			prPolicy = "\U0001F680 Issues + PRs + auto-merge on green CI."
		default:
			prPolicy = "\U0001F4DD Advisory only — beads, no issues/PRs."
		}
	}

	return fmt.Sprintf("[PROJECT] Org: %s | Repos: %s | ACMM: L%d (%s) | Mode: %s %s | %s ",
		p.Org, strings.Join(repos, ", "), p.ACMMLevel, levelName,
		mode.Emoji(), mode.String(), prPolicy)
}

// metricsCachePath is a var (not const) so tests can point it at a temp file
// to exercise readCoveragePreamble without a real /data volume. Production
// value is unchanged.
var metricsCachePath = "/data/metrics/agent-metrics-cache.json"

func (m *Manager) readCoveragePreamble() string {
	data, err := os.ReadFile(metricsCachePath)
	if err != nil {
		return ""
	}
	var metrics map[string]map[string]json.Number
	if err := json.Unmarshal(data, &metrics); err != nil {
		return ""
	}
	ci, ok := metrics["ci-maintainer"]
	if !ok {
		return ""
	}
	cov, err := ci["coverage"].Int64()
	if err != nil {
		return ""
	}
	target, err := ci["coverageTarget"].Int64()
	if err != nil {
		target = 91
	}
	return fmt.Sprintf("[COVERAGE] Current: %d%% | Target: %d%%.", cov, target)
}

// shellEnvVar formats KEY='value' with single-quoting so values containing
// spaces, parentheses, or other shell metacharacters are safe in inline env
// var assignments sent to tmux via send-keys.
func shellEnvVar(key, value string) string {
	quoted := strings.ReplaceAll(value, "'", "'\"'\"'")
	return fmt.Sprintf("%s='%s'", key, quoted)
}

// applySecretEnv pushes only the Secret pairs into the agent's tmux session via
// set-environment. Values are passed as exec args (never through a shell), so
// they are not word-split and never land in the pane or in bash history.
// Failures are ignored for the same reason ensureTmuxSession ignores them: a
// missing session is handled by the launch path, not here.
func (m *Manager) applySecretEnv(agent *AgentProcess) {
	if agent == nil || agent.tmuxSession == "" {
		return
	}
	for _, p := range m.agentEnvPairs(agent) {
		if !p.Secret {
			continue
		}
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, p.Key, p.Value).Run()
	}
}

func (m *Manager) buildEnvPrefix(agent *AgentProcess) string {
	pairs := m.agentEnvPairs(agent)
	var parts []string
	for _, p := range pairs {
		if p.Secret {
			continue
		}
		parts = append(parts, shellEnvVar(p.Key, p.Value))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

func (m *Manager) pollTmuxOutput(name, session string, buf *RingBuffer, ctx context.Context) {
	const pollInterval = 3 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var prevLines []string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			output := m.captureTmuxPane(session)
			if output == "" {
				continue
			}
			var filtered []string
			for _, line := range strings.Split(output, "\n") {
				trimmed := strings.TrimRight(line, " \t")
				if trimmed != "" {
					filtered = append(filtered, trimmed)
				}
			}
			if len(filtered) == 0 {
				continue
			}
			if prevLines == nil {
				// First capture after (re)start — seed prevLines so subsequent
				// diffs work. Only write to the buffer if it's empty (fresh
				// start); skip if it already has content (restart) to avoid
				// duplicating the scrollback.
				if buf.Count() == 0 {
					for _, l := range filtered {
						buf.Write(l)
					}
				}
				prevLines = filtered
				continue
			}
			newLines := diffNewLines(prevLines, filtered)
			for _, l := range newLines {
				buf.Write(l)
				m.logOutputSignals(name, l)
				m.checkBlockedThrash(name, l)
			}
			prevLines = filtered
		}
	}
}

// Blocked-action thrash breaker: an agent that keeps hammering a policy wall
// (e.g. a push with no per-agent token, blocked every ~3s by
// git-credential-hive, or a proxy hard-deny) burns model tokens indefinitely
// with zero possible output — observed live 2026-08-04 on a hosted L2 hive
// whose guide agent retried a blocked push every 3 seconds. (Since #4289,
// ADVISORY-mode pushes are no longer blocked by the credential helper — the
// read-only token is served and GitHub rejects the push with 403 — but the
// helper still emits "git push blocked:" for unknown-UID and missing-token
// failures, which this breaker continues to catch.) The hub, not the model,
// breaks the loop: thrashThreshold blocked-action lines within thrashWindow
// pauses the session (visible, reversible, stops governor kicks) with the
// reason spelled out.
const (
	thrashWindow    = 60 * time.Second
	thrashThreshold = 5
	thrashCooldown  = 10 * time.Minute
)

// blockedActionMarkers are the policy-wall stderr lines that can never
// succeed by retrying. Keep in sync with bin/git-credential-hive.sh and the
// proxy's hard-deny responses.
var blockedActionMarkers = []string{
	"git push blocked:",
	"blocked by hive policy",
}

type thrashState struct {
	times    []time.Time
	lastTrip time.Time
}

// checkBlockedThrash records a blocked-action output line for the agent and,
// past the threshold, pauses the agent asynchronously (never inline: this is
// called from the output-capture goroutine and Pause takes m.mu).
func (m *Manager) checkBlockedThrash(agent, line string) {
	matched := false
	for _, marker := range blockedActionMarkers {
		if strings.Contains(line, marker) {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	now := time.Now()
	m.thrashMu.Lock()
	if m.thrash == nil {
		m.thrash = map[string]*thrashState{}
	}
	st := m.thrash[agent]
	if st == nil {
		st = &thrashState{}
		m.thrash[agent] = st
	}
	trip := recordBlockedAndCheck(st, now, thrashWindow, thrashThreshold, thrashCooldown)
	m.thrashMu.Unlock()
	if !trip {
		return
	}
	reason := fmt.Sprintf("blocked-action loop: %d+ policy-blocked attempts in %s — the block is terminal in this mode; paused to stop token burn", thrashThreshold, thrashWindow)
	m.logger.Warn("thrash breaker tripped", "agent", agent, "line", truncateStr(line, 160))
	go func() {
		if err := m.Pause(agent, "thrash-breaker", reason); err != nil {
			m.logger.Warn("thrash breaker pause failed", "agent", agent, "error", err)
		}
	}()
}

// recordBlockedAndCheck is the pure sliding-window decision: append now, drop
// entries older than window, and report whether the threshold is crossed
// outside the cooldown. Split out for direct unit testing.
func recordBlockedAndCheck(st *thrashState, now time.Time, window time.Duration, threshold int, cooldown time.Duration) bool {
	st.times = append(st.times, now)
	cutoff := now.Add(-window)
	kept := st.times[:0]
	for _, t := range st.times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	st.times = kept
	if len(st.times) < threshold {
		return false
	}
	if !st.lastTrip.IsZero() && now.Sub(st.lastTrip) < cooldown {
		return false
	}
	st.lastTrip = now
	st.times = nil
	return true
}

// waitForCLIReady polls the tmux pane until the CLI shows its ready prompt
// or the timeout expires. Returns true if the CLI became ready.
func (m *Manager) waitForCLIReady(session string) bool {
	deadline := time.After(cliReadyTimeout)
	ticker := time.NewTicker(cliReadyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-ticker.C:
			if m.tmuxPaneHasCLI(session) {
				return true
			}
		}
	}
}

// waitForInputPromptForAgent polls until the CLI shows its input prompt (❯)
// using the agent's tmux socket.

// waitForInputPromptForAgentUnless is waitForInputPromptForAgent with an
// abort predicate, consulted once per poll tick: when it reports true the wait
// returns false at once instead of running out inputPromptTimeout. The kick
// paths pass a "this kick's epoch moved" check (#7363) so a restart that
// invalidates a pending kick releases its goroutine promptly rather than
// leaving it to notice only once the relaunched CLI shows a prompt. A nil
// predicate never aborts.
func (m *Manager) waitForInputPromptForAgentUnless(agent *AgentProcess, abort func() bool) bool {
	deadline := time.After(inputPromptTimeout)
	ticker := time.NewTicker(inputPromptPollInterval)
	defer ticker.Stop()

	for {
		if abort != nil && abort() {
			return false
		}
		select {
		case <-deadline:
			m.logger.Warn("prompt timeout — dumping pane",
				"agent", agent.Name,
				"session", agent.tmuxSession)
			output := m.captureTmuxPaneForAgent(agent)
			m.logger.Warn("pane content at timeout",
				"agent", agent.Name,
				"len", len(output),
				"has_goose_ready", strings.Contains(output, "goose is ready"),
				"has_enter", strings.Contains(output, "> Enter to send"),
				"has_arrow", strings.Contains(output, "❯"),
				"has_bob_placeholder", strings.Contains(output, bobInputPlaceholder),
				"has_codex_ready", strings.Contains(output, codexInputPromptMarker),
				"head_500", truncateHead(output, 500), "tail_500", truncateTail(output, 500))
			return false
		case <-ticker.C:
			// A consent/selection screen also contains "❯" but is NOT a
			// ready input prompt — sending a kick there feeds the menu.
			// Check the visible pane only: a dismissed consent screen
			// lingers in the scrollback that captureTmuxPaneForAgent sees.
			visible := m.captureVisiblePaneForAgent(agent)
			if paneShowsConsentScreen(visible) {
				continue
			}
			// An actively-working agent also keeps its "❯" input box
			// rendered but is NOT ready — kicking it would Ctrl+C + /clear
			// its in-flight work and every sub-agent it dispatched (#7085).
			// Use the visible pane only for the same reason as above: a
			// finished task's working marker lingers in scrollback.
			if paneShowsAgentWorking(visible) {
				continue
			}
			output := m.captureTmuxPaneForAgent(agent)
			if paneShowsInputPrompt(output) {
				return true
			}
		}
	}
}

// waitForInputPrompt polls until the CLI shows its input prompt (❯),
// indicating it is ready to accept a kick. This is stricter than
// waitForCLIReady which matches any CLI marker (including trust prompts).
func (m *Manager) waitForInputPrompt(session string) bool {
	deadline := time.After(inputPromptTimeout)
	ticker := time.NewTicker(inputPromptPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-ticker.C:
			output := m.captureTmuxPane(session)
			if paneShowsInputPrompt(output) {
				return true
			}
		}
	}
}

func (m *Manager) captureTmuxPane(session string) string {
	cmd := m.tmuxRawCmd("capture-pane", "-t", session, "-p",
		"-S", fmt.Sprintf("-%d", tmuxCaptureLines))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func (m *Manager) tmuxRawCmd(args ...string) *exec.Cmd {
	base := m.tmuxBaseArgs(&AgentProcess{})
	tmuxArgs := append(base[1:], args...)
	return exec.Command(base[0], tmuxArgs...)
}

func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	if agent.State != StateRunning {
		return nil
	}

	if agent.cancel != nil {
		agent.cancel()
	}

	m.tmuxSendKeysForAgent(agent, "C-c", "")

	agent.State = StateStopped
	m.logger.Info("audit: agent stopped", "name", name)
	m.audit(AuditAgentStopped, name, auditFields(
		"outcome", "success",
		"backend", agent.effectiveBackend(),
		"model", agent.effectiveModel(),
	))

	return nil
}

func (m *Manager) AddAgent(name string, cfg config.AgentConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !AgentAvailableAtACMMLevel(name, m.project.ACMMLevel) {
		m.logger.Info("agent below ACMM gate; not adding", "agent", name, "level", m.project.ACMMLevel)
		return
	}

	if _, exists := m.agents[name]; exists {
		return
	}

	agentID := cfg.ID
	if agentID == "" {
		agentID = name
	}
	agentUID := 0
	tmuxSocket := ""
	if m.uidMap != nil {
		agentUID = m.uidMap.AllocateUID(name)
		if agentUID > 0 {
			tmuxSocket = "hive-" + name
		}
		_ = m.uidMap.Save(UIDMapPath)
	}
	m.agents[name] = &AgentProcess{
		Name:         name,
		ID:           agentID,
		Config:       cfg,
		State:        StateStopped,
		UID:          agentUID,
		OutputBuffer: NewRingBuffer(outputBufferCapacity),
		tmuxSession:  "hive-" + name,
		tmuxSocket:   tmuxSocket,
	}
	m.idToName[agentID] = name
	m.logger.Info("audit: agent added", "name", name, "id", agentID, "uid", agentUID)
	m.audit(AuditAgentAdded, name, auditFields(
		"outcome", "success",
		"backend", cfg.Backend,
		"model", cfg.Model,
		"id", agentID,
	))
}

func (m *Manager) removeAgentsBelowACMMGateLocked(level int) {
	for name, existing := range m.agents {
		if AgentAvailableAtACMMLevel(name, level) {
			continue
		}
		if existing.cancel != nil {
			existing.cancel()
		}
		_ = m.tmuxCmd(existing, "kill-session", "-t", existing.tmuxSession).Run()
		delete(m.idToName, existing.ID)
		delete(m.agents, name)
		m.logger.Info("audit: agent removed by ACMM gate", "name", name, "id", existing.ID, "level", level, "session", existing.tmuxSession)
	}
}

// UpdateConfig updates the stored config for a running agent process so that
// status builders (which read from AgentProcess.Config) reflect changes made
// via the config dialog (which writes to the global Config.Agents map).
func (m *Manager) UpdateConfig(name string, cfg config.AgentConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.Config = cfg
	return nil
}

// ReconcileAgents makes the manager's name-keyed process table match the
// enabled config set. New agents are added, existing agents get fresh config,
// and removed agents have only their own hive-<name> tmux session retired.
func (m *Manager) ReconcileAgents(configs map[string]config.AgentConfig) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var added []string
	allowedConfigs := make(map[string]config.AgentConfig, len(configs))

	for name, cfg := range configs {
		if !AgentAvailableAtACMMLevel(name, m.project.ACMMLevel) {
			continue
		}
		allowedConfigs[name] = cfg
		if existing, ok := m.agents[name]; ok {
			delete(m.idToName, existing.ID)
			existing.Config = cfg
			if cfg.ID != "" {
				existing.ID = cfg.ID
			} else {
				existing.ID = name
			}
			m.idToName[existing.ID] = name
			continue
		}
		agentID := cfg.ID
		if agentID == "" {
			agentID = name
		}
		agentUID := 0
		tmuxSocket := ""
		if m.uidMap != nil {
			agentUID = m.uidMap.AllocateUID(name)
			if agentUID > 0 {
				tmuxSocket = "hive-" + name
			}
			_ = m.uidMap.Save(UIDMapPath)
		}
		m.agents[name] = &AgentProcess{
			Name:         name,
			ID:           agentID,
			Config:       cfg,
			State:        StateStopped,
			UID:          agentUID,
			Paused:       cfg.Paused,
			OutputBuffer: NewRingBuffer(outputBufferCapacity),
			tmuxSession:  "hive-" + name,
			tmuxSocket:   tmuxSocket,
		}
		m.idToName[agentID] = name
		added = append(added, name)
		m.logger.Info("audit: agent added by reconcile", "name", name, "id", agentID, "uid", agentUID)
	}

	for name, existing := range m.agents {
		if _, ok := allowedConfigs[name]; ok {
			continue
		}
		if existing.cancel != nil {
			existing.cancel()
		}
		_ = m.tmuxCmd(existing, "kill-session", "-t", existing.tmuxSession).Run()
		delete(m.idToName, existing.ID)
		delete(m.agents, name)
		m.logger.Info("audit: agent removed by reconcile", "name", name, "id", existing.ID, "session", existing.tmuxSession)
	}
	return added
}

func (m *Manager) RemoveAgent(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return
	}

	if agent.cancel != nil {
		agent.cancel()
	}

	delete(m.idToName, agent.ID)
	delete(m.agents, name)
	m.logger.Info("audit: agent removed", "name", name, "id", agent.ID)
	m.audit(AuditAgentRemoved, name, auditFields(
		"outcome", "success",
		"backend", agent.effectiveBackend(),
		"model", agent.effectiveModel(),
		"id", agent.ID,
	))
}

// effectiveBackend is the backend this agent will actually launch with: the
// per-agent override when set, otherwise its configured backend.
func (a *AgentProcess) effectiveBackend() string {
	if a.BackendOverride != "" {
		return a.BackendOverride
	}
	return a.Config.Backend
}

// effectiveModel is the model this agent will actually launch with: the
// per-agent override when set, otherwise its configured model. Returns the
// raw (un-normalized) name — the audit log should show what was ASKED for,
// since a bad model name is exactly the kind of misconfiguration being
// audited.
func (a *AgentProcess) effectiveModel() string {
	if a.ModelOverride != "" {
		return a.ModelOverride
	}
	return a.Config.Model
}

// dismissInferencePrompts polls the tmux pane for Claude Code interactive
// prompts and auto-dismisses them. The "Bypass Permissions mode" consent
// screen and the custom-API-key approval prompt are handled first and
// explicitly (see confirmMenuOption): their default selections are negative
// ("No, exit" / "No (recommended)"), so confirming blind terminates the CLI
// or declines the seeded key.
// Other prompts are handled dynamically regardless of prompt text changes
// between Claude Code versions by:
//  1. Detecting "Enter to confirm" (universal prompt footer)
//  2. Finding the selected option (line with "❯" marker)
//  3. If selected option looks negative (contains "No" or "exit"), navigate
//     away from it before confirming
//  4. For "Press Enter to continue" screens, just press Enter
//
// The pane is polled fast for the first 10s — the consent screen appears
// within ~5-8s of launch and every second it lingers is a window for a kick
// to be swallowed by the menu — then at a relaxed interval.
//
// Stops when the main Claude Code input prompt appears ("esc to interrupt").
func (m *Manager) dismissInferencePrompts(agent *AgentProcess) {
	const (
		// promptFastPollWindow covers the launch window in which the consent
		// screen normally appears (~5-8s after CLI start).
		promptFastPollWindow   = 10 * time.Second
		promptFastPollInterval = 250 * time.Millisecond
		promptPollInterval     = 1 * time.Second
		promptDismissTimeout   = 60 * time.Second
		postKeystrokeDelay     = 500 * time.Millisecond
	)

	start := time.Now()
	timeout := promptDismissTimeout
	if m.promptDismissTimeout > 0 {
		timeout = m.promptDismissTimeout
	}
	deadline := start.Add(timeout)
	lastPane := ""

	for time.Now().Before(deadline) {
		interval := promptPollInterval
		if time.Since(start) < promptFastPollWindow {
			interval = promptFastPollInterval
		}
		m.sleepDuringPromptDismiss(interval)

		pane := m.captureVisiblePaneForAgent(agent)
		if pane == "" {
			continue
		}

		// Bypass-permissions consent screen: handle first and explicitly,
		// even if the pane is unchanged since the last poll (a mistimed
		// keystroke must be retried, not skipped). The affirmative option
		// sits below the default "No, exit".
		if strings.Contains(pane, bypassConsentTitle) && !strings.Contains(pane, cliWorkingMarker) {
			m.logger.Info("accepting bypass-permissions consent", "agent", agent.Name)
			m.confirmMenuOption(agent, bypassConsentTitle, bypassConsentAcceptOption, "Down")
			lastPane = "" // re-capture fresh on the next pass
			continue
		}

		// Custom-API-key approval prompt: the affirmative "Yes" sits ABOVE
		// the default "No (recommended)" selection, so the generic
		// Down-then-Enter fallback below would decline it.
		if strings.Contains(pane, apiKeyPromptTitle) && !strings.Contains(pane, cliWorkingMarker) {
			m.logger.Info("approving seeded inference API key", "agent", agent.Name)
			m.confirmMenuOption(agent, apiKeyPromptTitle, apiKeyPromptAcceptOption, "Up")
			lastPane = ""
			continue
		}

		if pane == lastPane {
			continue
		}
		lastPane = pane

		// Main prompt visible — agent is ready
		if strings.Contains(pane, "bypass permissions on") || strings.Contains(pane, "esc to interrupt") {
			m.logger.Info("inference agent ready", "agent", agent.Name)
			return
		}

		// "Press Enter to continue" screens
		if strings.Contains(pane, "Press Enter to continue") {
			m.logger.Info("inference prompt: press enter", "agent", agent.Name)
			m.tmuxSendKeysForAgent(agent, "Enter")
			continue
		}

		// Selection prompts have "Enter to confirm" footer
		if !strings.Contains(pane, "Enter to confirm") {
			continue
		}

		// Find the currently selected option (marked with ❯)
		selected := selectedMenuOption(pane)

		m.logger.Info("inference prompt detected",
			"agent", agent.Name,
			"selected", selected,
		)

		// If current selection looks negative, navigate away from it
		selectedLower := strings.ToLower(selected)
		if strings.Contains(selectedLower, "no,") || strings.Contains(selectedLower, "no ") ||
			strings.Contains(selectedLower, "exit") {
			// Try moving down first (most prompts put the positive option below)
			m.tmuxSendKeysForAgent(agent, "Down")
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
		} else if strings.Contains(selectedLower, "fix with") {
			// Settings error: skip past "Fix with Claude" and "Exit" to "Continue without"
			m.tmuxSendKeysForAgent(agent, "Down")
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
			m.tmuxSendKeysForAgent(agent, "Down")
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
		}

		m.tmuxSendKeysForAgent(agent, "Enter")
	}

	m.logger.Warn("inference prompt dismissal timed out", "agent", agent.Name)
}

func (m *Manager) sleepDuringPromptDismiss(d time.Duration) {
	m.term().Sleep(d)
}

// selectedMenuOption returns the trimmed text of the "❯"-selected line of an
// interactive CLI menu, or "" if no line is selected.
func selectedMenuOption(pane string) string {
	for _, line := range strings.Split(pane, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "❯") {
			return trimmed
		}
	}
	return ""
}

// confirmMenuOption drives an interactive CLI menu identified by title to the
// option whose text contains want, then confirms it with Enter. Navigation
// matches the "❯"-selected line text rather than pressing a fixed number of
// keys, so it lands on the right option whichever position it occupies (menu
// option order differs between Claude CLI versions). navKey is the arrow key
// to step with ("Down" or "Up"). Returns true once the option was confirmed
// or the screen is gone.
func (m *Manager) confirmMenuOption(agent *AgentProcess, title, want, navKey string) bool {
	const (
		// menuMaxNavigateSteps bounds arrow-key navigation; the handled menus
		// have 2 options, extra headroom covers future variants.
		menuMaxNavigateSteps = 4
		postKeystrokeDelay   = 500 * time.Millisecond
	)
	for step := 0; step < menuMaxNavigateSteps; step++ {
		pane := m.captureVisiblePaneForAgent(agent)
		if !strings.Contains(pane, title) || strings.Contains(pane, cliWorkingMarker) {
			return true // screen already dismissed
		}
		if strings.Contains(selectedMenuOption(pane), want) {
			m.tmuxSendKeysForAgent(agent, "Enter")
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
			return true
		}
		m.tmuxSendKeysForAgent(agent, navKey)
		m.sleepDuringPromptDismiss(postKeystrokeDelay)
	}
	m.logger.Warn("inference menu: wanted option not reached",
		"agent", agent.Name, "title", title, "want", want)
	return false
}

const (
	// consentStuckGracePeriod is how long a consent screen must stay visible
	// across watcher passes before the agent counts as stuck. The launch-time
	// dismissal goroutine runs for 60s, so a screen still visible this long
	// after first being seen by the watcher means dismissal lost the race.
	consentStuckGracePeriod = 30 * time.Second
	// consentDismissCooldown is the minimum interval between watcher-triggered
	// dismissal passes for one agent, so a stubborn screen can't spam
	// keystroke goroutines (each dismissal pass itself polls for 60s).
	consentDismissCooldown = 2 * time.Minute
)

// clearConsentTracking resets the consent-stuck timer for an agent whose pane
// no longer shows a consent screen.
func (m *Manager) clearConsentTracking(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.consentSeenAt = time.Time{}
	}
}

// dismissConsentIfStuck re-runs dismissInferencePrompts for an inference agent
// whose pane has shown a consent screen for longer than the grace period,
// subject to a per-agent cooldown. Called from the watcher loop
// (CheckAndRestartCrashedAgents) so an agent that lands on a consent screen
// after launch — e.g. a crash-recovery restart whose launch-time dismissal
// timed out — recovers instead of sitting stuck while kicks appear to succeed.
func (m *Manager) dismissConsentIfStuck(name string) {
	now := time.Now()
	m.mu.Lock()
	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	if agent.consentSeenAt.IsZero() {
		agent.consentSeenAt = now
		m.mu.Unlock()
		return
	}
	stuckFor := now.Sub(agent.consentSeenAt)
	if stuckFor < consentStuckGracePeriod || now.Sub(agent.lastConsentDismiss) < consentDismissCooldown {
		m.mu.Unlock()
		return
	}
	agent.lastConsentDismiss = now
	m.mu.Unlock()

	m.logger.Warn("inference agent stuck on consent screen, re-running prompt dismissal",
		"name", name, "stuck_seconds", int(stuckFor.Seconds()))
	go m.dismissInferencePrompts(agent)
}

func (m *Manager) markProviderErrorLocked(agent *AgentProcess, match providerErrorMatch, now time.Time) time.Duration {
	// BackendAuth (#6558) is updated on every observation of this match,
	// independent of the backoff early-return below: an operator watching the
	// canary needs "still unlicensed" to keep its original Since even while
	// the backoff timer itself is not restarted.
	if status, ok := classifyBackendAuthStatus(match.Class, match.Line); ok {
		agent.markBackendAuthLocked(status, match.Line, now)
		// #6767: an unlicensed verdict against a copilot agent is direct
		// upstream evidence that whatever Copilot token this agent is
		// actually using has no license. If the hive currently treats a
		// token as AUTHORITATIVE, that same token is the one being pinned
		// into every agent's environment (COPILOT_GITHUB_TOKEN) and into
		// the shared CLI config, so the "not licensed" verdict IS a verdict
		// on the authoritative token. Latch that here so syncCopilotToken
		// stops clobbering an operator's recovery /login with the known-bad
		// authoritative token. Cleared as soon as setCopilotToken installs
		// a different value (recovery succeeded, or dashboard re-login).
		if status == BackendAuthUnlicensed && agent.Config.Backend == "copilot" && m.copilotAuthTokenAuthoritative {
			m.copilotAuthTokenRejected = true
		}
	}
	if !agent.ProviderErrorBackoffUntil.IsZero() && now.Before(agent.ProviderErrorBackoffUntil) &&
		agent.ProviderErrorClass == match.Class && agent.ProviderErrorLine == match.Line {
		return agent.ProviderErrorBackoffUntil.Sub(now)
	}
	agent.providerErrorBackoffAttempt++
	delay := providerErrorBackoffDelay(agent.providerErrorBackoffAttempt)
	agent.ProviderErrorBackoffUntil = now.Add(delay)
	agent.ProviderErrorClass = match.Class
	agent.ProviderErrorLine = match.Line
	agent.LastError = match.Line
	agent.lastInferKickPane = ""
	agent.actionNudgeSent = false
	return delay
}

func (m *Manager) clearProviderErrorLocked(agent *AgentProcess, now time.Time) {
	// BackendAuth (#6558) clears whenever the watchdog finds no provider
	// error on a pane it just checked — the spoke's evidence of a successful
	// turn — regardless of whether ProviderErrorClass was already empty.
	agent.clearBackendAuthLocked(now)
	if agent.ProviderErrorClass == "" && agent.ProviderErrorLine == "" && agent.ProviderErrorBackoffUntil.IsZero() {
		return
	}
	if agent.LastError == agent.ProviderErrorLine {
		agent.LastError = ""
	}
	agent.ProviderErrorClass = ""
	agent.ProviderErrorLine = ""
	agent.ProviderErrorBackoffUntil = time.Time{}
	agent.providerErrorBackoffAttempt = 0
}

func (m *Manager) providerErrorBackoffRemainingLocked(agent *AgentProcess, now time.Time) time.Duration {
	if agent == nil || agent.ProviderErrorBackoffUntil.IsZero() || !now.Before(agent.ProviderErrorBackoffUntil) {
		return 0
	}
	return agent.ProviderErrorBackoffUntil.Sub(now)
}

// ProviderErrorBackoffRemaining reports the active inference-provider backoff
// for a dashboard/governor caller that wants to avoid even attempting a kick.
func (m *Manager) ProviderErrorBackoffRemaining(name string) (time.Duration, string, string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, ok := m.agents[name]
	if !ok {
		return 0, "", "", false
	}
	remaining := m.providerErrorBackoffRemainingLocked(agent, time.Now())
	if remaining <= 0 {
		return 0, agent.ProviderErrorClass, agent.ProviderErrorLine, false
	}
	return remaining, agent.ProviderErrorClass, agent.ProviderErrorLine, true
}

// GetStatusFast returns an agent snapshot without ever waiting on the global
// manager lock.
//
// m.mu is a SINGLE GLOBAL lock, and restartWithReason holds it in write mode
// across the entire tmux relaunch — kill-session, ensureTmuxSession, the
// caveman install, send-keys and their sleeps. Measured on a live spoke that
// is an 11.25s hold, and three restarts can land inside 30s. Any handler whose
// first action is GetStatus therefore stalls for the length of somebody else's
// restart, which is why every agent settings dialog sat on "Loading..." while
// one unrelated agent was relaunching (#7417). Agents do not have independent
// locks: restarting one blocks reads for all of them.
//
// So: try the lock, and if a writer holds it (or is waiting — Go's RWMutex
// makes TryRLock fail for a pending writer, which is exactly what we want),
// fall back to the last snapshot instead of blocking. Callers that only need
// display fields get a value that is at worst one restart stale, which beats a
// 12-second spinner. If no snapshot has been taken yet this returns an error,
// and every current caller already degrades to its configured values on error.
//
// This does NOT fix the lock hold itself — restartWithReason still needs to be
// phased so the relaunch happens outside m.mu. It stops that hold from being
// visible to readers who never needed to be serialized against it.
func (m *Manager) GetStatusFast(name string) (*AgentProcess, error) {
	if m.mu.TryRLock() {
		agent, ok := m.agents[name]
		if !ok {
			m.mu.RUnlock()
			return nil, fmt.Errorf("agent %s not found", name)
		}
		snap := agent.snapshot()
		m.mu.RUnlock()

		m.statusSnapMu.Lock()
		if m.statusSnaps == nil {
			m.statusSnaps = make(map[string]*AgentProcess)
		}
		// Cache the same pointer we return: AgentProcess contains locks
		// (paneMu), so copying the value trips govet copylocks. Snapshots
		// are read-only display values by contract; a later refresh
		// replaces the map entry rather than mutating this one.
		m.statusSnaps[name] = &snap
		m.statusSnapMu.Unlock()

		return &snap, nil
	}

	m.statusSnapMu.RLock()
	cached, ok := m.statusSnaps[name]
	m.statusSnapMu.RUnlock()
	if ok && cached != nil {
		// Returned as-is (no copy) for the same copylocks reason; stale
		// snapshots are read-only.
		return cached, nil
	}
	return nil, fmt.Errorf("agent %s status unavailable: manager busy", name)
}

func (m *Manager) GetStatus(name string) (*AgentProcess, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}
	snap := agent.snapshot()
	return &snap, nil
}

// CountAgentsWithModel returns how many agents have an effective method
// (backend) or model assigned, resolving overrides ahead of config exactly as
// the launcher does. Reported to the hub so it can tell whether this hive has
// completed the "assign a method/model to an agent" adoption step.
//
// An agent counts if EITHER a backend or a model is set: "claude with the
// default model" and "the governor's default backend pinned to a specific
// model" are both real assignments. Values like "auto" and "default" are
// deliberate routing selections, not absences, so they count too — only a
// wholly empty backend AND model reads as unassigned.
func (m *Manager) CountAgentsWithModel() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, a := range m.agents {
		if a == nil {
			continue
		}
		backend := a.Config.Backend
		if a.BackendOverride != "" {
			backend = a.BackendOverride
		}
		model := a.Config.Model
		if a.ModelOverride != "" {
			model = a.ModelOverride
		} else if a.PinnedModel != "" {
			model = a.PinnedModel
		}
		if strings.TrimSpace(backend) != "" || strings.TrimSpace(model) != "" {
			count++
		}
	}
	return count
}

func (m *Manager) AllStatuses() map[string]*AgentProcess {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]*AgentProcess, len(m.agents))
	for k, v := range m.agents {
		snap := v.snapshot()
		result[k] = &snap
	}
	return result
}

// backendBinaryAliases names the backends whose binary is NOT simply the
// backend name. Only genuine aliases belong here: every other CLI backend is
// derived from config.CLIBackends by identity, and every routable model-gateway
// backend is resolved by Manager.backendBinaryName. Keeping this map to aliases
// only is what makes the accept-then-fail class of bug structurally impossible.
var backendBinaryAliases = map[string]string{
	// pi was previously aliased to "goose", which made every pi-configured
	// agent exec the goose CLI instead of pi (the backend launch command
	// switch now has a real pi case). pi is a first-class CLI backend
	// (config.CLIBackends includes "pi"), so identity mapping applies.
}

// backendBinaryName maps a config-independent agent backend to the NAME of the
// CLI binary that is exec'd for it, without touching the filesystem. Split out
// from backendBinary so the "every supported backend resolves" invariant can be
// tested without requiring each CLI to be installed on the test machine.
//
// Both canonical lists are derived rather than written out here:
//
//   - config.CLIBackends (claude, copilot, goose, codex, pi, bob, aider, gemini)
//     each launch a binary of the same name, except for the aliases above.
//   - config.InferenceBackends (vllm, llm-d, litellm, watsonx) all launch the
//     SAME claude CLI, pointed at hive's local OpenAI-compatible translator via
//     ANTHROPIC_BASE_URL — the backend name selects the upstream route, not the
//     binary.
//
// Deriving both means a backend added to either list can never again be
// accepted by config.ValidateBackend and then rejected hours later at kick time
// with "unknown backend". Previously only InferenceBackends was derived, so
// codex and aider were valid config values that failed at launch.
func backendBinaryName(backend string) (string, error) {
	binaries := make(map[string]string, len(config.CLIBackends)+len(config.InferenceBackends))
	for _, b := range config.CLIBackends {
		binaries[b] = b
	}
	for _, b := range config.InferenceBackends {
		binaries[b] = "claude"
	}
	for backend, binary := range backendBinaryAliases {
		binaries[backend] = binary
	}

	binary, ok := binaries[backend]
	if !ok {
		return "", fmt.Errorf("unknown backend: %s", backend)
	}
	return binary, nil
}

// backendBinaryName resolves both config-independent backends and live
// configured gateway names. A gateway name validates via Manager.routableBackend,
// so the launch path must use the same predicate and route it through claude.
func (m *Manager) backendBinaryName(backend string) (string, error) {
	if binary, err := backendBinaryName(backend); err == nil {
		return binary, nil
	}
	if m != nil && m.routableBackend(backend) {
		return "claude", nil
	}
	return "", fmt.Errorf("unknown backend: %s", backend)
}

// backendBinary resolves an agent backend to the absolute path of the CLI
// binary that is actually exec'd for it.
func backendBinary(backend string) (string, error) {
	binary, err := backendBinaryName(backend)
	if err != nil {
		return "", err
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("backend %s not found in PATH: %w", backend, err)
	}

	return path, nil
}

func (m *Manager) backendBinary(backend string) (string, error) {
	binary, err := m.backendBinaryName(backend)
	if err != nil {
		return "", err
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("backend %s binary %s not found in PATH: %w", backend, binary, err)
	}

	return path, nil
}

func (m *Manager) backendLaunchFailureMessage(backend string, err error) string {
	binary, nameErr := m.backendBinaryName(backend)
	if nameErr != nil {
		return fmt.Sprintf(
			"backend %s did not launch: %v. This backend is not a supported CLI, built-in inference backend, or configured model gateway; switch this agent to a supported backend or configure a matching model gateway.",
			backend, err)
	}
	return fmt.Sprintf(
		"backend %s did not launch: %v. The %s CLI required for this backend is not installed in this hive image — upgrade the hive image or switch this agent to a different backend.",
		backend, err, binary)
}

const (
	sharedConfigDesiredMode = 0o660
	// agyDefaultEffort is the reasoning effort passed alongside agy's --model
	// when the agent has no usable reasoning_effort configured (see
	// agyLaunchEffort). agy requires --effort whenever --model is given and
	// otherwise ignores the model entirely; "low" is the effort agy defaults
	// to on its own, so this makes the configured model take effect without
	// changing behaviour.
	agyDefaultEffort = "low"

	tokenRestartCooldownSec = 60 // minimum seconds between token-triggered restarts per agent
	// loginPromptTailLines bounds the pane region the login-prompt detector
	// reads: a prompt the CLI is stuck at sits at the pane bottom, while
	// echoed kick text and startup flashes live in scrollback (see the poller).
	loginPromptTailLines = 15
	// loginStreakRestartMin is how many consecutive polls (~3s apart) must see
	// the login prompt before a token-triggered restart may fire — filters the
	// CLI's transient startup "/login" flash.
	loginStreakRestartMin = 3
	// tokenRestartMaxAttempts bounds CONSECUTIVE token-triggered restarts that
	// fail to clear the login prompt.
	//
	// The three guards above answer WHEN to restart; none of them answered HOW
	// MANY TIMES, so a restart that could never work was retried forever at the
	// cooldown interval. #4596 is precisely that shape: the shared credential is
	// valid (so configHasTokens() is true) while $HOME/.claude.json has lost its
	// oauthAccount (so the CLI shows the login menu regardless), and each
	// restart re-launched a CLI that rewrote the same contended file and asked
	// again. Restarts are not free — they destroy in-flight work, which is the
	// failure the kick grace above was added for.
	//
	// Three is deliberately generous: one restart genuinely does fix the case
	// this feature was built for (an operator authenticates in one agent's
	// terminal and the others need a nudge), so the cap only engages on a
	// theory that has now failed repeatedly.
	tokenRestartMaxAttempts = 3
	// tokenRestartKickGrace suppresses token-triggered restarts after a kick
	// delivery so the restart can never destroy just-delivered work.
	tokenRestartKickGrace      = 10 * time.Minute
	expiredTokenHangTimeoutSec = 180 // blank pane after this many seconds triggers token purge + restart
	tlsErrorRestartCooldownSec = 120 // minimum seconds between TLS-error-triggered restarts per agent
)

// loginPromptPatterns are substrings that indicate an agent is stuck on a

// codexBackend is the backend name for the OpenAI Codex CLI.
const codexBackend = "codex"

// bobBackend is the backend name for the IBM bobshell ("bob") CLI.
const bobBackend = "bob"

// normalizeModelName converts YAML-friendly model names to the format each
// CLI backend expects. Claude CLI uses hyphens (claude-opus-4-7), while
// gemini/goose/agy-style backends use dots (claude-opus-4.7).
//
// copilot does NOT take the blind trailing-digits dot-rewrite below: the
// Copilot CLI's --model nomenclature mixes separators per model family
// (claude-fable-5 is DASHED, claude-opus-4.6 is DOTTED), so the rewrite
// corrupted every dashed-family id — verified live, copilot CLI v1.0.78
// rejected the rewritten `claude-fable.5` ("is not available") and fell back
// to a different model (#4262). copilot instead uses the alias-based
// CanonicalizeCopilotModel (copilot_models.go), which normalizes separator
// drift against the known CLI-accepted list in both directions and passes
// unknown ids through verbatim. Applied here — at launch time — so an
// already-stored bad id self-corrects on existing spokes without operator
// action.
//
// Self-hosted inference backends (vllm, llm-d, litellm) and configured gateway
// names are the outbound gateway model id verbatim — the string must match an
// entitled model on the gateway EXACTLY (prefixes like "Azure/", dots vs
// hyphens, case). Rewriting it (e.g. "Azure/gpt-4" -> "Azure/gpt.4",
// "gpt-4o-2024-08-06" -> "gpt-4o-2024-08.06") produces a model the team is not
// entitled to and the gateway 403s ("team not allowed to access model") even
// for entitled models. So never normalize inference model names — pass them
// through untouched.
//
// bob is likewise excluded. bobLaunchCmd passes no --model at all (bob
// auto-selects), so this is defense-in-depth rather than the fix: the value is
// still computed and logged on the bob launch path, and the dot-rewrite is
// what turned a configured `claude-sonnet-4-6` into the unknown
// `claude-sonnet-4.6` that made bob die with "Cannot read properties of
// undefined (reading 'maxTokens')". Leaving it unrewritten keeps logs honest
// about what was configured and stops the corrupted id from being handed to a
// future bob consumer.
func normalizeModelNameForBackend(model, backend string, inferenceRoutable bool) string {
	if backend == "claude" || backend == bobBackend || inferenceRoutable {
		return model
	}
	if backend == "copilot" {
		return CanonicalizeCopilotModel(model)
	}
	idx := strings.LastIndex(model, "-")
	if idx < 0 || idx == len(model)-1 {
		return model
	}
	suffix := model[idx+1:]
	allDigits := true
	for _, c := range suffix {
		if c < '0' || c > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return model[:idx] + "." + suffix
	}
	return model
}

func normalizeModelName(model, backend string) string {
	return normalizeModelNameForBackend(model, backend, IsInferenceBackend(backend))
}

// ClearModeOverrides clears Config.Mode for the NAMED agents so that
// DefaultAgentMode determines their mode from the ACMM level. Call it before
// SyncModeFiles when applying a pack, because a pack agent's Config.Mode may
// have been set by a previous level's pack and would otherwise override the
// new level's expected default.
//
// Scoped to a name list — the pack's roster — on purpose (#7503). The previous
// ClearAllModeOverrides wiped every agent in the process table, including
// agents no pack manages. Their Mode was never pack-seeded, so there is no
// stale pack value to clear: what got cleared was the OPERATOR's setting. On
// the projectbluefin spoke `reviewer` was `mode: ADVISORY` in every config
// layer and ran as ISSUES_AND_PRS, the L5 default — two rungs more authority
// than anything on disk granted it — because the startup pack apply cleared it
// here and SyncModeFiles then wrote the default. Names not in the process
// table are ignored.
func (m *Manager) ClearModeOverrides(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range names {
		if agent, ok := m.agents[name]; ok {
			agent.Config.Mode = ""
		}
	}
}

// SyncModeFiles rewrites /tmp/.hive-mode-* for all running agents to reflect the given ACMM level.
func (m *Manager) SyncModeFiles(level int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, agent := range m.agents {
		if agent.Paused {
			continue
		}
		mode := DefaultAgentMode(name, level)
		// converseConfigured is logged on both branches below so one grep by
		// agent name shows whether the capability came from config or fell
		// back to the default alongside the mode decision (#7503).
		converseConfigured := agent.Config.Converse != nil
		if modeStr := agent.Config.Mode; modeStr != "" {
			if parsed, ok := ParseAgentMode(modeStr); ok {
				m.logger.Info("SyncModeFiles: Config.Mode override",
					"agent", name, "level", level,
					"default", DefaultAgentMode(name, level).String(),
					"override", modeStr,
					"converse_configured", converseConfigured)
				mode = parsed
			}
		} else {
			// Log the fallback too, not only the override (#7503). Before this,
			// an agent whose configured mode had been dropped on the way to the
			// process table was indistinguishable from one that never set a
			// mode: the only signal was reading /tmp/.hive-mode-<agent> and
			// comparing by hand. One line saying "config said nothing, using
			// the level default" turns that into a grep. `overlay_file` names
			// the per-agent file when the entry came from one, so an operator
			// can check what it says against what is being used.
			m.logger.Info("SyncModeFiles: no Config.Mode, using level default",
				"agent", name, "level", level,
				"default", mode.String(),
				"converse_configured", converseConfigured,
				"overlay_file", agent.Config.SourceFile())
		}
		modeFile := filepath.Join(agentStateDir, ".hive-mode-"+name)
		if err := writeAgentStateFile(modeFile, []byte(mode.String())); err != nil {
			m.logger.Warn("SyncModeFiles: write failed", "file", modeFile, "error", err)
		}
		// The capability file rides the same sync (#4492). It is level-independent
		// today, but writing it here is what makes a `converse` change take effect
		// on the next reconcile instead of only at the next agent launch.
		caps := DefaultCapabilities(mode, level)
		if agent.Config.Converse != nil {
			caps.Converse = *agent.Config.Converse
		}
		m.writeAgentCapsFile(name, caps)
	}
}

// agentCapabilities returns the ORTHOGONAL capabilities for a given agent
// (#4492). Unlike agentMode there is no per-level default table: `converse` is
// opt-in everywhere, so an agent whose config says nothing gets the zero value
// and behaves exactly as it did before capabilities existed.
func (m *Manager) agentCapabilities(agent *AgentProcess) AgentCapabilities {
	caps := DefaultCapabilities(m.agentMode(agent), m.project.ACMMLevel)
	if agent.Config.Converse != nil {
		caps.Converse = *agent.Config.Converse
	}
	return caps
}

// writeAgentCapsFile persists the capability set the proxy reads on the request
// path. It is written for EVERY agent, including those with no capabilities, so
// a cleared `converse` actually revokes: leaving a stale file behind would keep
// granting the capability after the operator turned it off.
func (m *Manager) writeAgentCapsFile(name string, caps AgentCapabilities) {
	capsFile := filepath.Join(agentStateDir, ".hive-caps-"+name)
	if err := writeAgentStateFile(capsFile, []byte(caps.String())); err != nil {
		m.logger.Warn("caps file write failed", "file", capsFile, "error", err)
	}
}

// agentMode returns the GitHub interaction mode for a given agent at the current ACMM level.
// If the agent has an explicit Mode in its config (hive.yaml or pack YAML), that takes precedence.
// Otherwise, the default table by ACMM level is used.
func (m *Manager) agentMode(agent *AgentProcess) AgentMode {
	if modeStr := agent.Config.Mode; modeStr != "" {
		if parsed, ok := ParseAgentMode(modeStr); ok {
			return parsed
		}
	}
	return DefaultAgentMode(agent.Name, m.project.ACMMLevel)
}

// DefaultAgentMode returns the default mode for a given agent name and ACMM level,
// ignoring any hive.yaml override. Used by the dashboard to show "(default)" indicators.
func DefaultAgentMode(agentName string, level int) AgentMode {
	if agentName == "supervisor" {
		return ModeAdvisory
	}
	switch level {
	case 1:
		return ModeAdvisory
	case 2:
		return ModeAdvisory
	case 3:
		if agentName == "quality" {
			return ModeIssuesAndPRs
		}
		return ModeAdvisory
	case 4:
		switch agentName {
		case "quality", "sec-check", "ci-maintainer":
			return ModeIssuesAndPRs
		case "scanner", "guide":
			return ModeIssuesOnly
		default:
			return ModeAdvisory
		}
	case 5:
		return ModeIssuesAndPRs
	case 6:
		if agentName == "scanner" {
			return ModeIssuesPRsMerge
		}
		return ModeIssuesAndPRs
	default:
		return ModeAdvisory
	}
}

// agentCanWrite returns true if this agent is allowed to push branches and create PRs.
// Deprecated: use agentMode() for granular mode checks.
func (m *Manager) agentCanWrite(agent *AgentProcess) bool {
	return m.agentMode(agent).CanPush()
}

// AuthorizePROpen enforces the policy for the hive-opens-PR watcher: an agent
// may open a PR (by dropping a request file) only if BOTH hold:
//
//  1. Forge-resistance — the request file's owning UID (fileUID) maps to the
//     agent it claims to be (via the uid-map). One agent cannot open a PR "as"
//     another, and a non-agent process (unknown UID) is refused. When per-agent
//     UIDs are not in play (fileUID <= 0, e.g. shared-dev-UID mode with no map),
//     ownership is unverifiable, so we fall back to the ACMM check alone rather
//     than hard-failing — the same posture the credential helper takes.
//  2. ACMM write-gate — the agent must be push-capable at the hive's current
//     ACMM level, i.e. exactly the CanPush() check that governs `gh pr create`.
//
// Returns nil to authorize, or an error describing the denial. This mirrors the
// direct PR path's policy so the request-file route grants no extra privilege.
func (m *Manager) AuthorizePROpen(agentName string, fileUID int) error {
	if strings.TrimSpace(agentName) == "" {
		return fmt.Errorf("no agent named in the request")
	}
	// Forge check: when we have a UID map and a real owning UID, the file owner
	// must BE this agent.
	if m.uidMap != nil && fileUID > 0 {
		owner := m.uidMap.LookupByUID(fileUID)
		if owner == "" {
			return fmt.Errorf("request file owned by unknown uid %d (not a registered agent)", fileUID)
		}
		if owner != agentName {
			return fmt.Errorf("request claims agent %q but file is owned by agent %q (uid %d)", agentName, owner, fileUID)
		}
	}
	// ACMM write-gate: resolve the agent and check CanPush.
	m.mu.RLock()
	agent := m.agents[agentName]
	m.mu.RUnlock()
	if agent == nil {
		return fmt.Errorf("unknown agent %q", agentName)
	}
	if !m.agentMode(agent).CanPush() {
		return fmt.Errorf("agent %q is not push-capable at this ACMM level (mode %s) — advisory agents may not open PRs",
			agentName, m.agentMode(agent).String())
	}
	return nil
}

// AuthorizeIssueOpen enforces the policy for the issue-request watcher,
// mirroring AuthorizePROpen with the mode gates that govern the direct gh
// paths: "issue" requests need CanCreateIssues() (mode >= ISSUES_ONLY);
// "comment" and "claim" requests need the same (commenting and claiming an
// issue are both issue-writes under the same tier). The same UID
// forge-resistance applies: the request file's owner must BE the claimed
// agent. A nil manager or unknown agent is denied.
func (m *Manager) AuthorizeIssueOpen(agentName string, fileUID int, kind string) error {
	if strings.TrimSpace(agentName) == "" {
		return fmt.Errorf("no agent named in the request")
	}
	if m.uidMap != nil && fileUID > 0 {
		owner := m.uidMap.LookupByUID(fileUID)
		if owner == "" {
			return fmt.Errorf("request file owned by unknown uid %d (not a registered agent)", fileUID)
		}
		if owner != agentName {
			return fmt.Errorf("request claims agent %q but file is owned by agent %q (uid %d)", agentName, owner, fileUID)
		}
	}
	m.mu.RLock()
	agent := m.agents[agentName]
	m.mu.RUnlock()
	if agent == nil {
		return fmt.Errorf("unknown agent %q", agentName)
	}
	if !m.agentMode(agent).CanCreateIssues() {
		return fmt.Errorf("agent %q may not create issues or comments at this ACMM level (mode %s)",
			agentName, m.agentMode(agent).String())
	}
	return nil
}

// AuthorizeReviewRequest enforces the policy for the review-request watcher.
// It mirrors AuthorizePROpen's forge-resistance, but grants on capability as
// well as mode (hivecommons/hive#7485).
//
// The review-request watcher is the SANCTIONED path: its doc comment tells an
// agent to write a request file "INSTEAD of running `gh pr review` from its
// own shell", because the file relay is App-authored and lands on the audit
// trail. But it was gated with AuthorizePROpen, which requires CanPush() —
// while the proxy, which is what a direct `gh pr review` goes through, grants
// the same write on Converse alone:
//
//	MinMode: agent.ModeIssuesAndPRs, Capability: agent.AgentCapabilities.CanConverse
//	                                            — pkg/proxy/rules.go:152
//
// So an ADVISORY+converse agent could post a review by going AROUND the relay
// but not THROUGH it: the audited path was strictly stricter than the
// unaudited one, which is exactly backwards — it pressures agents off the
// trail. This aligns the relay with the proxy so the sanctioned route is never
// the more restricted one. Converse stays conversation-only: it does not grant
// pushing, opening PRs, or merging, which all remain on the mode ladder.
//
// A nil manager or unknown agent is denied.
func (m *Manager) AuthorizeReviewRequest(agentName string, fileUID int) error {
	if strings.TrimSpace(agentName) == "" {
		return fmt.Errorf("no agent named in the request")
	}
	// Forge check: when we have a UID map and a real owning UID, the file owner
	// must BE this agent.
	if m.uidMap != nil && fileUID > 0 {
		owner := m.uidMap.LookupByUID(fileUID)
		if owner == "" {
			return fmt.Errorf("request file owned by unknown uid %d (not a registered agent)", fileUID)
		}
		if owner != agentName {
			return fmt.Errorf("request claims agent %q but file is owned by agent %q (uid %d)", agentName, owner, fileUID)
		}
	}
	m.mu.RLock()
	agent := m.agents[agentName]
	m.mu.RUnlock()
	if agent == nil {
		return fmt.Errorf("unknown agent %q", agentName)
	}
	if m.agentCapabilities(agent).CanConverse() {
		return nil
	}
	if !m.agentMode(agent).CanPush() {
		return fmt.Errorf("agent %q may not review PRs at this ACMM level (mode %s) and does not have the `converse` capability",
			agentName, m.agentMode(agent).String())
	}
	return nil
}

// AuthorizeMerge enforces the policy for the hive-merges-PR watcher, mirroring
// AuthorizePROpen but with the stricter CanMerge() gate: the request's agent
// must own the request file (forge-resistance) AND be merge-capable at the
// hive's current ACMM level (ModeIssuesPRsMerge). This keeps the file-based
// merge relay under the exact same authority as a direct merge would require —
// an issues/PRs agent that can open PRs still cannot merge them unless its mode
// grants merge. A nil manager or unknown agent is denied.
func (m *Manager) AuthorizeMerge(agentName string, fileUID int) error {
	if strings.TrimSpace(agentName) == "" {
		return fmt.Errorf("no agent named in the request")
	}
	// Forge check: when we have a UID map and a real owning UID, the file owner
	// must BE this agent.
	if m.uidMap != nil && fileUID > 0 {
		owner := m.uidMap.LookupByUID(fileUID)
		if owner == "" {
			return fmt.Errorf("request file owned by unknown uid %d (not a registered agent)", fileUID)
		}
		if owner != agentName {
			return fmt.Errorf("request claims agent %q but file is owned by agent %q (uid %d)", agentName, owner, fileUID)
		}
	}
	// ACMM merge-gate: resolve the agent and check CanMerge.
	m.mu.RLock()
	agent := m.agents[agentName]
	m.mu.RUnlock()
	if agent == nil {
		return fmt.Errorf("unknown agent %q", agentName)
	}
	if !m.agentMode(agent).CanMerge() {
		return fmt.Errorf("agent %q is not merge-capable at this ACMM level (mode %s) — only ISSUES_PRS_MERGE agents may merge PRs",
			agentName, m.agentMode(agent).String())
	}
	return nil
}

// AgentCapabilities reports whether the named agent is ABLE — at the hive's
// current ACMM level and the agent's effective mode — to create issues, open
// PRs, and merge PRs. These are the EXACT gates AuthorizePROpen (CanPush) and
// AuthorizeMerge (CanMerge) enforce, so a hub capability badge derived from
// these can never claim a capability the merge/PR relay would actually refuse.
// ok=false when the agent is unknown to the manager (the caller then reports
// "unknown", not a false negative). Read-only under RLock.
func (m *Manager) AgentCapabilities(agentName string) (canOpenIssue, canOpenPR, canMerge, ok bool) {
	m.mu.RLock()
	agent, exists := m.agents[agentName]
	m.mu.RUnlock()
	if !exists || agent == nil {
		return false, false, false, false
	}
	mode := m.agentMode(agent)
	return mode.CanCreateIssues(), mode.CanPush(), mode.CanMerge(), true
}

// EffectiveBackend reports the named agent's effective backend, honoring any
// runtime BackendOverride (see effectiveBackend). ok=false when the agent is
// unknown. Read-only under RLock — a small exported wrapper so callers outside
// the package (the heartbeat builder) need not reach into unexported state.
func (m *Manager) EffectiveBackend(agentName string) (backend string, ok bool) {
	m.mu.RLock()
	agent, exists := m.agents[agentName]
	m.mu.RUnlock()
	if !exists || agent == nil {
		return "", false
	}
	return effectiveBackend(agent), true
}

// InvocationMetadata reports the effective backend, model, and reasoning effort
// the hive invokes for the named agent, accounting for runtime overrides — the
// launch-time truth the invocation-attribution trail records (see pkg/github/attribution
// .go). ok=false when the agent is unknown to the manager (the caller then
// falls back to static config). Read-only under RLock; called from the
// PR-request watcher goroutine, never from the launch path.
func (m *Manager) InvocationMetadata(agentName string) (backend, model, effort string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, exists := m.agents[agentName]
	if !exists {
		return "", "", "", false
	}
	backend = effectiveBackend(agent)
	model = agent.Config.Model
	if agent.ModelOverride != "" {
		model = agent.ModelOverride
	}
	return backend, model, ResolveReasoningEffort(backend, model, agent.Config.ReasoningEffort), true
}

// ResolveReasoningEffort reports the reasoning effort the hive actually launches
// a given backend/model pair with, given the agent's configured reasoning_effort.
// Exported because the attribution trail is
// resolved in TWO places — Manager.InvocationMetadata above for a running agent,
// and cmd/hive's fallback that reads straight from config when the Manager does
// not know the agent — and both must give the same answer.
//
// Before this existed the fallback carried its own hardcoded "low", so changing
// agyDefaultEffort here would have left cmd/hive silently stamping PRs with an
// effort agy was no longer being launched with. An attribution trail that
// misreports is worse than one that says nothing.
//
// The rules mirror the launch path exactly:
//   - agy REQUIRES --effort whenever --model is given, so with a model it runs
//     at agyLaunchEffort(configured) and with no model at no effort at all.
//   - codex is launched with `-c model_reasoning_effort` only when an effort
//     is configured; unset means codex's own default, which the hive does not
//     resolve, so the honest answer is the configured value verbatim.
//   - every other backend takes its effort from its own config, which the
//     hive does not resolve here, so the honest answer is "".
func ResolveReasoningEffort(backend, model, configured string) string {
	switch backend {
	case "agy":
		if model != "" {
			return agyLaunchEffort(configured)
		}
		return ""
	case codexBackend:
		return configured
	}
	return ""
}

// filteredEnv returns os.Environ() with write-capable tokens removed for advisory agents.
// COPILOT_GITHUB_TOKEN is kept for all agents (needed for AI auth); write access is
// gated by --enable-all-github-mcp-tools flag. GH_TOKEN and GITHUB_TOKEN are stripped
// from non-quality agents to enforce gh-wrapper and credential helper policies.
func (m *Manager) filteredEnv(agent *AgentProcess) []string {
	env := os.Environ()
	if m.agentMode(agent).CanPush() {
		return env
	}
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "GH_TOKEN=") ||
			strings.HasPrefix(e, "GITHUB_TOKEN=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

// embeddedTokenRe matches git remote URLs with embedded credentials:
// https://x-access-token:TOKEN@github.com/org/repo.git
var embeddedTokenRe = regexp.MustCompile(`^https://[^@]+@(github\.com/.+)$`)

// sanitizeGitRemotes strips embedded tokens from git remote URLs in all repos
// under the agent's work directory. Copilot CLI embeds the GitHub App token
// directly in the remote URL when it clones, bypassing both the credential
// helper (Layer 1) and env var filtering (Layer 2).
func (m *Manager) sanitizeGitRemotes(agent *AgentProcess) {
	if m.agentMode(agent).CanPush() {
		return
	}
	agentDir := m.workDir + "/" + agent.Name
	_ = filepath.WalkDir(agentDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.Name() != ".git" || !d.IsDir() {
			return nil
		}
		repoDir := filepath.Dir(path)
		out, err := exec.Command("git", "-C", repoDir, "remote", "get-url", "origin").Output()
		if err != nil {
			return filepath.SkipDir
		}
		url := strings.TrimSpace(string(out))
		if match := embeddedTokenRe.FindStringSubmatch(url); match != nil {
			clean := "https://" + match[1]
			_ = exec.Command("git", "-C", repoDir, "remote", "set-url", "origin", clean).Run()
			m.logger.Info("stripped embedded token from git remote",
				"agent", agent.Name, "repo", repoDir)
		}
		return filepath.SkipDir
	})
}

// agentEnvPair is an unquoted key-value environment variable.
type agentEnvPair struct {
	Key   string
	Value string
	// Secret vars are set via tmux set-environment only, never on the command line.
	Secret bool
}

// inferenceQuietCLIEnv is the set of Claude CLI switches exported to
// inference-routed sessions so the CLI stops emitting non-inference traffic
// (telemetry, error reporting, nonessential lookups) to its Anthropic host.
var inferenceQuietCLIEnv = []string{
	"DISABLE_TELEMETRY",
	"DISABLE_ERROR_REPORTING",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
}

func (m *Manager) agentEnvPairs(agent *AgentProcess) []agentEnvPair {
	model := agent.Config.Model
	if agent.ModelOverride != "" {
		model = agent.ModelOverride
	}
	backend := agent.Config.Backend
	if agent.BackendOverride != "" {
		backend = agent.BackendOverride
	}
	displayName := agent.Config.DisplayName
	if displayName == "" {
		displayName = agent.Name
	}
	vars := []agentEnvPair{
		{"HIVE_AGENT", agent.Name, false},
		{"HIVE_AGENT_DISPLAY_NAME", displayName, false},
		{"HIVE_BACKEND", backend, false},
		{"HIVE_MODEL", model, false},
	}
	if hiveID := os.Getenv("HIVE_ID"); hiveID != "" {
		vars = append(vars, agentEnvPair{"HIVE_ID", hiveID, false})
	}
	vars = append(vars, agentEnvPair{"HIVE_ACMM_LEVEL", fmt.Sprintf("%d", m.project.ACMMLevel), false})

	mode := m.agentMode(agent)
	if agent.Config.Tools != nil {
		if effectiveMode := agent.Config.Tools.EffectiveMode(); effectiveMode != "" {
			vars = append(vars, agentEnvPair{"HIVE_AGENT_MODE", effectiveMode, false})
		} else {
			vars = append(vars, agentEnvPair{"HIVE_AGENT_MODE", mode.String(), false})
		}
	} else {
		vars = append(vars, agentEnvPair{"HIVE_AGENT_MODE", mode.String(), false})
	}
	modeFile := filepath.Join(agentStateDir, ".hive-mode-"+agent.Name)
	if err := writeAgentStateFile(modeFile, []byte(mode.String())); err != nil {
		m.logger.Warn("agentBootstrapEnv: mode file write failed", "file", modeFile, "error", err)
	}
	m.writeAgentCapsFile(agent.Name, m.agentCapabilities(agent))
	// Plain proxy URL without userinfo — Claude Code's native binary fails
	// to open a socket when the URL contains username:password@ (FailedToOpenSocket).
	// Agent identification uses UID-based /proc/net/tcp lookup instead of
	// Proxy-Authorization headers. GIT_TERMINAL_PROMPT=0 prevents git from
	// prompting for proxy credentials.
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyListenPort)
	vars = append(vars, agentEnvPair{"HTTPS_PROXY", proxyURL, false})
	vars = append(vars, agentEnvPair{"HTTP_PROXY", proxyURL, false})
	vars = append(vars, agentEnvPair{"HIVE_PROXY_AGENT", agent.Name, false})
	vars = append(vars, agentEnvPair{"GIT_TERMINAL_PROMPT", "0", false})
	vars = append(vars, agentEnvPair{"NODE_EXTRA_CA_CERTS", proxyCACertPath, false})
	if sha := os.Getenv("HIVE_SHA"); sha != "" {
		vars = append(vars, agentEnvPair{"HIVE_SHA", sha, false})
	}
	if advisory := os.Getenv("HIVE_ADVISORY_ISSUE"); advisory != "" {
		vars = append(vars, agentEnvPair{"HIVE_ADVISORY_ISSUE", advisory, false})
	}
	// HIVE_REPO / HIVE_REPOS: the shipped policy templates instruct agents to
	// run `gh issue create --repo "$HIVE_REPO"`, but nothing ever exported it
	// to hosted agents (only the OSS scheduler set a hardcoded "<org>/hive").
	// Root-caused on a live hosted hive (2026-08-20): the sec-check agent saw
	// HIVE_REPO unset, fell back to the git remote of its own workdir, and
	// silently scanned only the primary repo — the other project repos were
	// never touched. Export the primary repo and the full project repo list so
	// templates and agents can target every configured repo.
	if m.project.Org != "" && len(m.project.Repos) != 0 {
		primary := m.project.PrimaryRepo()
		if primary == "" {
			primary = m.project.Repos[0]
		}
		vars = append(vars, agentEnvPair{"HIVE_REPO", m.project.Org + "/" + primary, false})
		full := make([]string, len(m.project.Repos))
		for i, r := range m.project.Repos {
			full[i] = m.project.Org + "/" + r
		}
		vars = append(vars, agentEnvPair{"HIVE_REPOS", strings.Join(full, ","), false})
	}
	// GH_HOST: point the gh CLI at the configured forge host for GHE spokes.
	// See ProjectContext.GHHost. The gh wrapper pairs this with
	// GH_ENTERPRISE_TOKEN so the per-agent scoped token authenticates there.
	if m.project.GHHost != "" {
		vars = append(vars, agentEnvPair{"GH_HOST", m.project.GHHost, false})
	}
	if m.routableBackend(backend) {
		const inferenceTranslatePort = 18444
		vars = append(vars, agentEnvPair{"ANTHROPIC_API_KEY", "sk-hive-" + agent.Name, false})
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", inferenceTranslatePort)
		vars = append(vars, agentEnvPair{"ANTHROPIC_BASE_URL", baseURL, false})
		vars = append(vars, agentEnvPair{"NO_PROXY", "127.0.0.1,localhost", false})
		// Cap the CLI output-token budget at a value every commercial model
		// litellm may front will accept. A prior 128000 (chosen so verbose
		// OSS models would not truncate) exceeds Azure GPT-4o's 16384
		// completion-token cap, so every request 400s with
		// "max_tokens is too large: 128000. This model supports at most
		// 16384 completion tokens". See inferenceMaxOutputTokensDefault.
		// TODO: the gateway 400 body names the model's real cap ("supports
		// at most N completion tokens"); a future enhancement could parse it
		// to auto-adjust per-model instead of using a universal floor.
		vars = append(vars, agentEnvPair{"CLAUDE_CODE_MAX_OUTPUT_TOKENS", strconv.Itoa(inferenceMaxOutputTokensDefault), false})
		// The Claude CLI sends telemetry batches, error reports, and other
		// non-inference traffic to its configured Anthropic host. Routed at
		// an OpenAI-compatible gateway that traffic has nowhere useful to go
		// (the proxy now answers it locally rather than forwarding it — see
		// classifyInferencePath), so switch it off at the source. Only for
		// inference-routed sessions: subscription/Anthropic-direct sessions
		// keep Anthropic's own telemetry.
		for _, v := range inferenceQuietCLIEnv {
			vars = append(vars, agentEnvPair{v, "1", false})
		}
	}
	if m.copilotAuthToken != "" {
		vars = append(vars, agentEnvPair{copilotTokenEnvVar, m.copilotAuthToken, true})
	}
	// Point the GitHub MCP server at the App installation token so PRs, issue
	// comments, and merges are authored by the App bot ("<slug>[bot]") — NOT by
	// the Copilot login user. COPILOT_GITHUB_TOKEN above stays as the Copilot
	// OAuth token because it authenticates the AI model (a separate concern from
	// GitHub write identity); leaving it untouched keeps the Copilot CLI login
	// working. The Copilot CLI reads GH_TOKEN / GITHUB_TOKEN for GitHub API auth
	// (per its README: GH_TOKEN or GITHUB_TOKEN, in that precedence), so setting
	// GITHUB_TOKEN here makes the built-in GitHub MCP server act as the App bot.
	//
	// Gated on the opt-in flag first (default OFF → no behavior change on any
	// hive that has not explicitly enabled App-bot authorship), then on CanPush():
	// advisory agents are deliberately kept GITHUB_TOKEN-less (see the -u
	// GITHUB_TOKEN strip after the env loop) so they cannot write; only push-
	// capable tiers — the ones that legitimately open/merge PRs — get the App
	// token. m.appAuth != nil means an App is configured. The value is the
	// per-agent tier-SCOPED App token, and refreshAgentTokens re-pushes it hourly
	// so it never goes stale.
	if m.project.AppAuthoredPRs && m.appAuth != nil && agent.UID > 0 && m.agentMode(agent).CanPush() {
		if data, err := os.ReadFile(ghpkg.AgentTokenCachePath(agent.Name)); err == nil {
			if tok := strings.TrimSpace(string(data)); tok != "" {
				vars = append(vars, agentEnvPair{"GITHUB_TOKEN", tok, true})
			}
		}
	}
	// Linear write credential for ISSUES_ONLY+ agents — see linearEnvPairs.
	// Nil for advisory agents and for hives with no Linear credential, so a
	// GitHub-only hive sees no change.
	vars = append(vars, m.linearEnvPairs(agent)...)
	// CLAUDE_CODE_OAUTH_TOKEN is a LAST RESORT, not the normal delivery path.
	//
	// Claude Code treats this variable as a static bearer token: when it is
	// set the CLI uses it verbatim, never opens ~/.claude/.credentials.json,
	// and therefore never refreshes. Measured in-container (2026-09-01): with
	// the variable set to a bad value and a perfectly good credentials file
	// beside it, the CLI answered "401 OAuth access token is invalid" — there
	// is no fallback to the file.
	//
	// m.claudeAuthToken is a snapshot of the SHORT-LIVED access token, taken
	// once at manager construction and refreshed only by ReloadClaudeToken()
	// after a dashboard login. Injecting it therefore pinned every claude
	// agent to the remaining life of whatever access token happened to be on
	// disk when the container started — Claude access tokens live 8h, so the
	// whole fleet 401'd within a day of every restart and the only recovery
	// hive offered was an operator re-login, once per agent. That is the daily
	// re-authentication treadmill of #5454.
	//
	// It is also unnecessary since per-agent homes (#4619): every agent's
	// ~/.claude is a symlink to the shared /data/home/.claude, so the CLI can
	// read the credential itself — and redeem its refresh grant on start,
	// which is the one thing the env var makes impossible.
	//
	// So inject ONLY when the agent has no credential file it can read. That
	// keeps the variable doing the job it was added for (#c5648bc9: deliver a
	// dashboard-obtained token to an agent that cannot see the file) and stops
	// it overriding a credential that can still refresh itself.
	if m.claudeAuthToken != "" && backend == "claude" && !claudeCredentialReachable(agent, backend) {
		vars = append(vars, agentEnvPair{"CLAUDE_CODE_OAUTH_TOKEN", m.claudeAuthToken, true})
	}
	// bob reads its key from BOBSHELL_API_KEY. Secret: true keeps the value off
	// the shell command line (out of `ps`, bash history, and pane scrollback);
	// it reaches the CLI via tmux set-environment only. Gated on the backend so
	// no other CLI's environment carries an IBM credential it has no use for.
	if backend == bobBackend {
		if key := m.bobAPIKey(); key != "" {
			vars = append(vars, agentEnvPair{config.BobAPIKeyEnvVar, key, true})
		}
		// BOBSHELL_DEFAULT_AUTH_TYPE is what actually selects API-key auth;
		// without it bob defaults to W3ID SSO and parks at the interactive key
		// prompt forever. Deliberately NOT Secret: the value is the literal
		// non-credential string "api-key", and secret pairs only reach a
		// freshly-created pane shell via tmux set-environment, whereas
		// non-secret pairs are re-applied on EVERY launch through
		// buildEnvPrefix. That asymmetry is exactly what caused the sibling
		// bug fixed in #2228, so the auth type must ride the always-reapplied
		// path or a relaunch into an existing session loses it.
		vars = append(vars, agentEnvPair{config.BobAuthTypeEnvVar, config.BobAuthTypeAPIKey, false})
	}
	// BD_DIR tells the `bd` CLI where to read/write beads. Without this,
	// bd falls back to cwd (/data/agents/<name>) instead of the configured
	// beads_dir (/data/beads/<name>), causing a path mismatch with the
	// dashboard and advisory digest.
	if agent.Config.BeadsDir != "" {
		vars = append(vars, agentEnvPair{"BD_DIR", agent.Config.BeadsDir, false})
	}
	if agent.Config.CavemanMode != "" {
		vars = append(vars, agentEnvPair{"HIVE_CAVEMAN_MODE", agent.Config.CavemanMode, false})
	}
	// Export the RESOLVED explain mode, not the raw config value, so an agent's
	// skills and helper scripts see the same answer the kick suffix acted on
	// (including inheritance from the hive-wide default and the off fallback for
	// an invalid value). Always exported, off included, so a script can branch on
	// it without having to re-derive the precedence rules itself.
	vars = append(vars, agentEnvPair{config.ExplainModeEnvVar, resolveExplainMode(agent.Config, m.explainModeDefault()), false})
	// GIT_SSL_CAINFO only — NOT SSL_CERT_FILE (that breaks Copilot API TLS)
	vars = append(vars, agentEnvPair{"GIT_SSL_CAINFO", proxyCACertPath, false})
	if agent.UID > 0 {
		vars = append(vars, agentEnvPair{"HIVE_AGENT_TOKEN_CACHE", ghpkg.AgentTokenCachePath(agent.Name), false})
	}
	if agent.UID > 0 {
		// Per-UID agents get a per-agent HOME (#4596) — AgentHome is the single
		// source of truth so the auth probe and this export can never diverge.
		vars = append(vars, agentEnvPair{"HOME", AgentHome(agent.Name, agent.UID, backend), false})

		// Per-agent XDG data/state roots (#6238), beneath the per-agent HOME.
		// Every backend CLI keeps its session transcripts, run locks and
		// caches under $XDG_DATA_HOME / $XDG_STATE_HOME; with the legacy
		// shared /data/home/.local those were one contended tree owned by
		// whichever agent wrote first. Exported explicitly (not left to the
		// spec default under $HOME) so the answer cannot depend on how each
		// CLI resolves XDG, and only when HOME itself is per-agent — the
		// HIVE_SHARED_AGENT_HOME=1 escape hatch keeps the legacy layout whole.
		// XDG_CONFIG_HOME is deliberately NOT set: ~/.config stays the shared
		// credential/config bridge (gh hosts.yml, goose config.yaml). See
		// setupAgentXDGDirs, which pre-creates these as the agent's own dirs.
		if xdgHome, ok := perAgentXDGHome(agent.Name, agent.UID, backend); ok {
			vars = append(vars, agentEnvPair{"XDG_DATA_HOME", agentXDGDataHome(xdgHome), false})
			vars = append(vars, agentEnvPair{"XDG_STATE_HOME", agentXDGStateHome(xdgHome), false})
		}

		// Under the per-agent-UID layout the global npm prefix is owned by the
		// image's build user, so the Claude Code CLI's self-updater fails on
		// every launch with "✘ Auto-update failed: no write permission to npm
		// prefix" — a red line in every agent pane for an update the agent must
		// not perform anyway (the CLI version is managed by the image, not by
		// an in-pod npm write). Disabling the updater removes the failure at its
		// source; a per-agent npm prefix would instead let an agent drift off
		// the pinned image version.
		vars = append(vars, agentEnvPair{"DISABLE_AUTOUPDATER", "1", false})
	}

	// Codex CLI 0.144.1's in-process app-server performs OWNER-gated operations
	// on files under CODEX_HOME (helper-binary "PATH alias" symlinks under
	// tmp/arg0, sqlite state). The shared /data/home/.codex is owned by dev
	// (the entrypoint chowns it group-writable + setgid), which claude/copilot
	// tolerate but Codex does not — every non-owner agent UID fails with
	// "failed to start embedded app server: Operation not permitted (os error 1)".
	// The manager launches the codex binary DIRECTLY (not via agent-launch.sh),
	// so CODEX_HOME must be set here. Give each agent its own dir; codex will
	// NOT create it (it errors "CODEX_HOME ... does not exist"), so it is
	// pre-created AS the agent below in setupCodexHome.
	if backend == codexBackend {
		vars = append(vars, agentEnvPair{"CODEX_HOME", codexHomePath(agent.Name), false})
	}

	for _, conn := range agent.Config.Connections {
		if conn.Type != "api" {
			continue
		}
		envName := conn.EnvName
		if envName == "" {
			envName = "HIVE_CONN_" + strings.ToUpper(strings.ReplaceAll(conn.Name, "-", "_")) + "_URL"
		}
		vars = append(vars, agentEnvPair{envName, conn.URI, false})
		if conn.Auth != nil && conn.Auth.Type == "env" && conn.Auth.EnvVar != "" {
			if tokenVal := os.Getenv(conn.Auth.EnvVar); tokenVal != "" {
				vars = append(vars, agentEnvPair{conn.Auth.EnvVar, tokenVal, true})
			}
		}
	}

	return vars
}

func (m *Manager) PinCLI(name, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.PinnedCLI = version
	m.logger.Info("agent CLI pinned", "name", name, "version", version)
	return nil
}

func (m *Manager) UnpinCLI(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.PinnedCLI = ""
	m.logger.Info("agent CLI unpinned", "name", name)
	return nil
}

func (m *Manager) PinModel(name, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	prevModel := agent.effectiveModel()
	agent.PinnedModel = model
	agent.ModelOverride = model
	m.logger.Info("agent model pinned", "name", name, "model", model)
	if prevModel != model {
		m.audit(AuditAgentModelSet, name, auditFields(
			"outcome", "success",
			"backend", agent.effectiveBackend(),
			"model", model,
			"previous_model", prevModel,
			"trigger", "pin",
		))
	}
	return nil
}

func (m *Manager) UnpinModel(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.PinnedModel = ""
	m.logger.Info("agent model unpinned", "name", name)
	return nil
}

func (m *Manager) SetModelOverride(name, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	// Store the CLI-accepted spelling for copilot so the persisted selection,
	// the dropdown preselect, and auto-heal all agree on one canonical id
	// (separator drift like claude-fable.5 vs claude-fable-5, #4262). Launch
	// re-applies the same canonicalization, so even ids stored before this
	// existed self-correct there.
	if agent.effectiveBackend() == "copilot" {
		model = CanonicalizeCopilotModel(model)
	}

	// A pin blocks the governor's auto-selection, never a user's explicit
	// switch: retarget the pin to the new model so the pin state is
	// unchanged (still pinned) while the change takes effect.
	if agent.PinnedModel != "" {
		agent.PinnedModel = model
		m.logger.Info("agent model pin retargeted by user switch", "name", name, "model", model)
	}

	prevModel := agent.effectiveModel()
	agent.ModelOverride = model
	m.logger.Info("agent model override set", "name", name, "model", model)
	// State CHANGES only — the governor re-asserts the current model on every
	// evaluation cycle, so auditing unchanged writes would flood the ring.
	if prevModel != model {
		m.audit(AuditAgentModelSet, name, auditFields(
			"outcome", "success",
			"backend", agent.effectiveBackend(),
			"model", model,
			"previous_model", prevModel,
		))
	}

	effectiveBackend := agent.Config.Backend
	if agent.BackendOverride != "" {
		effectiveBackend = agent.BackendOverride
	}
	if m.routableBackend(effectiveBackend) && m.inferenceRouteCallback != nil {
		m.inferenceRouteCallback(name, effectiveBackend, model)
	}
	return nil
}

func (m *Manager) SetBackendOverride(name, backend string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	// Refuse a backend the launcher cannot dispatch, at SET time. Previously
	// any string was accepted here and the agent was then restarted into it,
	// failing only at launch with "unknown backend: <x>" — the agent stops
	// working and the operator gets no signal at the moment of the change.
	// routableBackend covers configured gateway names (resolved live), so a
	// gateway-named backend still passes.
	if err := m.validateBackendName(backend); err != nil {
		return err
	}

	// Captured after validation so a rejected switch records no audit event:
	// the override is only mutated below, once the backend is known routable.
	prevBackend := agent.effectiveBackend()
	agent.BackendOverride = backend
	m.logger.Info("agent backend override set", "name", name, "backend", backend)
	// Record only a real transition: /switch/{backend} is also re-applied on
	// config reload with the value already in effect, and auditing those
	// no-ops would bury the actual operator changes.
	if prevBackend != backend {
		m.audit(AuditAgentBackendSet, name, auditFields(
			"outcome", "success",
			"backend", backend,
			"model", agent.effectiveModel(),
			"previous_backend", prevBackend,
		))
	}

	if m.routableBackend(backend) && m.inferenceRouteCallback != nil {
		model := agent.ModelOverride
		if model == "" {
			model = agent.Config.Model
		}
		m.inferenceRouteCallback(name, backend, model)
	} else if !IsInferenceBackend(backend) && m.clearInferenceRouteCallback != nil {
		m.clearInferenceRouteCallback(name)
	}
	return nil
}

// RefreshInferenceRoutes re-fires the inference route callback for every
// agent whose effective backend matches, so endpoint or credential changes
// (e.g. a governor LiteLLM config save) take effect on live agents without
// a restart.
func (m *Manager) RefreshInferenceRoutes(backend string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inferenceRouteCallback == nil || !m.routableBackend(backend) {
		return
	}
	for name, agent := range m.agents {
		effective := agent.Config.Backend
		if agent.BackendOverride != "" {
			effective = agent.BackendOverride
		}
		if effective != backend {
			continue
		}
		model := agent.ModelOverride
		if model == "" {
			model = agent.Config.Model
		}
		m.inferenceRouteCallback(name, backend, model)
	}
}

// GetBufferOutput returns output from the ring buffer directly, bypassing
// the tmux pane capture. The ring buffer accumulates all output over time
// (up to 500 lines) while the pane capture only has visible lines.
func (m *Manager) GetBufferOutput(name string, lines int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}

	if agent.OutputBuffer != nil && agent.OutputBuffer.Count() > 0 {
		return agent.OutputBuffer.Last(lines), nil
	}

	if pane := agent.FilteredPaneLines(lines); len(pane) > 0 {
		return pane, nil
	}

	return nil, nil
}

func (m *Manager) GetOutput(name string, lines int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}

	if pane := agent.FilteredPaneLines(lines); len(pane) > 0 {
		return pane, nil
	}

	if agent.OutputBuffer != nil {
		return agent.OutputBuffer.Last(lines), nil
	}

	return nil, nil
}

// SessionMissing reports whether an agent the manager believes is RUNNING has
// no live tmux session — the zombie case, where in-memory state and reality
// have diverged.
//
// It is deliberately false for any agent that is not StateRunning: a paused,
// stopped or never-started agent legitimately has no session, and reporting
// those as missing would turn every deliberate pause into a fault.
//
// The session check must go through the agent's OWN tmux socket. Each agent
// runs under its own UID on its own socket (e.g. /tmp/tmux-2007/hive-scanner),
// so a query against the default socket answers "no server running" even when
// every session is alive — the exact false reading that has sent live
// diagnosis down the wrong path.
func (m *Manager) SessionMissing(name string) bool {
	m.mu.RLock()
	agent, ok := m.agents[name]
	if !ok || agent.State != StateRunning || agent.Paused {
		m.mu.RUnlock()
		return false
	}
	m.mu.RUnlock()
	// The exec runs outside the lock: it shells out to tmux, and holding a
	// manager lock across a subprocess is how the startup path has deadlocked
	// before.
	return !m.tmuxSessionExistsForAgent(agent)
}

func (m *Manager) TmuxSession(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return ""
	}
	return agent.tmuxSession
}

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/effects"
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
	Source    string    `json:"source,omitempty"`
}

const (
	outputBufferCapacity = 500
	kickHistoryCapacity  = 50
	tmuxCaptureLines     = 2000
	proxyListenPort      = 18443
	proxyCACertPath      = "/data/proxy-ca.pem"

	// Completion markers live on the same potentially slow NFS PVC as the
	// migrated trees. One read per second per waiting agent is responsive
	// without amplifying load during a rolling update.
	uidIsolationPollInterval = time.Second

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
	LastKickSource    string
	KickRefused       bool
	KickRefusalReason string
	LaunchedMode      AgentMode
	HasLaunched       bool
	baseConfig        config.AgentConfig
	agentSpecPrompt   string
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
	launching            bool
	startupLaunchQueued  bool
	startupKickInFlight  bool
	startupKickGen       int
	pendingStartupKick   string
	pendingStartupSource string
	BootstrapOverride    string // when set, replaces buildBootstrapPrompt output
	LastError            string // captured from bare copilot diagnostic launch
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
	// provider-auth failures observed in the CLI pane.
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
	// TaskMCPURL is the hub-served, read-only task context endpoint for
	// hub-launched agents. Empty keeps launches byte-for-byte unchanged.
	TaskMCPURL string
	// RepoPaused reports whether a repo carries an operator pause (#6203). It is
	// a live predicate rather than a snapshot list because ProjectContext is
	// built once at boot and a pause is taken mid-run — a repo frozen for a
	// release must drop out of the next agent's scope without a hive restart.
	// Nil means nothing is paused, which is every hive that does not use the
	// feature and every test that constructs a ProjectContext literal.
	RepoPaused func(repo string) bool
	// AgentRepos returns the repositories one agent serves (#6204), and
	// AgentPrimaryRepo the repo its prompts should treat as "the" repo. Both
	// are live functions rather than a snapshot map because ProjectContext is
	// built once at boot while a scope is edited mid-run — a specialist
	// re-scoped in the dashboard must reach the next agent launch without a
	// hive restart.
	//
	// Nil means no scoping: every agent gets Repos and PrimaryRepo, which is
	// what every hive did before this field and what every test that builds a
	// ProjectContext literal still gets.
	AgentRepos       func(agent string) []string
	AgentPrimaryRepo func(agent string) string
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
	// the output-capture goroutines, and taking m.mu there risks startup
	// re-entrancy deadlocks.
	thrashMu sync.Mutex
	thrash   map[string]*thrashState
	// statusSnapshots backs GetStatusFast. Own mutex, NEVER m.mu — its whole
	// purpose is to be readable while m.mu is held by a restart (#7417).
	statusSnapMu sync.RWMutex
	statusSnaps  map[string]*AgentProcess
	// consentWedges records consent-screen restarts for the heartbeat's
	// ConsentWedged signal (#5577). Own mutex, NEVER m.mu — the recording
	// sites can run with m.mu held. Zero value ready.
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
	// sandboxJobLauncherFactory builds the Kubernetes Job launcher for an
	// agent on sandbox.runtime: job (#6311). Nil means the real in-cluster
	// launcher; tests inject a fake through setSandboxJobLauncherFactoryForTest.
	sandboxJobLauncherFactory func(config.SandboxJobConfig) sandbox.Launcher
	sandboxRunner             sandboxCommandRunner
	sandboxPushMinter         pushbroker.TokenMinter
	sandboxPRClient           PRCreator
	sandboxMutation           effects.Boundary
	sandboxAuditCallback      atomic.Pointer[func(agent, action, detail string)]

	// terminal is the nil-safe pane/tmux boundary. Nil means use the real tmux
	// implementation, preserving zero-value Manager behavior in tests.
	terminal             TerminalSession
	promptDismissTimeout time.Duration

	// Per-kick durable log archiving (#4296, #4295) — see kick_logs.go.
	// kickLogDir/kickLogRetention/kickLogMaxBytes are resolved once in
	// NewManager from env overrides; capture/clear seams live on terminal.
	kickLogDir       string
	kickLogRetention int
	kickLogMaxBytes  int64
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
		cfg = withTaskMCPConnection(cfg, project.TaskMCPURL)
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
			Name:       name,
			ID:         agentID,
			Config:     cfg,
			baseConfig: cfg,
			State:      StateStopped,
			UID:        agentUID,
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
	// PHASE 1 — brief critical section: map lookup, BYO-agent spec application,
	// the pure in-memory decisions (running/sandbox), and claiming the per-agent
	// launch guard. AgentSpec file I/O stays here so the effective config is
	// visible before sandbox and tmux preparation, and so Config/BootstrapOverride
	// writes stay under the same lock as every status reader.
	m.mu.Lock()

	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent %s not found", name)
	}
	if !AgentAvailableAtACMMLevel(name, m.project.ACMMLevel) {
		agent.startupLaunchQueued = false
		m.mu.Unlock()
		return fmt.Errorf("agent %s is not available below ACMM L5", name)
	}

	if agent.State == StateRunning {
		agent.startupLaunchQueued = false
		m.mu.Unlock()
		return fmt.Errorf("agent %s already running", name)
	}
	if agent.launching {
		m.mu.Unlock()
		return fmt.Errorf("agent %s launch already in progress", name)
	}

	if err := m.applyAgentSpec(agent); err != nil {
		m.audit(AuditAgentStartFailed, name, auditFields(
			"outcome", "failure",
			"backend", agent.effectiveBackend(),
			"model", agent.effectiveModel(),
			"error", err.Error(),
			"stage", "agent_spec",
		))
		agent.startupLaunchQueued = false
		m.mu.Unlock()
		return err
	}

	if m.agentSandboxEnabledLocked(agent) {
		// Sandbox agents never launch a CLI here — this branch only sets
		// in-memory state and does no I/O, so it completes entirely inside the
		// Phase-1 lock and never claims the launch guard.
		if agent.Paused {
			agent.State = StatePaused
			agent.startupLaunchQueued = false
			m.logger.Info("sandbox agent starting paused", "name", agent.Name, "trigger", agent.PausedTrigger, "persisted", agent.Config.Paused)
			m.mu.Unlock()
			return nil
		}
		now := time.Now()
		agent.State = StateIdle
		agent.StartedAt = &now
		agent.HasLaunched = true
		agent.LaunchedMode = m.agentMode(agent)
		agent.startupLaunchQueued = false
		m.logger.Info("audit: sandbox agent ready", "name", name)
		m.mu.Unlock()
		return nil
	}

	// Serialize concurrent Start(sameName). Once we release m.mu for the
	// out-of-lock launch below, this guard is the only thing preventing a
	// second Start from racing this one's tmux launch and guarded-field writes
	// (m.mu no longer covers the whole method). Refuse the second caller fast.
	agent.launching = true
	m.mu.Unlock()

	// Clear the guard on EVERY exit from here down (error, park-and-return,
	// success, or panic). Phase 1's returns above happen before the guard is
	// set, so they neither set nor need to clear it.
	defer func() {
		m.mu.Lock()
		agent.launching = false
		agent.startupLaunchQueued = false
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
	if err := m.awaitUIDIsolation(ctx, agent); err != nil {
		return err
	}

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
	m.bobPreflightUnlocked(agent)

	// PHASE 3 — launchInTmux. It was written to be called WITH m.mu held: it
	// mutates m.mu-guarded AgentProcess fields (State, StartedAt, HasLaunched,
	// LaunchedMode, LastKick/LastKickMessage/KickHistory, LastError, cancel,
	// launchGen, forceRelaunch, awaitingBobKey, ...) directly with no internal
	// locking. Re-acquire m.mu for the duration so those writes stay race-free
	// against AllStatuses()/snapshot() and the model/backend/pause setters —
	// preserving its original contract exactly (the function is unchanged).
	//
	// The remaining subprocess sends below are the actual tmux launch boundary;
	// Bob's shared-home/file probes have already run lock-free above.
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

// kickOutcomePollEvery throttles the post-kick turn-ended check to one visible
// pane capture per this many 3s poll ticks (15s), so a long turn does not
// cost an extra tmux exec every tick.
const kickOutcomePollEvery = 5

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
		// The boot-time migration walk has already finished; publish this
		// agent's completion marker now or awaitUIDIsolation holds its launch
		// forever. One tiny same-PVC write on a rare admin action — not the
		// hot-path NFS I/O the m.mu discipline guards against.
		m.publishRuntimeUIDIsolationMarker(name)
	}
	m.agents[name] = &AgentProcess{
		Name:         name,
		ID:           agentID,
		Config:       cfg,
		baseConfig:   cfg,
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

	agent.clearAgentSpecPromptIfSpecRemoved(cfg)
	agent.Config = cfg
	agent.baseConfig = cfg
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
			existing.clearAgentSpecPromptIfSpecRemoved(cfg)
			existing.Config = cfg
			existing.baseConfig = cfg
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
			// Same contract as AddAgent: a reconcile-added agent gets its
			// marker now, or its launch holds forever (the live adjudicator
			// wedge came through exactly this path).
			m.publishRuntimeUIDIsolationMarker(name)
		}
		m.agents[name] = &AgentProcess{
			Name:         name,
			ID:           agentID,
			Config:       cfg,
			baseConfig:   cfg,
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

func (m *Manager) markProviderErrorLocked(agent *AgentProcess, match providerErrorMatch, now time.Time) time.Duration {
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
	agent.clearBackendAuthLocked(now)
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

// ActiveRepos is Repos minus the operator-paused entries: the repos an agent
// should be told to work.
//
// PrimaryRepo deliberately does NOT filter. The primary repo is the hive's
// identity — what $HIVE_REPO names and what an agent's workdir is a checkout
// of — and re-pointing it at a different repository because the operator
// froze the first one would be a far larger surprise than the pause itself.
// A paused primary simply stops appearing in the work scope.
func (p ProjectContext) ActiveRepos() []string {
	return p.unpaused(p.Repos)
}

// ReposFor returns the repositories the named agent serves, falling back to the
// hive-wide list when the agent is unscoped or no scope function is wired.
func (p ProjectContext) ReposFor(agent string) []string {
	if p.AgentRepos == nil {
		return p.Repos
	}
	if repos := p.AgentRepos(agent); repos != nil {
		return repos
	}
	return p.Repos
}

// ActiveReposFor is ReposFor minus the operator-paused entries: the repos the
// named agent should actually be handed work on.
//
// The two narrowings compose rather than override. A scope says which repos an
// agent is FOR (#6204); a pause says which repos are open for work at all
// (#6203). An agent scoped to a repo the operator has since paused is handed
// nothing for it, and unpausing restores it without touching the scope.
func (p ProjectContext) ActiveReposFor(agent string) []string {
	return p.unpaused(p.ReposFor(agent))
}

// unpaused drops the operator-paused entries from repos, preserving order.
func (p ProjectContext) unpaused(repos []string) []string {
	if p.RepoPaused == nil {
		return repos
	}
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		if p.RepoPaused(r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// PrimaryRepoFor returns the repo the named agent's prompts should treat as
// "the" repo — what $HIVE_REPO names.
//
// Unlike PrimaryRepo() this is per-agent, and it has to be: the shipped
// templates say `gh issue create --repo "$HIVE_REPO"`, so handing a specialist
// scoped away from the hive primary the hive primary would aim every example it
// has at a repository the proxy then refuses. An unscoped agent is unaffected.
// Like PrimaryRepo it does not filter paused repos — see ActiveRepos.
func (p ProjectContext) PrimaryRepoFor(agent string) string {
	if p.AgentPrimaryRepo != nil {
		if repo := strings.TrimPrefix(strings.TrimSpace(p.AgentPrimaryRepo(agent)), p.Org+"/"); repo != "" {
			return repo
		}
	}
	return p.PrimaryRepo()
}

func (m *Manager) bobPreflightUnlocked(agent *AgentProcess) {
	if agent == nil {
		return
	}
	backend := agent.Config.Backend
	if agent.BackendOverride != "" {
		backend = agent.BackendOverride
	}
	if backend != bobBackend || m.bobAPIKey() == "" {
		return
	}
	// Bob shared-home repair and readability probes touch /data/home and run
	// subprocesses. Keep them in unlocked prep phases so a slow PVC or UID
	// probe cannot block AllStatuses()/heartbeat while boot launches stagger
	// across the agent fleet.
	m.ensureBobAuthSettings(agent.Name, bobSharedHome)
	m.verifyBobKeyReadable(agent.Name, m.bobKeyFilePath(), agent.UID)
	_ = m.verifyBobStateDirsWritable(agent.Name, bobSharedHome, m.workDir+"/"+agent.Name, agent.UID)
}

package agent

import (
	"time"
)

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
	// rateLimitRestartCooldownSec paces relaunches of an agent parked on the
	// CLI's rate-limited credential-validation banner. GitHub's core REST
	// window is an hour, and every relaunch inside it burns one more request
	// from the same exhausted budget, so this is deliberately far slower than
	// tokenRestartCooldownSec and uncapped: it stops firing the moment a
	// relaunch validates.
	rateLimitRestartCooldownSec = 300
	// rateLimitRelaunchFleetSpacing is the minimum gap between rate-limit
	// relaunches ACROSS agents. The per-agent cooldown alone re-creates the
	// burst that was limited: every agent that saw the banner during the
	// boot stagger comes due 300s later in the same tight sequence and all
	// of them validate against the one exhausted budget again (observed
	// 2026-10-20 on hosted-available-oke-11: 22 relaunches across 11 agents
	// after each roll). Spacing the fleet out means one validation per
	// window, so a budget that has recovered admits the first and the rest
	// follow one by one instead of re-tripping the limit together.
	rateLimitRelaunchFleetSpacing = 30 * time.Second
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
	// tokenRestartWindow / tokenRestartWindowMax bound token-triggered restarts
	// ACROSS cap re-arms. The per-boot cap above is re-armed whenever the
	// pane shows a CLI marker past the boot grace (shouldResetTokenRestartCap),
	// and a Copilot CLI whose credential is present but REJECTED satisfies
	// that on every boot: it paints a clean ready prompt, only the first kick
	// surfaces "Authorization error … Run /login", the pane flips back to the
	// login screen and the heal fires again as attempt 1 of 3 — observed on
	// six hosted hives at 5–17 restarts/24h, every one logged as attempt 1
	// (hivecommons/hive#12466). The rolling window is the hub's own
	// restart-storm window and the budget sits below its 5/24h storm bar
	// (pkg/hub/spoke.DefaultAgentRestartProblemThreshold), so a login the
	// restart cannot fix can never be reported as a storm by itself. Cleared
	// only by an operator reset/resume, exactly like the per-boot cap.
	tokenRestartWindow    = 24 * time.Hour
	tokenRestartWindowMax = tokenRestartMaxAttempts
	// tokenRestartKickGrace suppresses token-triggered restarts after a kick
	// delivery so the restart can never destroy just-delivered work.
	tokenRestartKickGrace = 10 * time.Minute
	// tokenRestartQuietWindow suppresses token-triggered restarts while the
	// pane is still changing. In the 2026-10-01 hosted-available-oke-11
	// incident, an agent actively investigating a GitHub-login bug mentioned
	// login prompts in its own output and was repeatedly killed mid-turn.
	tokenRestartQuietWindow    = 2 * time.Minute
	expiredTokenHangTimeoutSec = 180 // blank pane after this many seconds triggers token purge + restart
	tlsErrorRestartCooldownSec = 120 // minimum seconds between TLS-error-triggered restarts per agent

	// fatalNetworkProducingGraceSec is how recently the pane must have
	// changed for an agent to count as "still producing", which vetoes the
	// fatal-network restart. It is deliberately a few poll intervals rather
	// than one: the poll runs every ~3s and a CLI mid-answer can pause
	// longer than a single interval between rendered chunks, so a tighter
	// window would call a working agent dead on an ordinary stall.
	fatalNetworkProducingGraceSec = 15
)

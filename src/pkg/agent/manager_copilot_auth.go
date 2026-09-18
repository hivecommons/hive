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

	// fatalNetworkProducingGraceSec is how recently the pane must have
	// changed for an agent to count as "still producing", which vetoes the
	// fatal-network restart. It is deliberately a few poll intervals rather
	// than one: the poll runs every ~3s and a CLI mid-answer can pause
	// longer than a single interval between rendered chunks, so a tighter
	// window would call a working agent dead on an ordinary stall.
	fatalNetworkProducingGraceSec = 15
)

- Stopped the copilot hang detector from destroying kicks while they are being
  typed. Delivering a kick is not instantaneous — `deliverKickLocked` types the
  message as 400-rune chunks, so a 37KB governor kick occupies the pane for
  roughly 100 seconds, during which the CLI's idle chrome is scrolled out of
  the captured pane. The "copilot hung with no CLI prompt" detector keys on
  precisely that absence, so it read a healthy mid-delivery agent as dead: on
  the scanner agent it fired 68 seconds into a kick, recreated the tmux
  session, and the remaining chunks landed nowhere — eleven consecutive
  `tmux send-keys failed` lines, then a restart, then `audit: agent kicked`
  logged as if the 37,564-character prompt had arrived. It had not, and the
  agent sat at `Session: 0 AIC used` doing nothing until the next cadence.
  `interruptions_total` had reached 5, so five consecutive governor kicks were
  eaten by the detector meant to rescue the agent. Agents now carry a
  `kickDelivering` flag held for the duration of delivery, and the three pane
  actions that would corrupt or destroy an in-flight kick — the hang
  diagnostic, the fatal-TLS restart, and the transient-error retry nudge, which
  types into the pane — all stand down while it is set. The login-triggered
  restart already had an equivalent kick grace; this closes the remaining
  paths.

- The contributor relay now scans deep pane scrollback for the PR URL an agent
  opened, instead of only the 15-line window it sends upstream as `tmux_output`.
  About ten of those fifteen terminal rows are TUI chrome, so a genuine PR link
  scrolled out of view within seconds and a task that shipped a PR was reported
  as shipping nothing — costing it the correct issue cooldown ([#6667]).

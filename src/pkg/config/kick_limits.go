package config

// Bounds an operator may set a cap to. The ceiling matters: without it a
// setting of `max_prs: 100000` is honoured verbatim and reproduces the very
// unbounded prompt this block exists to prevent, through the control meant to
// prevent it. 500 is far above any useful backlog slice for a single turn
// while still keeping the fully-expanded list inside a sane prompt size.
const (
	MinKickListCap = 1
)

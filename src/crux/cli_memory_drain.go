package crux

// CLI memory drain (hivecommons/hive#17033). The runtime hands the host the
// per-agent CLI memory stores before a kick wipes them; the host converts
// them into draft knowledge entries. Defined here, like the other Host
// capabilities, so cmd/hive can install the hook without importing
// crux/agent; crux/agent aliases these types.

// CLIMemorySource is one memory store to drain: the CLI that wrote it
// ("claude", "codex", "gemini") and the file or directory it lives in.
type CLIMemorySource struct {
	CLI  string
	Path string
}

// CLIMemoryDrainRequest names the agent whose stores are being drained.
// Truncate asks the drainer to empty, after a drain that reported no error,
// every source file whose facts are all recorded in the store; the kick path
// leaves it false because clear_on_kick owns the wipe there.
type CLIMemoryDrainRequest struct {
	Agent    string
	Backend  string
	Sources  []CLIMemorySource
	Truncate bool
}

// CLIMemoryDrainResult is what the drainer reports back for the runtime's
// one log line per drain. Spooled counts memory files kept in the hive's
// spool because not all of their facts were stored; they are retried on the
// agent's next drain, so the wipe that follows the drain does not lose them.
type CLIMemoryDrainResult struct {
	// Store is the knowledge store (vault path or name) the drafts landed in.
	Store   string
	Drained int
	Dupes   int
	Skipped int
	Spooled int
	// Truncated and Kept count the files emptied, and the files left as
	// they were because a fact is missing from the store, on a Truncate
	// request.
	Truncated int
	Kept      int
	Err       error
}

// CLIMemoryDrainer converts the stores in req into draft knowledge entries.
// It runs synchronously on the kick path, before the memory wipe, so it
// must be bounded.
type CLIMemoryDrainer func(req CLIMemoryDrainRequest) CLIMemoryDrainResult

// CLIMemoryDrainHost is the optional Host capability the in-process runtime
// implements. It is not part of Host: a sidecar runtime cannot call a Go
// closure synchronously on its kick path, so the hive installs the drainer
// only when the runtime it built satisfies this interface.
type CLIMemoryDrainHost interface {
	SetCLIMemoryDrainer(fn CLIMemoryDrainer)
}

// CLIMemoryDrainNowRequest asks the runtime to drain one agent's memory now
// rather than at its next kick (hivecommons/hive#17208): the entry point an
// agent-UID Claude hook reaches through the hive. Agent is the name the
// caller claims and CallerUID the UID that proved the call (the owner of the
// request file); the runtime refuses a name that UID does not belong to.
// Cwd, when set, narrows the drain to the Claude project directory Claude
// derives from it. The sources are always resolved from the agent's own
// home, never taken from the caller. OnWrite marks a request from the
// PostToolUse hook, which runs only for agents with
// agents.cli_memory.drain_on_write (hivecommons/hive#17210).
type CLIMemoryDrainNowRequest struct {
	Agent     string
	CallerUID int
	Cwd       string
	Truncate  bool
	OnWrite   bool
}

// CLIMemoryDrainNowHost is the optional capability behind that entry point.
// Ran is false, with a nil error, when the drain is off for the agent
// (agents.cli_memory.drain_to_knowledge) or it has no private store to
// drain: the call is a no-op, not a failure.
type CLIMemoryDrainNowHost interface {
	DrainCLIMemoryNow(req CLIMemoryDrainNowRequest) (res CLIMemoryDrainResult, ran bool, err error)
}

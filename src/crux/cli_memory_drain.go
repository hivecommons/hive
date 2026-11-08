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
type CLIMemoryDrainRequest struct {
	Agent   string
	Backend string
	Sources []CLIMemorySource
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
	Err     error
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

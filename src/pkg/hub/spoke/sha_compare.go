package spoke

// StandardSHALen is the canonical short-SHA length used across hive: the hub
// stores branch SHAs as Commit.SHA[:StandardSHALen], spoke and hub binaries
// build gitShort with `git rev-parse --short=7`, and the hub normalizes any
// SHA a spoke reports to this length on ingest.
const StandardSHALen = 7

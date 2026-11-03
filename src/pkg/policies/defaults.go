package policies

import "embed"

//go:embed defaults
var DefaultPolicies embed.FS

// UserOverrideDir is where the dashboard prompt editor writes a template when
// the shipped/seeded copy in /data/policies cannot be written in place (a
// read-only mount or a file owned by another uid on a hosted hive,
// hivecommons/hive#13005). Kick-time template resolution probes it before
// /data/policies so a saved override always wins over the shipped default.
const UserOverrideDir = "/data/policy-overrides"

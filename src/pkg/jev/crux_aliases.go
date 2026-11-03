package jev

import cruxjev "github.com/hivecommons/hive/crux/jev"

// The environment contract and the jev-decide skill live in the Crux ring
// (pkg/crux/jev) because the agent runtime exports and installs them; this
// package keeps the names so the server and CLI read the same constants.
const (
	ModeEnvVar      = cruxjev.ModeEnvVar
	EndpointEnvVar  = cruxjev.EndpointEnvVar
	DefaultEndpoint = cruxjev.DefaultEndpoint
	SkillName       = cruxjev.SkillName
	SkillFile       = cruxjev.SkillFile
)

var (
	SkillMarkdown = cruxjev.SkillMarkdown
	SkillRelDir   = cruxjev.SkillRelDir
)

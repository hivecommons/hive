package github

type issueLabelDefinition struct {
	color       string
	description string
}

var escalationIssueLabelDefinitions = map[string]issueLabelDefinition{
	"needs-direction": {
		color:       "d4c5f9",
		description: "Hive needs a maintainer direction decision before continuing",
	},
	"needs-spec": {
		color:       "bfd4f2",
		description: "Hive needs a specification or acceptance criteria before continuing",
	},
	"needs-signal": {
		color:       "fbca04",
		description: "Hive needs a better CI or test signal before continuing",
	},
	"meta": {
		color:       "5319e7",
		description: "Tracker for multiple open items sharing one root cause",
	},
}

func escalationIssueLabelDefinition(name string) (issueLabelDefinition, bool) {
	def, ok := escalationIssueLabelDefinitions[name]
	return def, ok
}

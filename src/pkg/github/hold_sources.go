package github

// Hold sources name why a held issue or pull request is held. A hold can have
// several sources at once; the dashboard's your-turn decision maps the set to
// ownership (hivecommons/hive#11507).
const (
	HoldSourceLevelGate         = "level_gate"
	HoldSourceOutreach          = "outreach"
	HoldSourceSelfAuthorization = "self_authorization"
	HoldSourceReporterTrust     = "reporter_trust"
	HoldSourceSHAHold           = "sha_hold"
	HoldSourceHoldguard         = "holdguard"
	HoldSourceHumanMergePath    = "human_merge_path"
	HoldSourceStandbyDonation   = "standby_donation"
	HoldSourceDashboardPause    = "dashboard_pause"
	HoldSourcePerson            = "person"
	HoldSourceUnknown           = "unknown"
)

// HoldSources lists every valid hold source, in the order the spec names them.
var HoldSources = []string{
	HoldSourceLevelGate,
	HoldSourceOutreach,
	HoldSourceSelfAuthorization,
	HoldSourceReporterTrust,
	HoldSourceSHAHold,
	HoldSourceHoldguard,
	HoldSourceHumanMergePath,
	HoldSourceStandbyDonation,
	HoldSourceDashboardPause,
	HoldSourcePerson,
	HoldSourceUnknown,
}

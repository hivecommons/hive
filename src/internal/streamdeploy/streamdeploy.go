// Package streamdeploy holds the CI checks for the Twitch streamer manifests
// under src/deploy/stream (hivecommons/hive#15966). That directory is not a Go
// package the test matrix reaches, so its config contracts — the existing
// twitch.tv/hivecommons stream left byte-for-byte unchanged, the independent
// hivecommonsfleet deployment disabled by default, and the public-safe gate
// failing closed — are pinned from here.
package streamdeploy

// StreamDir is src/deploy/stream relative to this package's directory, which
// is where `go test` runs.
const StreamDir = "../../deploy/stream"

// PublicSafePolicyVersion is the acknowledgement the owner writes into
// PUBLIC_SAFE_POLICY_ACK to enable the hivecommonsfleet broadcast. It must
// match PUBLIC_SAFE_POLICY_VERSION in public-safe-gate.sh and the policy
// section of src/docs/streaming-the-activity-monitor.md.
const PublicSafePolicyVersion = "hivecommonsfleet-public-safe-v1"

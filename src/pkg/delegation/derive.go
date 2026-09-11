package delegation

import "github.com/hivecommons/hive/pkg/keyderive"

// deriveDomainKey returns a domain-separated sub-key of master for the given
// info label, as lowercase hex.
//
// BYTE-IDENTICAL to pkg/hub's deriveDomainKey, and that is a hard requirement
// rather than a coincidence: the hub derives this package's signing seed with
// ITS copy when it publishes verification material, and this package derives it
// here when it mints. If the two ever disagreed, the hub would publish a public
// key that verifies nothing — a failure that shows up only as "every chain is
// unverifiable", with no error anywhere near the cause.
//
// THIN WRAPPER OVER pkg/keyderive, deliberately, rather than importing pkg/hub
// directly. pkg/hub is a very large package that imports most of the tree;
// pkg/delegation is imported BY the emit sites (dashboard, agent) and must stay
// near-leaf, so importing hub would create a cycle and drag the whole hub into
// every consumer. pkg/keyderive is a stdlib-only leaf with no such risk, so both
// this wrapper and hub's now call the SAME implementation instead of each
// keeping its own copy — the duplication #6643 removed.
// TestDeriveDomainKeyMatchesHubDerivation still pins the two wrappers against
// each other so a future local reimplementation cannot silently drift back in.
//
// Returns "" for an empty master so callers keep the fail-closed contract every
// other derivation site in hive relies on: no secret configured means no key,
// which means MintToken returns "" and no chain is emitted.
func deriveDomainKey(master, info string) string {
	return keyderive.DomainKey(master, info)
}

package hub

// Version-absent detection: a hive that heartbeats but reports NO git_hash is
// invisible to the upgrade path AND to the operator.
//
// The hub decides whether to instruct an upgrade by comparing the spoke's
// reported git_hash against the branch target. With an empty git_hash that
// comparison cannot run (see the `payload.GitHash != ""` gate on the
// spoke-managed upgrade path), so the hub issues no instruction at all - and
// the hive silently stops receiving upgrades while continuing to count as
// online, because beats keep arriving. The registry row keeps whatever version
// the last GOOD report left behind, so every operator-facing surface renders
// the condition as health.
//
// Two hives sat in exactly this state for hours (#6025):
//
//   - hosted-llm-d-llm-d-worklo-wva1: pod 1/1 Running, hub logging its
//     heartbeat every 120s, while the spoke's own logs showed 104 "proxy
//     client request read timed out", 34 "hub heartbeat collect timed out",
//     and 0 successful heartbeat collections. It sent last-good cached stats
//     with no version, ran digest 14c75aa0 while :stable was ef5a603a, and
//     received no upgrade instruction.
//   - hosted-available-vllmd-01: showed "No heartbeat for 8h29m" and
//     "Upgrading 29m" simultaneously, both stale, because the hub had no fresh
//     version to update the row with.
//
// This is the same family as status_flip.go: a hub-observed fact about beats
// that needs NOTHING new from the spoke (an old spoke cannot silence it), and
// whose message is that the row cannot be trusted. It differs in what it adds:
// a flipping row is untrustworthy, a version-less row is untrustworthy AND
// frozen, because the upgrade instruction that would move it is never sent.

// versionAbsentBeatsToConfirm is how many CONSECUTIVE version-less heartbeats
// are required before the hive is declared version-absent.
//
// One beat is not evidence. A spoke legitimately omits git_hash during
// startup, and a single collect timeout is a blip the next beat clears. A
// spoke beats roughly every 120s, so three consecutive empty beats is about
// six minutes of the hub having no version to compare - long past any
// start-up window, and short enough to have surfaced both hives above within
// minutes instead of hours.
const versionAbsentBeatsToConfirm = 3

// versionAbsentState is the per-hive run length of version-less beats. Kept
// beside the other per-beat trackers (reporterSeen, statusFlipSeen) rather
// than in the registry: it is beat bookkeeping, not hive state, and it must
// never contend with the registry lock on the hottest write path.
type versionAbsentState struct {
	// consecutive counts beats in a row that carried no git_hash. Any beat
	// WITH a version resets it to zero - a hive that recovers stops being
	// reported the moment it proves it can report again.
	consecutive int
}

// noteVersionAbsent records whether this beat carried a git_hash and returns
// true once versionAbsentBeatsToConfirm consecutive beats have carried none.
//
// gitHash is the already-sanitized, already-shortened value the registry entry
// will store, so this predicate is asking exactly the question the upgrade
// comparison asks: is there a version here to compare?
func (s *HubServer) noteVersionAbsent(hiveID, gitHash string) bool {
	s.versionAbsentMu.Lock()
	defer s.versionAbsentMu.Unlock()
	if s.versionAbsentSeen == nil {
		s.versionAbsentSeen = make(map[string]*versionAbsentState)
	}
	st := s.versionAbsentSeen[hiveID]
	if st == nil {
		st = &versionAbsentState{}
		s.versionAbsentSeen[hiveID] = st
	}
	if gitHash != "" {
		// A reported version ends the run outright. Not a decrement: the
		// signal is about an UNBROKEN streak of blindness, and one good beat
		// means the hub can compare again.
		st.consecutive = 0
		return false
	}
	// Saturate rather than counting forever, so a hive that stays mute for
	// weeks cannot overflow the counter.
	if st.consecutive < versionAbsentBeatsToConfirm {
		st.consecutive++
	}
	return st.consecutive >= versionAbsentBeatsToConfirm
}

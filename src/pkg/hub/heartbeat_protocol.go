package hub

// Heartbeat feature negotiation, hub registration protocol rollout step 5
// (hivecommons/hive#18642, #19056, #19237).
//
// A spoke may send protocol: {version, features} in its heartbeat. While
// HIVE_HUB_HEARTBEAT_PROTOCOL is on, the hub answers with protocol:
// {version, accepted, ignored}: the protocol version it speaks for this beat,
// the requested features it honours, and the requested features it does not.
//
// Invariants:
//   - Additive. A heartbeat without protocol, or any heartbeat while the flag
//     is off, gets exactly the response it got before: no protocol field.
//   - Informational only. Negotiation changes nothing else in the response;
//     an accepted feature is one the hub already serves to every spoke.
//   - A version the hub cannot speak (below 1) is answered, not refused: the
//     beat is still accepted, every requested feature is ignored, and
//     unsupported_version echoes what the spoke sent.
//   - A newer version (above the hub's) is answered with the hub's version, so
//     a newer spoke can fall back to it.
//   - Only well-formed feature names are echoed back, at most
//     heartbeatProtocolMaxFeatures of them, deduplicated and sorted.

import (
	"os"
	"sort"
	"strings"
)

const (
	heartbeatProtocolEnv = "HIVE_HUB_HEARTBEAT_PROTOCOL"

	// heartbeatProtocolVersion is the newest heartbeat protocol version this
	// hub speaks.
	heartbeatProtocolVersion = 1

	// heartbeatProtocolMaxFeatures bounds how many requested features one
	// beat may have evaluated and echoed back.
	heartbeatProtocolMaxFeatures = 32
	// heartbeatProtocolMaxFeatureLen bounds one feature name.
	heartbeatProtocolMaxFeatureLen = 64

	HeartbeatFeatureActivityDigest = "activity_digest"
	HeartbeatFeatureActivityEvents = "activity_events"
	HeartbeatFeatureHostAliases    = "host_aliases"
)

// heartbeatProtocolFeatures are the features this hub accepts at
// heartbeatProtocolVersion. Each names behaviour the hub already serves.
var heartbeatProtocolFeatures = map[string]bool{
	HeartbeatFeatureActivityDigest: true, // activity digest and activity_etag (#15951)
	HeartbeatFeatureActivityEvents: true, // activity events and activity_events_ack (#15964)
	HeartbeatFeatureHostAliases:    true, // host_aliases on every beat (#18637)
}

// HeartbeatProtocol is the spoke's optional protocol offer in a heartbeat.
type HeartbeatProtocol struct {
	Version  int      `json:"version"`
	Features []string `json:"features,omitempty"`
}

// HeartbeatProtocolAck is the hub's answer to a HeartbeatProtocol offer.
// Accepted and Ignored are always present (possibly empty) so a spoke can
// tell "nothing accepted" from an older hub that sent no answer at all.
type HeartbeatProtocolAck struct {
	Version            int      `json:"version"`
	Accepted           []string `json:"accepted"`
	Ignored            []string `json:"ignored"`
	UnsupportedVersion *int     `json:"unsupported_version,omitempty"`
}

// heartbeatProtocolEnabled reads the flag per request, like the other hub
// toggles, so it can be flipped without a restart.
func heartbeatProtocolEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(heartbeatProtocolEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// heartbeatProtocolAckFor returns the protocol answer for one heartbeat, or nil
// when the flag is off or the spoke made no offer (the response is then
// unchanged from before negotiation existed).
func heartbeatProtocolAckFor(offer *HeartbeatProtocol) *HeartbeatProtocolAck {
	if offer == nil || !heartbeatProtocolEnabled() {
		return nil
	}
	return negotiateHeartbeatProtocol(*offer)
}

func negotiateHeartbeatProtocol(offer HeartbeatProtocol) *HeartbeatProtocolAck {
	ack := &HeartbeatProtocolAck{
		Version:  heartbeatProtocolVersion,
		Accepted: []string{},
		Ignored:  []string{},
	}
	supported := offer.Version >= 1
	if !supported {
		v := offer.Version
		ack.UnsupportedVersion = &v
	}
	seen := map[string]bool{}
	for _, raw := range offer.Features {
		if len(seen) >= heartbeatProtocolMaxFeatures {
			break
		}
		name := strings.TrimSpace(raw)
		if !validHeartbeatFeatureName(name) || seen[name] {
			continue
		}
		seen[name] = true
		if supported && heartbeatProtocolFeatures[name] {
			ack.Accepted = append(ack.Accepted, name)
		} else {
			ack.Ignored = append(ack.Ignored, name)
		}
	}
	sort.Strings(ack.Accepted)
	sort.Strings(ack.Ignored)
	return ack
}

// validHeartbeatFeatureName accepts lowercase letters, digits, '_', '-' and
// '.', starting with a letter, up to heartbeatProtocolMaxFeatureLen bytes.
func validHeartbeatFeatureName(name string) bool {
	if name == "" || len(name) > heartbeatProtocolMaxFeatureLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}

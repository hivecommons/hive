package hub

import (
	"reflect"
	"strings"
)

// Credential fence for self-service (self-hosted, owner-registered) hives
// (#18060).
//
// A self-service hive runs on its owner's infrastructure and is listed on the
// hub as directory metadata only (#12571). The hub holds no authority over its
// policy, credentials or upgrades, and every identity field such a spoke puts
// in its heartbeat payload (cluster, namespace, App installation) is reported
// by the spoke itself and cannot be trusted. Several delivery paths in the
// heartbeat response builder resolve what to send from exactly those
// payload-reported fields: appKeySyncForHeartbeat, for example, falls back to
// the spoke-reported cluster_id for a hive with no SaaS record and would hand
// that cluster's App private key to any spoke that claims it.
//
// So instead of a check at each delivery site, ONE guard runs on every
// heartbeat response before it is signed and written
// (signHeartbeatResponse is the single point every response passes through).
// The guard is driven by heartbeatResponseFieldFence, which classifies every
// HeartbeatResponse field. TestHeartbeatResponseFieldFenceInventory fails when
// a field is added to HeartbeatResponse without a classification here, so a
// new credential or management field cannot reach a self-service hive by
// default.

// SelfServiceHiveIDPrefix is the reserved hive ID namespace for hives that
// owners register themselves (for example "byo-<github-login>-<name>"). Every
// hive ID in this namespace is fenced, whatever key it authenticated with.
const SelfServiceHiveIDPrefix = "byo-"

// IsSelfServiceHiveID reports whether hiveID is in the reserved self-service
// namespace.
func IsSelfServiceHiveID(hiveID string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(hiveID)), SelfServiceHiveIDPrefix)
}

// heartbeatFieldClass is how the credential fence treats one
// HeartbeatResponse field for a self-service hive.
type heartbeatFieldClass int

const (
	// heartbeatFieldAllowed is protocol, signature or display-only data that
	// carries no hub-held secret and no instruction to the spoke.
	heartbeatFieldAllowed heartbeatFieldClass = iota + 1
	// heartbeatFieldFenced is a hub-held credential, a management action, hub
	// policy over the hive, or data resolved from spoke-reported identity. It
	// is zeroed before the response leaves the hub.
	heartbeatFieldFenced
)

// heartbeatResponseFieldFence classifies every HeartbeatResponse field by its
// Go field name. Adding a field to HeartbeatResponse requires adding it here.
var heartbeatResponseFieldFence = map[string]heartbeatFieldClass{
	// Protocol and descriptive data.
	"OK":                heartbeatFieldAllowed,
	"HubGitHash":        heartbeatFieldAllowed,
	"LatestSHA":         heartbeatFieldAllowed,
	"LatestTag":         heartbeatFieldAllowed,
	"HubBanner":         heartbeatFieldAllowed, // admin message, display only
	"ActivityETag":      heartbeatFieldAllowed,
	"ActivityEventsAck": heartbeatFieldAllowed,
	// Response signature binding.
	"SigHiveID":   heartbeatFieldAllowed,
	"SigSeq":      heartbeatFieldAllowed,
	"SigSignedAt": heartbeatFieldAllowed,
	"SigVersion":  heartbeatFieldAllowed,

	// Hub-held credentials.
	"GitHubAppConfig": heartbeatFieldFenced, // App, cluster and secondary keys, webhook relay key
	"PendingGateway":  heartbeatFieldFenced, // funded gateway API key
	// Upgrade and lifecycle management.
	"UpgradeTo":        heartbeatFieldFenced,
	"UpgradeInterrupt": heartbeatFieldFenced,
	"SwitchToTag":      heartbeatFieldFenced,
	"RestartSpoke":     heartbeatFieldFenced,
	"UpgradeAnnounce":  heartbeatFieldFenced,
	"UpgradePolicy":    heartbeatFieldFenced, // the owner, not the hub, sets upgrade posture
	// Operational actions.
	"ResetAgentRestarts": heartbeatFieldFenced,
	"Breaker":            heartbeatFieldFenced,
	"SelfExperimentFlag": heartbeatFieldFenced,
	// Hub policy over the hive's config and access.
	"IsPublic":            heartbeatFieldFenced,
	"AuthorizedUsers":     heartbeatFieldFenced,
	"AuthorizedUserNames": heartbeatFieldFenced,
	"ProjectConfig":       heartbeatFieldFenced, // claimed project config
	// Resolved from spoke-reported or hub-provisioned identity.
	"ClusterID": heartbeatFieldFenced,
}

// fenceSelfServiceHeartbeatResponse zeroes every fenced field of resp when
// hiveID is a self-service hive, and returns the names of fields that held a
// value (never the values). Any field missing from heartbeatResponseFieldFence
// is treated as fenced, so an unclassified field fails closed even in a build
// that skipped the inventory test. For any other hive it does nothing.
func fenceSelfServiceHeartbeatResponse(resp *HeartbeatResponse, hiveID string) []string {
	if resp == nil || !IsSelfServiceHiveID(hiveID) {
		return nil
	}
	v := reflect.ValueOf(resp).Elem()
	t := v.Type()
	var withheld []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if heartbeatResponseFieldFence[f.Name] == heartbeatFieldAllowed {
			continue
		}
		fv := v.Field(i)
		if !fv.CanSet() || fv.IsZero() {
			// Unexported fields are never marshalled to the wire.
			continue
		}
		fv.Set(reflect.Zero(f.Type))
		withheld = append(withheld, f.Name)
	}
	return withheld
}

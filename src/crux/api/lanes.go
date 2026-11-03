package api

import "time"

const PathLanes = "/" + Version + "/lanes"

// Lane error codes retain the same meaning across HTTP handlers and clients.
const (
	CodeLaneUnavailable = "lane_unavailable"
	CodeLaneForbidden   = "lane_forbidden"
	CodeLaneInvalid     = "lane_invalid"
	CodeLaneConflict    = "lane_conflict"
	CodeLaneCursor      = "lane_cursor"
	CodeLaneStale       = "lane_stale"
	CodeLaneCapacity    = "lane_capacity"
	CodeLaneTimeout     = "lane_timeout"
	CodeLaneCanceled    = "lane_canceled"
)

// Lane IDs are plain subject-safe identifiers, not forge coordinates.
type Lane struct {
	ID          string              `json:"id"`
	Owner       string              `json:"owner"`
	Publishers  []string            `json:"publishers"`
	Subscribers []string            `json:"subscribers"`
	Groups      map[string][]string `json:"groups"`
	Recoverers  map[string][]string `json:"recoverers,omitempty"`
}

const (
	LaneEvent = "event"
	LaneWork  = "work"
)

// LanePublish deliberately has no caller-supplied publisher. Authentication
// supplies the project identity; provenance is optional, untrusted metadata.
type LanePublish struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Schema     string `json:"schema"`
	Provenance string `json:"provenance,omitempty"`
	Payload    []byte `json:"payload"`
}

type LaneEnvelope struct {
	LanePublish
	Lane      string    `json:"lane"`
	Project   string    `json:"project"`
	CreatedAt time.Time `json:"created_at"`
}

type LanePublished struct {
	Sequence  uint64 `json:"sequence"`
	Duplicate bool   `json:"duplicate"`
}

// A cursor is the last examined lane stream sequence, including work records
// skipped by event replay. Zero explicitly opts into the earliest retained data.
type LaneReplay struct {
	Events []LaneRecord `json:"events"`
	Cursor uint64       `json:"cursor"`
}

type LaneRecord struct {
	Sequence uint64       `json:"sequence"`
	Envelope LaneEnvelope `json:"envelope"`
}

type LaneClaim struct {
	LaneRecord
	Receipt string    `json:"receipt"`
	Expires time.Time `json:"expires"`
	Attempt uint64    `json:"attempt"`
}

type LaneReceipt struct {
	Sequence uint64 `json:"sequence"`
	Receipt  string `json:"receipt"`
}

type LaneRenewed struct {
	Expires time.Time `json:"expires"`
}

type LaneGroupState struct {
	Revision       uint64           `json:"revision"`
	Cursor         uint64           `json:"cursor"`
	FirstRetained  uint64           `json:"first_retained"`
	LastSequence   uint64           `json:"last_sequence"`
	Recoverable    []LaneReceipt    `json:"recoverable"`
	Recoveries     []LaneRecovered  `json:"recoveries"`
	Retries        []LaneRetry      `json:"retries"`
	MissingThrough uint64           `json:"missing_through"`
	Missing        []LaneExpiration `json:"missing"`
	Slots          int              `json:"slots"`
}

// LaneExpiration describes missing, lease-expired work without exposing its
// ownership receipt or representing successful execution.
type LaneExpiration struct {
	Sequence uint64 `json:"sequence"`
	Project  string `json:"project"`
	Attempt  uint64 `json:"attempt"`
}

type LaneRetry struct {
	Sequence    uint64    `json:"sequence"`
	Attempt     uint64    `json:"attempt"`
	AvailableAt time.Time `json:"available_at"`
}

// Recovery explicitly accepts an expired prefix and listed missing deliveries.
// Missing needs the configured group recovery capability, not a foreign receipt.
// Expired optionally supplies the caller's original receipts for extra fencing.
// Revision fences the entire delivery-group state.
type LaneRecover struct {
	ID            string        `json:"id"`
	Revision      uint64        `json:"revision"`
	CursorThrough uint64        `json:"cursor_through"`
	Expired       []LaneReceipt `json:"expired"`
	Missing       []uint64      `json:"missing"`
}

type LaneRecovered struct {
	ID             string           `json:"id"`
	Project        string           `json:"project"`
	Cursor         uint64           `json:"cursor"`
	GapFrom        uint64           `json:"gap_from,omitempty"`
	GapThrough     uint64           `json:"gap_through,omitempty"`
	MissingThrough uint64           `json:"missing_through"`
	Retired        []uint64         `json:"retired"`
	Expirations    []LaneExpiration `json:"expirations"`
	RecoveredAt    time.Time        `json:"recovered_at"`
}

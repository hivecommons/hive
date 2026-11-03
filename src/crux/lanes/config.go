// Package lanes owns forge-neutral durable events and work. Streams and all
// mutable delivery state live in JetStream; no process-local journal is used.
package lanes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

const (
	DefaultNamespace     = "crux"
	DefaultRetention     = 7 * 24 * time.Hour
	DefaultDedup         = 2 * time.Minute
	DefaultLease         = time.Minute
	DefaultAckRetention  = time.Hour
	DefaultRetryDelay    = time.Second
	DefaultMaxRetryDelay = time.Minute
	MinRetryDelay        = 100 * time.Millisecond
	MaxRetryDelay        = time.Hour
	DefaultTimeout       = 5 * time.Second
	DefaultMaxPayload    = 64 << 10
	DefaultMaxMessages   = 10000
	DefaultMaxBytes      = 64 << 20
	DefaultMaxKVBytes    = 64 << 20
	DefaultMaxLedger     = 1024
	DefaultMaxPending    = 128
	DefaultMaxLanes      = 64
	DefaultMaxGroups     = 64
	DefaultMaxMembers    = 64
	MaxLaneMessages      = 1000000
	MaxStorageBytes      = 1 << 30
	MaxLedgerEntries     = 10000
	MaxIDLength          = 64
	MaxProvenanceLength  = 2048
	MaxReplayBatch       = 128
	MaxRecoveryBatch     = 16
	MaxScanBatch         = 512
	MinLease             = time.Second
	MinDedup             = 100 * time.Millisecond
	MaxLease             = time.Hour
	MinRetention         = time.Second
	MaxRetention         = 30 * 24 * time.Hour
	MaxTimeout           = time.Minute
	MaxPayload           = 1 << 20
	MaxReplicas          = 5
)

var (
	ErrInvalid     = errors.New("invalid lane request")
	ErrForbidden   = errors.New("lane capability denied")
	ErrConflict    = errors.New("lane state conflict")
	ErrCursor      = errors.New("invalid or expired lane cursor")
	ErrStale       = errors.New("stale lane receipt")
	ErrCapacity    = errors.New("lane capacity exhausted")
	ErrEmpty       = errors.New("no claimable work")
	ErrUnavailable = errors.New("lane broker unavailable")
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	envPattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type Principal struct {
	Project  string `json:"project"`
	TokenEnv string `json:"token_env"`
}

// Config is administrator-owned configuration, never an API request. Secrets
// are referenced by environment variable name and never serialized into it.
type Config struct {
	URL            string        `json:"url"`
	CredentialsEnv string        `json:"credentials_env,omitempty"`
	Namespace      string        `json:"namespace"`
	Retention      time.Duration `json:"retention"`
	Dedup          time.Duration `json:"dedup"`
	Lease          time.Duration `json:"lease"`
	AckRetention   time.Duration `json:"ack_retention"`
	RetryDelay     time.Duration `json:"retry_delay"`
	MaxRetryDelay  time.Duration `json:"max_retry_delay"`
	Timeout        time.Duration `json:"timeout"`
	MaxPayload     int           `json:"max_payload"`
	MaxMessages    int64         `json:"max_messages"`
	MaxBytes       int64         `json:"max_bytes"`
	MaxKVBytes     int64         `json:"max_kv_bytes"`
	MaxLedger      int           `json:"max_ledger"`
	MaxPending     int           `json:"max_pending"`
	Replicas       int           `json:"replicas"`
	Principals     []Principal   `json:"principals"`
	Lanes          []api.Lane    `json:"lanes"`
}

func ValidID(id string) bool { return len(id) <= MaxIDLength && idPattern.MatchString(id) }

func (c *Config) Defaults() {
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	if c.Retention == 0 {
		c.Retention = DefaultRetention
	}
	if c.Dedup == 0 {
		c.Dedup = DefaultDedup
	}
	if c.Lease == 0 {
		c.Lease = DefaultLease
	}
	if c.AckRetention == 0 {
		c.AckRetention = DefaultAckRetention
	}
	if c.RetryDelay == 0 {
		c.RetryDelay = DefaultRetryDelay
	}
	if c.MaxRetryDelay == 0 {
		c.MaxRetryDelay = DefaultMaxRetryDelay
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxPayload == 0 {
		c.MaxPayload = DefaultMaxPayload
	}
	if c.MaxMessages == 0 {
		c.MaxMessages = DefaultMaxMessages
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.MaxKVBytes == 0 {
		c.MaxKVBytes = DefaultMaxKVBytes
	}
	if c.MaxLedger == 0 {
		c.MaxLedger = DefaultMaxLedger
	}
	if c.MaxPending == 0 {
		c.MaxPending = DefaultMaxPending
	}
	if c.Replicas == 0 {
		c.Replicas = 1
	}
}

func (c Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "nats" && u.Scheme != "tls") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: broker URL must be nats:// or tls:// without inline credentials", ErrInvalid)
	}
	if err := c.validateBounds(); err != nil {
		return err
	}
	if c.CredentialsEnv != "" && !validEnv(c.CredentialsEnv) {
		return fmt.Errorf("%w: broker credential binding", ErrInvalid)
	}
	projects := map[string]bool{}
	envs := map[string]bool{}
	for _, p := range c.Principals {
		if !ValidID(p.Project) || !validEnv(p.TokenEnv) || projects[p.Project] || envs[p.TokenEnv] {
			return fmt.Errorf("%w: principal configuration", ErrInvalid)
		}

		projects[p.Project], envs[p.TokenEnv] = true, true
	}
	ids := map[string]bool{}
	for _, l := range c.Lanes {
		if !ValidID(l.ID) || !projects[l.Owner] || ids[l.ID] || len(l.Groups) > DefaultMaxGroups {
			return fmt.Errorf("%w: lane registry", ErrInvalid)
		}
		ids[l.ID] = true
		members := append(slices.Clone(l.Publishers), l.Subscribers...)
		for group, m := range l.Groups {
			if !ValidID(group) || len(m) == 0 || len(m) > DefaultMaxMembers {
				return fmt.Errorf("%w: delivery group", ErrInvalid)
			}
			members = append(members, m...)
		}
		for group, recoverers := range l.Recoverers {
			if !ValidID(group) || len(recoverers) == 0 || len(recoverers) > DefaultMaxMembers {
				return fmt.Errorf("%w: recovery capability", ErrInvalid)
			}
			for _, project := range recoverers {
				if !allowed(l.Groups[group], project) {
					return fmt.Errorf("%w: recoverer must be a group member", ErrInvalid)
				}
			}
		}
		if len(l.Publishers) > DefaultMaxMembers || len(l.Subscribers) > DefaultMaxMembers {
			return fmt.Errorf("%w: membership bounds", ErrInvalid)
		}
		for _, p := range members {
			if !projects[p] {
				return fmt.Errorf("%w: unknown member", ErrInvalid)
			}
		}
	}
	return nil
}

func validEnv(name string) bool {
	return len(name) <= MaxIDLength && envPattern.MatchString(name)
}

func allowed(members []string, project string) bool { return slices.Contains(members, project) }

func cloneLane(l api.Lane) api.Lane {
	l.Publishers = slices.Clone(l.Publishers)
	l.Subscribers = slices.Clone(l.Subscribers)
	groups := make(map[string][]string, len(l.Groups))
	for group, members := range l.Groups {
		groups[group] = slices.Clone(members)
	}
	l.Groups = groups
	recoverers := make(map[string][]string, len(l.Recoverers))
	for group, members := range l.Recoverers {
		recoverers[group] = slices.Clone(members)
	}
	l.Recoverers = recoverers
	return l
}

func (c Config) clone() Config {
	c.Principals = slices.Clone(c.Principals)
	c.Lanes = slices.Clone(c.Lanes)
	for i := range c.Lanes {
		c.Lanes[i] = cloneLane(c.Lanes[i])
	}
	return c
}

func (b *Broker) authorize(project, lane, capability, group string) (api.Lane, error) {
	l, ok := b.registry[lane]
	if !ok {
		return api.Lane{}, ErrForbidden
	}
	switch capability {
	case "publish":
		ok = allowed(l.Publishers, project)
	case "subscribe":
		ok = allowed(l.Subscribers, project)
	case "work":
		ok = allowed(l.Groups[group], project)
	case "recover":
		ok = allowed(l.Groups[group], project) && allowed(l.Recoverers[group], project)
	default:
		ok = false
	}
	if !ok {
		return api.Lane{}, ErrForbidden
	}
	return l, nil
}

func (b *Broker) Registry(project string) []api.Lane {
	out := []api.Lane{}
	for _, l := range b.cfg.Lanes {
		member := l.Owner == project || allowed(l.Publishers, project) || allowed(l.Subscribers, project)
		for _, m := range l.Groups {
			member = member || allowed(m, project)
		}
		if member {
			out = append(out, cloneLane(l))
		}
	}
	return out
}

// bound defers formatting of the range and value text until the bound is violated.
func bound(field string, bad bool, text func() (rng string, got any)) error {
	if !bad {
		return nil
	}
	rng, got := text()
	return fmt.Errorf("%w: lanes: %s must be %s, got %v", ErrInvalid, field, rng, got)
}

func (c Config) validateBounds() error {
	checks := []error{
		bound("namespace", !ValidID(c.Namespace), func() (string, any) {
			return fmt.Sprintf("a lowercase identifier of at most %d characters", MaxIDLength), fmt.Sprintf("%q", c.Namespace)
		}),
		bound("retention", c.Retention < MinRetention || c.Retention > MaxRetention, func() (string, any) { return fmt.Sprintf("between %s and %s", MinRetention, MaxRetention), c.Retention }),
		bound("dedup", c.Dedup < MinDedup || c.Dedup > c.Retention, func() (string, any) {
			return fmt.Sprintf("between %s and retention (%s)", MinDedup, c.Retention), c.Dedup
		}),
		bound("lease", c.Lease < MinLease || c.Lease > MaxLease, func() (string, any) { return fmt.Sprintf("between %s and %s", MinLease, MaxLease), c.Lease }),
		bound("ack_retention", c.AckRetention < c.Lease || c.AckRetention > c.Retention, func() (string, any) {
			return fmt.Sprintf("between lease (%s) and retention (%s)", c.Lease, c.Retention), c.AckRetention
		}),
		bound("timeout", c.Timeout <= 0 || c.Timeout > MaxTimeout, func() (string, any) { return fmt.Sprintf("greater than 0 and at most %s", MaxTimeout), c.Timeout }),
		bound("retry_delay", c.RetryDelay < MinRetryDelay, func() (string, any) { return fmt.Sprintf("at least %s", MinRetryDelay), c.RetryDelay }),
		bound("max_retry_delay", c.MaxRetryDelay < c.RetryDelay || c.MaxRetryDelay > MaxRetryDelay, func() (string, any) {
			return fmt.Sprintf("between retry_delay (%s) and %s", c.RetryDelay, MaxRetryDelay), c.MaxRetryDelay
		}),
		bound("max_payload", c.MaxPayload <= 0 || c.MaxPayload > MaxPayload, func() (string, any) { return fmt.Sprintf("between 1 and %d", MaxPayload), c.MaxPayload }),
		bound("max_messages", c.MaxMessages <= 0 || c.MaxMessages > MaxLaneMessages, func() (string, any) { return fmt.Sprintf("between 1 and %d", MaxLaneMessages), c.MaxMessages }),
		bound("max_bytes", c.MaxBytes < int64(c.MaxPayload) || c.MaxBytes > MaxStorageBytes, func() (string, any) {
			return fmt.Sprintf("between max_payload (%d) and %d", c.MaxPayload, MaxStorageBytes), c.MaxBytes
		}),
		bound("max_kv_bytes", c.MaxKVBytes <= 0 || c.MaxKVBytes > MaxStorageBytes, func() (string, any) { return fmt.Sprintf("between 1 and %d", MaxStorageBytes), c.MaxKVBytes }),
		bound("max_pending", c.MaxPending <= 0, func() (string, any) { return "at least 1", c.MaxPending }),
		bound("max_ledger", c.MaxLedger < c.MaxPending || c.MaxLedger > MaxLedgerEntries, func() (string, any) {
			return fmt.Sprintf("between max_pending (%d) and %d", c.MaxPending, MaxLedgerEntries), c.MaxLedger
		}),
		bound("replicas", c.Replicas < 1 || c.Replicas > MaxReplicas, func() (string, any) { return fmt.Sprintf("between 1 and %d", MaxReplicas), c.Replicas }),
		bound("lanes", len(c.Lanes) > DefaultMaxLanes, func() (string, any) { return fmt.Sprintf("at most %d entries", DefaultMaxLanes), len(c.Lanes) }),
		bound("principals", len(c.Principals) == 0 || len(c.Principals) > DefaultMaxMembers, func() (string, any) {
			return fmt.Sprintf("between 1 and %d entries", DefaultMaxMembers), len(c.Principals)
		}),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	return nil
}

// flexDuration accepts a JSON number (nanoseconds) or a time.ParseDuration string.
type flexDuration time.Duration

func (d *flexDuration) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = flexDuration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\" or integer nanoseconds")
	}
	*d = flexDuration(n)
	return nil
}

// UnmarshalJSON accepts duration fields as integer nanoseconds or strings such as "30s".
// It decodes strictly: the outer decoder's DisallowUnknownFields does not propagate
// into Unmarshaler methods, so unknown keys must be rejected here.
func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config
	aux := struct {
		*plain
		Retention     flexDuration `json:"retention"`
		Dedup         flexDuration `json:"dedup"`
		Lease         flexDuration `json:"lease"`
		AckRetention  flexDuration `json:"ack_retention"`
		RetryDelay    flexDuration `json:"retry_delay"`
		MaxRetryDelay flexDuration `json:"max_retry_delay"`
		Timeout       flexDuration `json:"timeout"`
	}{plain: (*plain)(c)}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&aux); err != nil {
		return err
	}
	c.Retention, c.Dedup, c.Lease = time.Duration(aux.Retention), time.Duration(aux.Dedup), time.Duration(aux.Lease)
	c.AckRetention, c.RetryDelay = time.Duration(aux.AckRetention), time.Duration(aux.RetryDelay)
	c.MaxRetryDelay, c.Timeout = time.Duration(aux.MaxRetryDelay), time.Duration(aux.Timeout)
	return nil
}

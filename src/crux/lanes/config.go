// Package lanes owns forge-neutral durable events and work. Streams and all
// mutable delivery state live in JetStream; no process-local journal is used.
package lanes

import (
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
	if !ValidID(c.Namespace) || c.Retention < MinRetention || c.Retention > MaxRetention ||
		c.Dedup < MinDedup || c.Dedup > c.Retention || c.Lease < MinLease || c.Lease > MaxLease ||
		c.AckRetention < c.Lease || c.AckRetention > c.Retention || c.Timeout <= 0 || c.Timeout > MaxTimeout ||
		c.RetryDelay < MinRetryDelay || c.MaxRetryDelay < c.RetryDelay || c.MaxRetryDelay > MaxRetryDelay ||
		c.MaxPayload <= 0 || c.MaxPayload > MaxPayload || c.MaxMessages <= 0 || c.MaxMessages > MaxLaneMessages ||
		c.MaxBytes < int64(c.MaxPayload) || c.MaxBytes > MaxStorageBytes || c.MaxKVBytes <= 0 || c.MaxKVBytes > MaxStorageBytes ||
		c.MaxPending <= 0 || c.MaxLedger < c.MaxPending || c.MaxLedger > MaxLedgerEntries ||
		c.Replicas < 1 || c.Replicas > MaxReplicas || len(c.Lanes) > DefaultMaxLanes ||
		len(c.Principals) == 0 || len(c.Principals) > DefaultMaxMembers {
		return fmt.Errorf("%w: lane configuration bounds", ErrInvalid)
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

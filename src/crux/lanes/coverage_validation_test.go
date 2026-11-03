package lanes

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

func TestConfigurationRejectsInvalidMembershipAndCredentialBindings(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"credential binding", func(c *Config) { c.CredentialsEnv = "not-an-env" }},
		{"duplicate principal", func(c *Config) { c.Principals = append(c.Principals, c.Principals[0]) }},
		{"invalid group", func(c *Config) { c.Lanes[0].Groups["bad.*"] = []string{"atlas"} }},
		{"invalid recoverer group", func(c *Config) { c.Lanes[0].Recoverers["bad.*"] = []string{"atlas"} }},
		{"recoverer not a member", func(c *Config) { c.Lanes[0].Recoverers["workers"] = []string{"outsider"} }},
		{"publisher limit", func(c *Config) {
			c.Lanes[0].Publishers = make([]string, DefaultMaxMembers+1)
			for i := range c.Lanes[0].Publishers {
				c.Lanes[0].Publishers[i] = "atlas"
			}
		}},
		{"unknown subscriber", func(c *Config) { c.Lanes[0].Subscribers = []string{"outsider"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig("nats://127.0.0.1:4222")
			tt.change(&c)
			if err := c.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid administrator configuration accepted", err)
			}
		})
	}
}

func TestGroupBudgetRejectsInvalidHistoryReservationsAndMissingLedger(t *testing.T) {
	state, _, err := maximumGroupShape()
	if err != nil {
		t.Fatal(err)
	}
	state.Slots = minGroupSlots
	for _, slots := range []int{-minGroupSlots, MaxLedgerEntries + minGroupSlots} {
		if _, err := groupReservation(slots); !errors.Is(err, ErrCapacity) {
			t.Fatal("invalid reservation count accepted", slots, err)
		}
	}
	for id, record := range state.Recoveries {
		original := record
		record.Result.Project = ""
		state.Recoveries[id] = record
		if err := validateGroup(state); !errors.Is(err, ErrConflict) {
			t.Fatal("invalid recovery identity accepted", err)
		}
		record = original
		record.Result.Expirations = append([]api.LaneExpiration(nil), original.Result.Expirations...)
		record.Result.Expirations[0].Attempt = 0
		state.Recoveries[id] = record
		if err := validateGroup(state); !errors.Is(err, ErrConflict) {
			t.Fatal("invalid expiration history accepted", err)
		}
		state.Recoveries[id] = original
		break
	}
	state.Leases = nil
	if err := validateGroup(state); !errors.Is(err, ErrConflict) {
		t.Fatal("missing durable ledger accepted", err)
	}
}

func TestPruningPreservesSlotsAndLiveHistoryWhileExpiringTerminalRecords(t *testing.T) {
	b := &Broker{cfg: Config{AckRetention: MinLease}}
	now := time.Now().UTC()
	old := now.Add(-b.cfg.AckRetention)
	token := strings.Repeat("a", receiptBytes*2)
	const slots = 3
	state := groupState{Slots: slots, Leases: map[uint64]lease{
		1: {Receipt: token, Project: "atlas", Expires: now, Attempt: 1, Done: true, AckedAt: old},
		2: {Receipt: token, Project: "atlas", Expires: now, Attempt: 1, Expired: true, RetiredAt: old},
		3: {Receipt: token, Project: "atlas", Expires: now.Add(MinLease), Attempt: 1},
	}, Recoveries: map[string]recoveryRecord{
		"old":  {RequestHash: digest([]byte("old")), Result: api.LaneRecovered{ID: "old", Project: "atlas", RecoveredAt: old}},
		"live": {RequestHash: digest([]byte("live")), Result: api.LaneRecovered{ID: "live", Project: "atlas", RecoveredAt: now}},
	}}
	if err := validateGroup(state); err != nil {
		t.Fatal("terminal-history fixture is not otherwise valid", err)
	}
	b.pruneGroup(&state, now)
	if len(state.Leases) != 1 || state.Leases[3].Project != "atlas" || state.Slots != slots {
		t.Fatal("pruning lost a live lease or shrank reusable slots", state)
	}
	if len(state.Recoveries) != 1 || state.Recoveries["live"].Result.RecoveredAt != now {
		t.Fatal("pruning lost live recovery response history", state.Recoveries)
	}
	b.cfg.RetryDelay, b.cfg.MaxRetryDelay = MinRetryDelay, MinRetryDelay+MinRetryDelay/2
	if delay := b.retryDelay(^uint64(0)); delay != b.cfg.MaxRetryDelay {
		t.Fatal("non-power-of-two retry ceiling exceeded", delay)
	}
}

package github

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const apiBudgetHysteresis = 100

type APIBudgetMode int

const (
	APIBudgetNormal APIBudgetMode = iota
	APIBudgetConserve
	APIBudgetCritical
)

func (m APIBudgetMode) String() string {
	switch m {
	case APIBudgetConserve:
		return "conserve"
	case APIBudgetCritical:
		return "critical"
	default:
		return "normal"
	}
}

type APIBudgetSnapshot struct {
	Mode         string    `json:"mode"`
	Remaining    int       `json:"remaining"`
	Limit        int       `json:"limit"`
	Reset        time.Time `json:"reset"`
	Since        time.Time `json:"since"`
	SkippedSteps []string  `json:"skipped_steps,omitempty"`
}

type apiBudgetState struct {
	mu       sync.Mutex
	reserve  int
	critical int
	mode     APIBudgetMode
	since    time.Time
	skipped  []string
}

func (c *Client) SetAPIBudgetThresholds(reserve, critical int) {
	if c == nil {
		return
	}
	c.apiBudget.mu.Lock()
	defer c.apiBudget.mu.Unlock()
	c.apiBudget.reserve = reserve
	c.apiBudget.critical = critical
}

func (c *Client) RecordAPIBudgetSkippedSteps(steps []string) {
	if c == nil {
		return
	}
	c.apiBudget.mu.Lock()
	defer c.apiBudget.mu.Unlock()
	c.apiBudget.skipped = append([]string(nil), steps...)
}

func (c *Client) APIBudgetMode() (APIBudgetMode, APIBudgetSnapshot) {
	if c == nil {
		return APIBudgetNormal, APIBudgetSnapshot{Mode: APIBudgetNormal.String()}
	}
	entry := c.rateLimits.snapshot(githubCoreRateLimitResource)
	now := c.rateLimits.clock()
	c.apiBudget.mu.Lock()
	defer c.apiBudget.mu.Unlock()
	reserve, critical := c.apiBudget.reserve, c.apiBudget.critical
	if reserve <= 0 {
		reserve = 800
	}
	if critical <= 0 {
		critical = 250
	}
	old := c.apiBudget.mode
	mode := old
	if entry.Limit <= 0 {
		mode = APIBudgetNormal
	} else if now.After(entry.Reset) || now.Equal(entry.Reset) {
		mode = APIBudgetNormal
	} else {
		switch old {
		case APIBudgetCritical:
			if entry.Remaining > critical+apiBudgetHysteresis {
				if entry.Remaining < reserve {
					mode = APIBudgetConserve
				} else {
					mode = APIBudgetNormal
				}
			}
		case APIBudgetConserve:
			if entry.Remaining < critical {
				mode = APIBudgetCritical
			} else if entry.Remaining > reserve+apiBudgetHysteresis {
				mode = APIBudgetNormal
			}
		default:
			if entry.Remaining < critical {
				mode = APIBudgetCritical
			} else if entry.Remaining < reserve {
				mode = APIBudgetConserve
			}
		}
	}
	if mode != old || c.apiBudget.since.IsZero() {
		c.apiBudget.mode = mode
		c.apiBudget.since = now
		if mode != old && c.logger != nil {
			level := slog.LevelInfo
			if mode == APIBudgetCritical {
				level = slog.LevelWarn
			}
			c.logger.LogAttrs(context.Background(), level, "github api budget mode changed",
				slog.String("from", old.String()), slog.String("to", mode.String()),
				slog.Int("remaining", entry.Remaining), slog.Duration("reset_in", entry.Reset.Sub(now)))
		}
	}
	return c.apiBudget.mode, APIBudgetSnapshot{
		Mode:         c.apiBudget.mode.String(),
		Remaining:    entry.Remaining,
		Limit:        entry.Limit,
		Reset:        entry.Reset,
		Since:        c.apiBudget.since,
		SkippedSteps: append([]string(nil), c.apiBudget.skipped...),
	}
}

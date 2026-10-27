package logread

import (
	"fmt"
	"strings"
	"time"
)

// parseHead reads the time and level of a hive log record. The hive logs
// through slog's JSON handler, which always writes them first:
// {"time":"<RFC 3339>","level":"<LEVEL>",...}. A line in any other shape has
// no known time or level.
func parseHead(raw string) (time.Time, string) {
	const timeKey, levelKey = `{"time":"`, `,"level":"`
	rest, ok := strings.CutPrefix(raw, timeKey)
	if !ok {
		return time.Time{}, ""
	}
	value, rest, ok := strings.Cut(rest, `"`)
	if !ok {
		return time.Time{}, ""
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, ""
	}
	rest, ok = strings.CutPrefix(rest, levelKey)
	if !ok {
		return t, ""
	}
	level, _, ok := strings.Cut(rest, `"`)
	if !ok {
		return t, ""
	}
	return t, normalizeLevel(level)
}

// normalizeLevel maps slog level text (DEBUG, INFO, WARN, ERROR, and offsets
// such as "WARN+2") to the lowercase level names the filter takes.
func normalizeLevel(level string) string {
	level = strings.ToLower(strings.TrimSpace(level))
	if i := strings.IndexAny(level, "+-"); i > 0 {
		level = level[:i]
	}
	if level == "warning" {
		return "warn"
	}
	return level
}

var levelRanks = map[string]int{"debug": 1, "info": 2, "warn": 3, "error": 4}

func levelRank(level string) int { return levelRanks[level] }

// ParseMinLevel validates a minimum-level filter: debug, info, warn (or
// warning) or error, case-insensitive. Empty means no level filter.
func ParseMinLevel(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	level := normalizeLevel(raw)
	if levelRank(level) == 0 || strings.ContainsAny(raw, "+-") {
		return "", fmt.Errorf("min_level must be one of debug, info, warn or error")
	}
	return level, nil
}

// ParseInstant parses a since/until value. It must be RFC 3339 with an
// explicit offset ("Z" or "+hh:mm"); a value without one is refused rather
// than guessed in some time zone.
func ParseInstant(name, raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC 3339 timestamp with an explicit offset, such as 2026-01-02T15:04:05Z or 2026-01-02T10:04:05-05:00", name)
	}
	return t, nil
}

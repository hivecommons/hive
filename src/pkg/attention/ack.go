package attention

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxSnooze bounds `/hive snooze`: a snooze quiets reminders, it must not
// become an indefinite mute of a condition that still needs a person.
const MaxSnooze = 30 * 24 * time.Hour

// CommandKind is an attention sub-command of `/hive`.
type CommandKind string

const (
	CommandAck    CommandKind = "ack"
	CommandSnooze CommandKind = "snooze"
)

var (
	// ErrNoEpisode means the item has no unresolved episode the command names.
	ErrNoEpisode = errors.New("attention: no unresolved attention episode matches")
	// ErrNotPermitted means the caller is neither the episode's named
	// recipient nor a maintainer; nothing was recorded.
	ErrNotPermitted = errors.New("attention: only the responsible person or a maintainer may ack or snooze")
)

// Command is a parsed `/hive ack [episode]` or `/hive snooze <duration>
// [episode]`. Target is an optional episode key-hash prefix as shown in the
// status card; empty means every unresolved episode on the item.
type Command struct {
	Kind   CommandKind
	Snooze time.Duration
	Target string
}

// ParseCommand reads an attention command off the comment's first non-empty
// line, never inside a quote or a fence. ok reports whether the comment is an
// attention command at all; err reports a malformed one (for example a
// missing or out-of-range snooze duration) so the caller can explain it.
func ParseCommand(body string) (cmd Command, ok bool, err error) {
	line := ""
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	if line == "" || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
		return Command{}, false, nil
	}
	f := strings.Fields(line)
	if len(f) < 2 || !strings.EqualFold(f[0], "/hive") {
		return Command{}, false, nil
	}
	switch CommandKind(strings.ToLower(f[1])) {
	case CommandAck:
		cmd = Command{Kind: CommandAck}
		if len(f) > 2 {
			cmd.Target = f[2]
		}
	case CommandSnooze:
		cmd = Command{Kind: CommandSnooze}
		if len(f) < 3 {
			return cmd, true, errors.New("snooze needs a duration, for example `/hive snooze 2d`")
		}
		d, perr := ParseSnooze(f[2])
		if perr != nil {
			return cmd, true, perr
		}
		cmd.Snooze = d
		if len(f) > 3 {
			cmd.Target = f[3]
		}
	default:
		return Command{}, false, nil
	}
	cmd.Target = strings.ToLower(strings.Trim(cmd.Target, "`"))
	if cmd.Target != "" && strings.Trim(cmd.Target, "0123456789abcdef") != "" {
		return cmd, true, fmt.Errorf("episode %q is not a key from the attention status card", cmd.Target)
	}
	return cmd, true, nil
}

// ParseSnooze accepts Go durations ("90m", "4h") plus whole days ("2d") and
// weeks ("1w"), bounded by MaxSnooze.
func ParseSnooze(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	var d time.Duration
	var err error
	if n := len(s); n > 1 && (s[n-1] == 'd' || s[n-1] == 'w') {
		var v int
		v, err = strconv.Atoi(s[:n-1])
		unit := 24 * time.Hour
		if s[n-1] == 'w' {
			unit *= 7
		}
		if err == nil && v > 0 && v <= int(MaxSnooze/unit) {
			d = time.Duration(v) * unit
		}
	} else {
		d, err = time.ParseDuration(s)
	}
	if err != nil || d < time.Minute || d > MaxSnooze {
		return 0, fmt.Errorf("snooze duration %q must be between 1m and 30d, for example `4h` or `2d`", s)
	}
	return d, nil
}

// MayAct reports whether actor may ack or snooze e: the episode's named
// recipient (case-insensitive, optional `@`) or a maintainer.
func MayAct(e Episode, actor string, maintainer bool) bool {
	if maintainer {
		return true
	}
	a := canonical(strings.TrimPrefix(strings.TrimSpace(actor), "@"))
	return a != "" && a == canonical(strings.TrimPrefix(strings.TrimSpace(e.Key.Recipient), "@"))
}

// Apply records an ack or snooze from actor on every unresolved episode of
// repo#item that cmd targets and actor may act on. It changes only the
// reminder state on the episode (AckedAt/AckedBy, SnoozedUntil/SnoozedBy).
// It never touches a hold label, a merge gate or a review verdict:
// acknowledgement is not approval, and a snooze releases no hold.
func (s *Store) Apply(repo string, item int, actor string, maintainer bool, cmd Command) ([]Episode, error) {
	if cmd.Kind != CommandAck && cmd.Kind != CommandSnooze {
		return nil, errors.New("attention: unsupported command")
	}
	if cmd.Kind == CommandSnooze && (cmd.Snooze <= 0 || cmd.Snooze > MaxSnooze) {
		return nil, errors.New("attention: snooze duration out of range")
	}
	actor = strings.TrimPrefix(strings.TrimSpace(actor), "@")
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched, permitted []*Episode
	for hash, e := range s.state.Episodes {
		if e.State == Resolved || e.Key == (Key{}) || canonical(e.Key.Repo) != canonical(repo) || e.Key.Item != item || !strings.HasPrefix(hash, cmd.Target) {
			continue
		}
		matched = append(matched, e)
		if MayAct(*e, actor, maintainer) {
			permitted = append(permitted, e)
		}
	}
	if len(matched) == 0 {
		return nil, ErrNoEpisode
	}
	if len(permitted) == 0 {
		return nil, ErrNotPermitted
	}
	now := s.now().UTC()
	out := make([]Episode, 0, len(permitted))
	for _, e := range permitted {
		switch cmd.Kind {
		case CommandAck:
			e.State = Acknowledged
			if e.AckedAt.IsZero() {
				e.AckedAt = now
			}
			e.AckedBy = actor
		case CommandSnooze:
			e.SnoozedUntil = now.Add(cmd.Snooze)
			e.SnoozedBy = actor
		}
		e.WouldNotify = false
		s.releaseBudget(e)
		out = append(out, *e)
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	return out, nil
}

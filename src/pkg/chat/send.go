package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Outbound delivery contract (hivecommons/hive#9127).
//
// Backend.Send receives one chunk of at most Config.MessageLimit runes: the
// drain loop splits every enqueued message with SplitMessage before it reaches
// the transport, so a backend never has to reject a reply for being too long.
// A transport that was rate limited (or hit a transient server error) reports
// it by returning a *RetryableError; the drain loop then waits the
// transport-provided Retry-After (capped at MaxRetryAfter) — or its own
// pacing interval when none was given — and re-sends the same chunk, up to
// maxSendAttempts. Any other error, or exhausting the attempts, drops the
// chunk with a warning. Waits are context-aware so shutdown never blocks on a
// rate-limit sleep.
const (
	// MaxRetryAfter caps the wait a backend may request through Retryable.
	// Anything longer than this is treated as "the transport is down", and it
	// is better to try again soon than to park the drain loop for minutes on
	// one message while everything behind it goes stale.
	MaxRetryAfter = 60 * time.Second
	// maxSendAttempts bounds the retries of a single chunk before it is
	// dropped, so a persistently rate-limited channel cannot wedge the queue.
	maxSendAttempts = 3
)

// RetryableError marks a Send failure the drain loop should retry.
type RetryableError struct {
	// RetryAfter is how long the transport asked the caller to wait before
	// resending. Zero means the transport gave no hint.
	RetryAfter time.Duration
	Err        error
}

func (e *RetryableError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%v (retry after %s)", e.Err, e.RetryAfter)
	}
	return e.Err.Error()
}

func (e *RetryableError) Unwrap() error { return e.Err }

// Retryable wraps err so the drain loop retries the chunk after `after`,
// capped at MaxRetryAfter. Negative or zero waits are recorded as zero.
func Retryable(err error, after time.Duration) error {
	if after < 0 {
		after = 0
	}
	if after > MaxRetryAfter {
		after = MaxRetryAfter
	}
	return &RetryableError{RetryAfter: after, Err: err}
}

// sendChunk delivers one already-split chunk, retrying on *RetryableError.
// It returns false only when ctx ended while waiting to retry.
func (s *Service) sendChunk(ctx context.Context, content string) bool {
	for attempt := 1; ; attempt++ {
		err := s.backend.Send(content)
		if err == nil {
			return true
		}
		var retryable *RetryableError
		if !errors.As(err, &retryable) || attempt >= maxSendAttempts {
			s.logger.Warn("chat send failed; message dropped",
				"backend", s.backend.Name(), "attempt", attempt, "error", err)
			return true
		}
		wait := retryable.RetryAfter
		if wait <= 0 {
			wait = s.sendInterval
		}
		s.logger.Warn("chat send failed; retrying",
			"backend", s.backend.Name(), "attempt", attempt, "retry_after", wait, "error", err)
		if !sleepContext(ctx, wait) {
			return false
		}
	}
}

// sleepContext waits for d or until ctx is done; it reports whether the full
// wait elapsed.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

const (
	fenceMarker = "```"
	fenceClose  = "\n```"
	// minFencedSplitLimit is the smallest limit at which the splitter still
	// spends runes on closing and reopening code fences across chunks. Below
	// it, fences are treated as ordinary text so a chunk always fits.
	minFencedSplitLimit = 32
)

// SplitMessage splits s into chunks of at most limit runes. It breaks on line
// boundaries and only cuts inside a line when the line alone exceeds the
// limit. An open ``` fence is closed at the end of a chunk and reopened (with
// the same info string) at the start of the next so every chunk renders on
// its own. A limit <= 0 disables splitting.
func SplitMessage(s string, limit int) []string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return []string{s}
	}
	sp := &splitter{limit: limit, fences: limit >= minFencedSplitLimit}
	for _, line := range strings.Split(s, "\n") {
		sp.addLine(line)
	}
	sp.flush()
	return sp.parts
}

type splitter struct {
	limit  int
	fences bool
	parts  []string

	cur      strings.Builder
	curRunes int
	// reopened is true while cur holds only the fence line flush() re-emitted
	// after cutting inside a code block; such a chunk carries no content.
	reopened bool
	// inFence tracks the fence state at the end of cur; fenceOpen is the line
	// that opened it, reused when a chunk starts inside the block.
	inFence   bool
	fenceOpen string
}

func (sp *splitter) isFenceLine(line string) bool {
	return sp.fences && strings.HasPrefix(strings.TrimLeft(line, " \t"), fenceMarker)
}

// sep is the rune cost of the newline that joins line onto cur.
func (sp *splitter) sep() int {
	if sp.cur.Len() > 0 {
		return 1
	}
	return 0
}

func (sp *splitter) write(text string) {
	if sp.cur.Len() > 0 {
		sp.cur.WriteByte('\n')
		sp.curRunes++
	}
	sp.cur.WriteString(text)
	sp.curRunes += utf8.RuneCountInString(text)
	sp.reopened = false
}

func (sp *splitter) flush() {
	if sp.cur.Len() == 0 || sp.reopened {
		return
	}
	part := sp.cur.String()
	if sp.inFence {
		// A chunk that ends on the opening fence line would carry an empty
		// block; leave the opener for the next chunk instead of closing it.
		if trimmed, ok := strings.CutSuffix(part, sp.fenceOpen); ok && (trimmed == "" || strings.HasSuffix(trimmed, "\n")) {
			part = strings.TrimSuffix(trimmed, "\n")
		} else {
			part += fenceClose
		}
	}
	if part != "" {
		sp.parts = append(sp.parts, part)
	}
	sp.cur.Reset()
	sp.curRunes = 0
	if sp.inFence {
		open := sp.fenceOpen
		// A very long info string would eat the chunk; fall back to a bare
		// fence so the reopened block still has room for content.
		if utf8.RuneCountInString(open) > sp.limit/4 {
			open = fenceMarker
		}
		sp.write(open)
		sp.reopened = true
	}
}

func (sp *splitter) addLine(line string) {
	// A fence line that cannot fit in a chunk with its closer is treated as
	// plain text; balancing it would only produce oversize chunks.
	toggles := sp.isFenceLine(line) && utf8.RuneCountInString(line)+len(fenceClose) <= sp.limit
	if toggles && sp.reopened {
		// The cut landed right before the closing fence: the reopened block
		// would be empty, so drop the reopener instead of emitting "```\n```".
		sp.cur.Reset()
		sp.curRunes = 0
		sp.reopened = false
		sp.inFence = false
		return
	}
	fenceAfter := sp.inFence != toggles
	reserve := 0
	if fenceAfter {
		reserve = len(fenceClose)
	}
	need := utf8.RuneCountInString(line)
	if sp.curRunes+sp.sep()+need+reserve > sp.limit {
		sp.flush()
	}
	if sp.curRunes+sp.sep()+need+reserve <= sp.limit {
		sp.write(line)
		sp.inFence = fenceAfter
		if toggles && fenceAfter {
			sp.fenceOpen = line
		}
		return
	}
	// The line alone overflows a chunk: cut it on rune boundaries. The
	// fence state cannot change mid-line, so reserve stays constant.
	for line != "" {
		available := sp.limit - sp.curRunes - sp.sep() - reserve
		if available < 1 {
			if sp.cur.Len() > 0 && !sp.reopened {
				sp.flush()
				continue
			}
			// Unreachable while fences are only balanced at
			// limit >= minFencedSplitLimit; kept so the loop always advances.
			available = sp.limit
		}
		piece := takeRunes(line, available)
		sp.write(piece)
		line = line[len(piece):]
		if line != "" {
			sp.flush()
		}
	}
	sp.inFence = fenceAfter
	if toggles && fenceAfter {
		sp.fenceOpen = fenceMarker
	}
}

// takeRunes returns the longest prefix of s holding at most n runes.
func takeRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	idx := 0
	for count := 0; idx < len(s) && count < n; count++ {
		_, size := utf8.DecodeRuneInString(s[idx:])
		idx += size
	}
	return s[:idx]
}

package chat

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/internal/testutil"
)

// flakyBackend answers each Send from a scripted list of errors, then nil.
type flakyBackend struct {
	mu     sync.Mutex
	script []error
	sent   []string
}

func (b *flakyBackend) Name() string { return "flaky" }
func (b *flakyBackend) Send(content string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, content)
	if len(b.script) == 0 {
		return nil
	}
	err := b.script[0]
	b.script = b.script[1:]
	return err
}
func (b *flakyBackend) SetTopic(string) error                       { return nil }
func (b *flakyBackend) Listen(ctx context.Context, _ func(Message)) { <-ctx.Done() }
func (b *flakyBackend) sentSnapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...)
}

func newDrainService(backend Backend, limit int) *Service {
	return NewService(backend, Config{MessageLimit: limit, SendInterval: time.Millisecond}, discardLogger())
}

// drainSettled enqueues a sentinel behind the messages under test and waits
// for it. msgQueue is FIFO and drainLoop sends in order, so once the sentinel
// lands every earlier send (retries included) is final; no timing margin is
// needed to prove that no extra send follows. Returns the sends before it.
func drainSettled(t *testing.T, s *Service, backend *flakyBackend) []string {
	t.Helper()
	const sentinel = "drain-settled-sentinel"
	s.enqueue(sentinel)
	testutil.Eventually(t, 2*time.Second, func() bool {
		sent := backend.sentSnapshot()
		return len(sent) > 0 && sent[len(sent)-1] == sentinel
	}, "drain loop never reached the sentinel")
	sent := backend.sentSnapshot()
	return sent[:len(sent)-1]
}

func TestDrainLoop_RetriesRetryableSendWithoutReordering(t *testing.T) {
	backend := &flakyBackend{script: []error{Retryable(errors.New("429"), 5*time.Millisecond)}}
	s := newDrainService(backend, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.drainLoop(ctx)

	s.enqueue("pong")
	s.enqueue("after")

	got := drainSettled(t, s, backend)
	want := []string{"pong", "pong", "after"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sends = %q, want %q", got, want)
	}
}

func TestDrainLoop_NonRetryableErrorDropsOnce(t *testing.T) {
	backend := &flakyBackend{script: []error{errors.New("400 bad request")}}
	s := newDrainService(backend, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.drainLoop(ctx)

	s.enqueue("bad")
	s.enqueue("next")

	got := drainSettled(t, s, backend)
	if len(got) != 2 || got[0] != "bad" || got[1] != "next" {
		t.Fatalf("sends = %q, want one attempt then the next message", got)
	}
}

func TestDrainLoop_BoundsRetryAttempts(t *testing.T) {
	always := make([]error, maxSendAttempts)
	for i := range always {
		always[i] = Retryable(errors.New("429"), time.Millisecond)
	}
	backend := &flakyBackend{script: always}
	s := newDrainService(backend, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.drainLoop(ctx)

	s.enqueue("stuck")
	s.enqueue("next")

	got := drainSettled(t, s, backend)
	if len(got) != maxSendAttempts+1 {
		t.Fatalf("sends = %d, want %d attempts then the next message: %q", len(got), maxSendAttempts+1, got)
	}
	for _, c := range got[:maxSendAttempts] {
		if c != "stuck" {
			t.Fatalf("sends = %q, want %d attempts of the same chunk", got, maxSendAttempts)
		}
	}
	if got[maxSendAttempts] != "next" {
		t.Fatalf("sends = %q, want the queue to move on after the attempt budget", got)
	}
}

func TestDrainLoop_RetryWaitStopsOnCancel(t *testing.T) {
	backend := &flakyBackend{script: []error{Retryable(errors.New("429"), MaxRetryAfter)}}
	s := newDrainService(backend, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.drainLoop(ctx)
	}()

	s.enqueue("pong")
	testutil.Eventually(t, 2*time.Second, func() bool {
		return len(backend.sentSnapshot()) >= 1
	}, "backend never saw the first attempt")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain loop stayed parked in a retry wait after cancel")
	}
	if got := backend.sentSnapshot(); len(got) != 1 {
		t.Fatalf("sends after cancel = %q, want the single pre-cancel attempt", got)
	}
}

func TestDrainLoop_SplitsToBackendLimitInOrder(t *testing.T) {
	backend := &flakyBackend{}
	s := newDrainService(backend, 40)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.drainLoop(ctx)

	var lines []string
	for i := 0; i < 12; i++ {
		lines = append(lines, strings.Repeat("x", 15)+string(rune('a'+i)))
	}
	msg := strings.Join(lines, "\n")
	s.enqueue(msg)

	testutil.Eventually(t, 2*time.Second, func() bool {
		return strings.Join(backend.sentSnapshot(), "\n") == msg
	}, "split chunks never reassembled the message")
	for _, chunk := range backend.sentSnapshot() {
		if utf8.RuneCountInString(chunk) > 40 {
			t.Fatalf("chunk exceeds the backend limit: %q", chunk)
		}
	}
	if n := len(backend.sentSnapshot()); n < 2 {
		t.Fatalf("expected the message to be split, got %d send(s)", n)
	}
}

func TestRetryable_CapsRetryAfterAndUnwraps(t *testing.T) {
	base := errors.New("rate limited")
	err := Retryable(base, 10*time.Minute)
	var retryable *RetryableError
	if !errors.As(err, &retryable) || retryable.RetryAfter != MaxRetryAfter {
		t.Fatalf("Retryable(10m) = %#v, want RetryAfter capped at %s", err, MaxRetryAfter)
	}
	if !errors.Is(err, base) {
		t.Fatal("Retryable must unwrap to the transport error")
	}
	if !errors.As(Retryable(base, -time.Second), &retryable) || retryable.RetryAfter != 0 {
		t.Fatal("negative Retry-After must be recorded as no hint")
	}
}

func TestSplitMessage_ShortOrUnlimitedPassesThrough(t *testing.T) {
	long := strings.Repeat("y", 100)
	for _, tc := range []struct {
		in    string
		limit int
	}{
		{"", 10}, {"short", 10}, {long, 0}, {long, -1}, {long, 100},
	} {
		got := SplitMessage(tc.in, tc.limit)
		if len(got) != 1 || got[0] != tc.in {
			t.Fatalf("SplitMessage(%d runes, %d) = %d parts, want passthrough", len(tc.in), tc.limit, len(got))
		}
	}
}

func TestSplitMessage_BreaksOnLineBoundaries(t *testing.T) {
	lines := []string{"line one is here", "line two is here", "line three here", "four"}
	in := strings.Join(lines, "\n")
	got := SplitMessage(in, 35)
	want := []string{"line one is here\nline two is here", "line three here\nfour"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("parts = %q, want %q", got, want)
	}
}

func TestSplitMessage_HardSplitsLongLinesOnRuneBoundaries(t *testing.T) {
	in := strings.Repeat("界", 45)
	got := SplitMessage(in, 20)
	if len(got) != 3 || strings.Join(got, "") != in {
		t.Fatalf("parts = %d, want 3 that reassemble the input", len(got))
	}
	for _, part := range got {
		if !utf8.ValidString(part) || utf8.RuneCountInString(part) > 20 {
			t.Fatalf("bad part %q", part)
		}
	}
}

func TestSplitMessage_KeepsCodeFencesBalanced(t *testing.T) {
	var body []string
	for i := 0; i < 30; i++ {
		body = append(body, strings.Repeat("c", 20))
	}
	in := "intro\n```go\n" + strings.Join(body, "\n") + "\n```\noutro"
	const limit = 100
	got := SplitMessage(in, limit)
	if len(got) < 3 {
		t.Fatalf("expected several chunks, got %d", len(got))
	}
	var seen []string
	for i, part := range got {
		if utf8.RuneCountInString(part) > limit {
			t.Fatalf("part %d has %d runes, limit %d", i, utf8.RuneCountInString(part), limit)
		}
		if strings.Count(part, "```")%2 != 0 {
			t.Fatalf("part %d has an unbalanced fence: %q", i, part)
		}
		if i > 0 && i < len(got)-1 && !strings.HasPrefix(part, "```go\n") {
			t.Fatalf("part %d does not reopen the fence with its info string: %q", i, part)
		}
		for _, line := range strings.Split(part, "\n") {
			if strings.HasPrefix(line, "```") {
				continue
			}
			seen = append(seen, line)
		}
	}
	// Reassembling the non-fence lines proves the splitter only added fence
	// lines and dropped no content.
	if strings.Join(seen, "\n") != "intro\n"+strings.Join(body, "\n")+"\noutro" {
		t.Fatalf("content lost across chunks:\n%q", seen)
	}
}

func TestSplitMessage_HardSplitInsideFenceStaysBalanced(t *testing.T) {
	in := "```\n" + strings.Repeat("z", 250) + "\n```"
	const limit = 64
	got := SplitMessage(in, limit)
	var content strings.Builder
	for i, part := range got {
		if utf8.RuneCountInString(part) > limit {
			t.Fatalf("part %d has %d runes", i, utf8.RuneCountInString(part))
		}
		if !strings.HasPrefix(part, "```\n") || !strings.HasSuffix(part, "\n```") {
			t.Fatalf("part %d is not a self-contained block: %q", i, part)
		}
		content.WriteString(strings.TrimSuffix(strings.TrimPrefix(part, "```\n"), "\n```"))
	}
	if content.String() != strings.Repeat("z", 250) {
		t.Fatalf("code content lost: %d runes", content.Len())
	}
}

// TestSplitMessage_Invariants drives the splitter with seeded random mixes of
// short lines, oversize lines, blank lines, fences and multi-byte runes and
// checks the three properties every consumer relies on: no chunk exceeds the
// limit, every chunk's fences are balanced, and no non-fence content is lost
// or reordered.
func TestSplitMessage_Invariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(9127, 0))
	atoms := []string{"", "word", "界界界", "```", "```go", "  ```", "\t", strings.Repeat("é", 70), strings.Repeat("-", 300)}
	for iter := 0; iter < 2000; iter++ {
		limit := minFencedSplitLimit + rng.IntN(120)
		var lines []string
		for n := rng.IntN(25); n >= 0; n-- {
			lines = append(lines, atoms[rng.IntN(len(atoms))])
		}
		in := strings.Join(lines, "\n")
		got := SplitMessage(in, limit)

		totalFences := 0
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
				totalFences++
			}
		}
		var kept []string
		for i, part := range got {
			if utf8.RuneCountInString(part) > limit {
				t.Fatalf("iter %d limit %d: part %d has %d runes\ninput %q", iter, limit, i, utf8.RuneCountInString(part), in)
			}
			fences := 0
			for _, line := range strings.Split(part, "\n") {
				if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
					fences++
					continue
				}
				kept = append(kept, line)
			}
			// An input whose own fences are unbalanced legitimately leaves
			// its final chunk open; every earlier chunk must still close.
			if fences%2 != 0 && (i < len(got)-1 || totalFences%2 == 0) {
				t.Fatalf("iter %d limit %d: part %d has %d fence lines\npart %q\ninput %q", iter, limit, i, fences, part, in)
			}
		}
		var want []string
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
				continue
			}
			want = append(want, line)
		}
		// Splitting an oversize line yields several kept lines; compare the
		// concatenation, which is what the reader sees back to back.
		if strings.Join(kept, "") != strings.Join(want, "") {
			t.Fatalf("iter %d limit %d: content changed\nkept %q\nwant %q\ninput %q", iter, limit, kept, want, in)
		}
	}
}

package turn

import (
	"context"
	"fmt"
	"strings"
)

// DigestMetadataKey marks the synthetic message DigestCompactor inserts.
const DigestMetadataKey = "hive_digest"

const digestHeader = "[hive context digest] Earlier messages were dropped to save context. " +
	"Tool calls already made in this session (their results are no longer shown; do not repeat them unless you need the content again):"

// maxDigestEntries bounds the digest so it cannot grow without limit.
const maxDigestEntries = 80

// DigestCompactor keeps the pinned head (system primer and task), a recent
// window, and replaces everything in between with one deterministic digest
// listing the tool calls already made. Built from live Family C runs (RFC
// #18464): a plain sliding window dropped the task itself and the model
// re-read it and repeated identical searches. The digest costs no extra
// model call.
//
// The recent window never starts with a tool result whose assistant
// tool_calls message was dropped: OpenAI-compatible APIs may reject such
// an orphaned tool message.
type DigestCompactor struct {
	PinHead   int // leading messages always kept; default 2 (system + task)
	MaxRecent int // trailing messages kept verbatim; default 24
}

// Compact implements Compactor.
func (c *DigestCompactor) Compact(_ context.Context, messages []Message, _ int) ([]Message, error) {
	pin, recentN := c.PinHead, c.MaxRecent
	if pin <= 0 {
		pin = 2
	}
	if recentN <= 0 {
		recentN = 24
	}
	if len(messages) <= pin+recentN+1 {
		return messages, nil
	}
	head := messages[:pin]
	start := len(messages) - recentN
	for start < len(messages) && messages[start].Role == RoleTool {
		start++
	}
	dropped := messages[pin:start]
	entries := digestEntries(dropped)
	out := make([]Message, 0, pin+1+len(messages)-start)
	out = append(out, head...)
	if len(entries) > 0 || len(dropped) > 0 {
		out = append(out, Message{
			Role:     RoleUser,
			Content:  renderDigest(entries, len(dropped)),
			Metadata: map[string]string{DigestMetadataKey: "1"},
		})
	}
	out = append(out, messages[start:]...)
	return out, nil
}

// digestEntries collects "- name args" lines from dropped tool calls,
// carrying forward any earlier digest, deduplicated and bounded.
func digestEntries(dropped []Message) []string {
	var entries []string
	seen := map[string]bool{}
	add := func(e string) {
		if e == "" || seen[e] {
			return
		}
		seen[e] = true
		entries = append(entries, e)
	}
	for _, m := range dropped {
		if m.Metadata[DigestMetadataKey] != "" {
			for _, line := range strings.Split(m.Content, "\n") {
				if strings.HasPrefix(line, "- ") {
					add(line)
				}
			}
			continue
		}
		for _, call := range m.ToolCalls {
			args := strings.Join(strings.Fields(call.Arguments), " ")
			if len(args) > 160 {
				args = args[:160] + "…"
			}
			add(fmt.Sprintf("- %s %s", call.Name, args))
		}
	}
	if len(entries) > maxDigestEntries {
		entries = entries[len(entries)-maxDigestEntries:]
	}
	return entries
}

func renderDigest(entries []string, dropped int) string {
	var b strings.Builder
	b.WriteString(digestHeader)
	b.WriteByte('\n')
	if len(entries) == 0 {
		fmt.Fprintf(&b, "(none; %d conversational messages dropped)\n", dropped)
	}
	for _, e := range entries {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	return b.String()
}

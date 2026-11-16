package llmrouter

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/turn"
)

// maxWireToolName is the OpenAI function-name length limit.
const maxWireToolName = 64

// toolNames maps Hive tool names (hive-tool/v1 uses dotted names such as
// repo.read_file) to the function-name alphabet OpenAI-compatible APIs accept
// (^[a-zA-Z0-9_-]{1,64}$) and back. Measured on Workers AI 2026-11-16: with a
// dotted name, llama-4-scout silently answered in prose (finish_reason=stop,
// no tool call) while the same tool named with underscores was called;
// strict OpenAI-spec servers reject the request outright.
type toolNames struct {
	toWire map[string]string
	toHive map[string]string
}

// newToolNames builds the mapping from the offered tools and from every
// tool name already in the conversation: a model offered a narrower set
// (e.g. only task.complete in a finish phase) still emits names it saw
// earlier, and those must map back to the Hive name so toolapprove and the
// Runner's offered-tool check see what was actually requested.
func newToolNames(tools []toolapprove.ToolRequest, history []turn.Message) (toolNames, error) {
	n := toolNames{toWire: map[string]string{}, toHive: map[string]string{}}
	add := func(name string) error {
		w := wireToolName(name)
		if prev, dup := n.toHive[w]; dup && prev != name {
			return fmt.Errorf("llmrouter: tools %q and %q map to the same wire name %q", prev, name, w)
		}
		n.toWire[name] = w
		n.toHive[w] = name
		return nil
	}
	for _, t := range tools {
		if err := add(t.Tool); err != nil {
			return toolNames{}, err
		}
	}
	for _, m := range history {
		for _, c := range m.ToolCalls {
			if c.Name != "" && c.Name != wireToolName(c.Name) {
				if err := add(c.Name); err != nil {
					return toolNames{}, err
				}
			}
		}
		if m.Role == turn.RoleTool && m.Name != "" && m.Name != wireToolName(m.Name) {
			if err := add(m.Name); err != nil {
				return toolNames{}, err
			}
		}
	}
	return n, nil
}

// wire returns the provider-safe name for a Hive tool name. Names absent from
// the tool list (history from an earlier turn) are sanitised one-way.
func (n toolNames) wire(hive string) string {
	if w, ok := n.toWire[hive]; ok {
		return w
	}
	return wireToolName(hive)
}

// hive maps a provider-returned name back to the Hive tool name; an unknown
// name is returned unchanged so toolapprove sees (and denies) exactly what the
// model asked for.
func (n toolNames) hive(wire string) string {
	if h, ok := n.toHive[wire]; ok {
		return h
	}
	return wire
}

func wireToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '.':
			b.WriteString("__")
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > maxWireToolName {
		out = out[:maxWireToolName]
	}
	return out
}

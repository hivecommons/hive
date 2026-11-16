package hivetool

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/turn"
)

// fencedRe matches ```hive-tool ... ``` blocks (RFC #18464 §3).
var fencedRe = regexp.MustCompile("(?s)```hive-tool[ \\t]*\\r?\\n(.*?)\\r?\\n?```")

// MaxFencedCalls bounds how many calls one message may carry.
const MaxFencedCalls = 8

type fencedCall struct {
	Protocol string          `json:"protocol"`
	KickID   string          `json:"kick_id"`
	CallID   string          `json:"call_id"`
	Seq      int             `json:"seq"`
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args"`
}

// ParseFenced extracts hive-tool/v1 calls from a text reply: the transport
// for chat-only bridges, and the fallback when a native-tool model writes
// its call as prose instead of emitting it (seen live with llama-4-scout).
// A block that is not valid hive-tool/v1 is reported in errs and skipped;
// valid blocks are still returned. call_id is traceability only: execution
// idempotency comes from the turn journal, never from model-minted ids.
func ParseFenced(content string) (calls []turn.ToolCall, errs []error) {
	for i, m := range fencedRe.FindAllStringSubmatch(content, -1) {
		if len(calls) == MaxFencedCalls {
			errs = append(errs, fmt.Errorf("more than %d hive-tool blocks; the rest were ignored", MaxFencedCalls))
			break
		}
		var fc fencedCall
		dec := json.NewDecoder(strings.NewReader(m[1]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&fc); err != nil {
			errs = append(errs, fmt.Errorf("block %d: not hive-tool/v1 JSON: %v", i+1, err))
			continue
		}
		if fc.Protocol != Protocol {
			errs = append(errs, fmt.Errorf("block %d: protocol %q, want %q", i+1, fc.Protocol, Protocol))
			continue
		}
		if strings.TrimSpace(fc.Tool) == "" {
			errs = append(errs, fmt.Errorf("block %d: tool is required", i+1))
			continue
		}
		args := "{}"
		if len(fc.Args) > 0 && string(fc.Args) != "null" {
			var obj map[string]any
			if err := json.Unmarshal(fc.Args, &obj); err != nil {
				errs = append(errs, fmt.Errorf("block %d: args must be a JSON object", i+1))
				continue
			}
			args = string(fc.Args)
		}
		id := fc.CallID
		if id == "" {
			id = fmt.Sprintf("fenced_%d", i)
		}
		calls = append(calls, turn.ToolCall{ID: id, Name: fc.Tool, Arguments: args})
	}
	return calls, errs
}

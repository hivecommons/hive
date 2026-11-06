package adminmcp

import (
	"encoding/json"
	"sort"
	"strings"
)

// maxShrinkRounds bounds how many times the largest list is halved before
// falling back to a text preview.
const maxShrinkRounds = 64

// EncodeResult scrubs data, wraps it in the data envelope and encodes it within
// MaxTextBytes. An oversized result is truncated with the disclosure
// `_admin_mcp.truncated=true` rather than failing: top-level lists are shortened
// first, and a result that still does not fit is replaced by a bounded preview
// of its JSON text. Both transports use it, so the cap is identical on each.
func EncodeResult(data any) ([]byte, error) {
	scrubbed := Scrub(data)
	b, err := json.Marshal(DataEnvelope{Data: scrubbed})
	if err != nil || len(b) <= MaxTextBytes {
		return b, err
	}
	originalBytes := len(b)
	if fitted, ok := shrinkLists(scrubbed, originalBytes); ok {
		return fitted, nil
	}
	return previewResult(b, originalBytes)
}

func textCapMeta(originalBytes int) map[string]any {
	return map[string]any{
		"truncated":      true,
		"reason":         "result exceeded the admin MCP text cap",
		"text_cap_bytes": MaxTextBytes,
		"original_bytes": originalBytes,
	}
}

// shrinkLists halves the largest top-level list until the envelope fits. Lists
// are ranked by their encoded size, not their length: halving a long list of
// small entries frees almost nothing, so ranking by length cut such lists
// (and the answers in them) for no gain before it reached the list that was
// actually over the cap.
func shrinkLists(data any, originalBytes int) ([]byte, bool) {
	var out map[string]any
	switch x := data.(type) {
	case []any:
		out = map[string]any{"items": x}
	case map[string]any:
		out = make(map[string]any, len(x)+1)
		for k, v := range x {
			out[k] = v
		}
	default:
		return nil, false
	}
	prior, _ := out["_admin_mcp"].(map[string]any)
	totals := map[string]int{}
	sizes := listSizes(out)
	for round := 0; round < maxShrinkRounds; round++ {
		key := largestList(sizes)
		if key == "" {
			return nil, false
		}
		items := out[key].([]any)
		if _, seen := totals[key]; !seen {
			totals[key] = len(items)
		}
		kept := items[:len(items)/2]
		out[key] = kept
		if len(kept) == 0 {
			delete(sizes, key)
		} else {
			sizes[key] = encodedSize(kept)
		}
		out[key+"_truncated"] = true
		meta := textCapMeta(originalBytes)
		for k, v := range prior {
			if _, set := meta[k]; !set {
				meta[k] = v
			}
		}
		meta["text_cap_truncated_fields"] = sortedKeys(totals)
		meta["original_lengths"] = totals
		out["_admin_mcp"] = meta
		b, err := json.Marshal(DataEnvelope{Data: out})
		if err == nil && len(b) <= MaxTextBytes {
			return b, true
		}
	}
	return nil, false
}

// listSizes returns the encoded size of every non-empty top-level list.
func listSizes(m map[string]any) map[string]int {
	sizes := map[string]int{}
	for k, v := range m {
		if items, ok := v.([]any); ok && len(items) > 0 {
			sizes[k] = encodedSize(items)
		}
	}
	return sizes
}

func encodedSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// largestList names the list with the most encoded bytes. Equal sizes go to
// the first name in sorted order, so the same result is cut the same way on
// every call.
func largestList(sizes map[string]int) string {
	best, bestSize := "", 0
	for k, size := range sizes {
		if size > bestSize || (size == bestSize && best != "" && k < best) {
			best, bestSize = k, size
		}
	}
	return best
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// previewResult replaces the result with a prefix of its JSON text. JSON string
// escaping can grow the prefix, so the budget is halved until it fits.
func previewResult(full []byte, originalBytes int) ([]byte, error) {
	for budget := MaxTextBytes / 2; budget > 0; budget /= 2 {
		prefix := strings.ToValidUTF8(string(full[:budget]), "")
		b, err := json.Marshal(DataEnvelope{Data: map[string]any{"_admin_mcp": textCapMeta(originalBytes), "text_preview": prefix}})
		if err != nil {
			return nil, err
		}
		if len(b) <= MaxTextBytes {
			return b, nil
		}
	}
	return json.Marshal(DataEnvelope{Data: map[string]any{"_admin_mcp": textCapMeta(originalBytes)}})
}
